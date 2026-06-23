package main

import (
	"net"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
)

func main() {
	if err := loadConfig(); err != nil {
		panic(err)
	}

	if err := writePIDFile(); err != nil {
		log.Err(err).Msg("Failed to write PID file")
	}
	defer removePIDFile()

	watcher := watchConfig()
	defer watcher.Close()

	go handleLogRotate()

	defer exitWaitGroup.Wait()

	for _, service := range services {
		if !*service.enable {
			continue
		}

		exitWaitGroup.Add(1)
		go service.run(&exitWaitGroup)
	}
}

func handleRequest(conn net.Conn) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}

		if err, ok := rec.(error); ok {
			log.Err(err).
				Str("client", conn.RemoteAddr().String()).
				Msg("Panic on handle request")
		} else {
			log.Error().Any("err", rec).
				Str("client", conn.RemoteAddr().String()).
				Msg("Panic on handle request")
		}
	}()

	// 确保连接关闭
	defer conn.Close()

	client := mapToHost(conn)
	if client == nil {
		return
	}
	defer client.Close()

	proxyConnections(conn, client)
}

func mapToHost(conn net.Conn) net.Conn {
	buf := getProxyBuffer()
	defer putProxyBuffer(buf)

	n, err := conn.Read(buf)
	if err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Msg("failed to reading hostname")
		return nil
	}
	if n == 0 {
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Msg("buffer is empty")
		return nil
	}

	mc_host := protocol.GetMcHost(buf[:n])
	if mc_host == "" {
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Msg("failed to parse mc host from buffer")
		return nil
	}

	host, ok := config.Hosts[mc_host]
	if !ok {
		host = config.Hosts["default"]
	}
	if host == "" {
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Str("host", mc_host).
			Msg("failed to route host")
		return nil
	}

	log.Debug().
		Str("client", conn.RemoteAddr().String()).
		Str("host", mc_host).
		Str("mc", host).
		Msg("map to host")

	var client net.Conn

	ok, err = invokeFirstHookHandler(api.HookUpstream, Handler2[net.Conn, string, bool](conn, host), func(handler func(net.Conn, string) (net.Conn, error)) error {
		var err error
		client, err = handler(conn, host)
		return err
	})
	if err != nil {
		log.Err(err).Msg("Failed to invoke upstream hook")
		return nil
	}

	if !ok {
		if host, ok := strings.CutPrefix(host, "quic://"); ok {
			client = upstreamQuic(host)
		} else if host, ok := strings.CutPrefix(host, "kcp://"); ok {
			client = upstreamKcp(host)
		} else if host, ok := strings.CutPrefix(host, "haproxy://"); ok {
			client = haProxyUpstream(conn, host)
		} else {
			client = upstreamTcp(host)
		}
	}
	if client == nil {
		return nil
	}

	if err := writeAll(client, buf[:n]); err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Str("host", mc_host).
			Str("mc", host).
			Msg("failed to write initial packet to upstream")
		client.Close()
		return nil
	}

	return client
}
