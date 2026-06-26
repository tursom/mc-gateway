package main

import (
	"context"
	"net"
	"os"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/upstreamtarget"
	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
)

func main() {
	if handled, code := runPluginCLI(os.Args[1:]); handled {
		os.Exit(code)
	}

	if err := loadConfig(); err != nil {
		panic(err)
	}

	if err := writePIDFile(); err != nil {
		log.Err(err).Msg("Failed to write PID file")
	}
	defer removePIDFile()
	defer closeGatewayRuntime()

	go handleLogRotate()

	defer exitWaitGroup.Wait()

	startEnabledServices()
}

func startEnabledServices() {
	startService(runTcpWebPortReuse)

	if config.Kcp.Enable {
		startService(runKcp)
	}
	if config.Quic.Enable {
		startService(runQuic)
	}
	if config.WebSocket.Enable && normalizedWebSocketPort() != normalizedTCPPort() {
		startService(runWebSocket)
	}
}

func startService(run func(wg *sync.WaitGroup)) {
	exitWaitGroup.Add(1)
	go run(&exitWaitGroup)
}

func handleRequest(conn net.Conn) {
	gatewayMetrics.ConnectionStarted()
	defer gatewayMetrics.ConnectionFinished()

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

	mcHost := protocol.GetMcHost(buf[:n])
	if mcHost == "" {
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Msg("failed to parse mc host from buffer")
		return nil
	}

	host, ok := lookupRoute(mcHost)
	if host == "" {
		gatewayMetrics.RouteMiss()
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Str("host", mcHost).
			Msg("failed to route host")
		return nil
	}
	if ok {
		gatewayMetrics.RouteHit(mcHost)
	}

	log.Debug().
		Str("client", conn.RemoteAddr().String()).
		Str("host", mcHost).
		Str("mc", host).
		Msg("map to host")

	var client net.Conn

	if pluginsManager != nil {
		result, err := pluginsManager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      conn,
			Host:        mcHost,
			Upstream:    host,
			InitialData: append([]byte(nil), buf[:n]...),
		})
		if err != nil {
			log.Err(err).
				Str("client", conn.RemoteAddr().String()).
				Str("host", mcHost).
				Str("mc", host).
				Msg("failed to invoke managed upstream plugin")
			return nil
		}
		if result.Handled {
			client = result.Conn
		}
	}

	if client == nil {
		ok, err = invokeFirstHookHandler(api.HookUpstream, Handler2[net.Conn, string, bool](conn, host), func(handler func(net.Conn, string) (net.Conn, error)) error {
			var err error
			client, err = handler(conn, host)
			return err
		})
		if err != nil {
			log.Err(err).Msg("Failed to invoke upstream hook")
			return nil
		}
	}

	if client == nil {
		target := upstreamtarget.Parse(host)
		switch target.Protocol {
		case upstreamtarget.ProtocolQUIC:
			client = upstreamQuic(target.Address)
		case upstreamtarget.ProtocolKCP:
			client = upstreamKcp(target.Address)
		case upstreamtarget.ProtocolHAProxy:
			client = haProxyUpstream(conn, target.Address)
		default:
			client = upstreamTcp(target.Address)
		}
	}
	if client == nil {
		return nil
	}

	if err := writeAll(client, buf[:n]); err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Str("host", mcHost).
			Str("mc", host).
			Msg("failed to write initial packet to upstream")
		client.Close()
		return nil
	}

	return client
}
