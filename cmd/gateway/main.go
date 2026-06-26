package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

	initialData := append([]byte(nil), buf[:n]...)
	handshake := protocol.ParseHandshake(initialData)
	if handshake.ServerHost == "" {
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Msg("failed to parse mc host from buffer")
		return nil
	}

	host, ok := lookupRoute(handshake.ServerHost)
	if host == "" {
		gatewayMetrics.RouteMiss()
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Str("host", handshake.ServerHost).
			Msg("failed to route host")
		return nil
	}
	if ok {
		gatewayMetrics.RouteHit(handshake.ServerHost)
	}

	log.Debug().
		Str("client", conn.RemoteAddr().String()).
		Str("host", handshake.ServerHost).
		Str("mc", host).
		Msg("map to host")

	var client net.Conn

	if pluginsManager != nil {
		req := newUpstreamConnectRequest(conn, host, handshake, initialData, ok)
		result, err := pluginsManager.ConnectUpstream(context.Background(), req)
		if err != nil {
			if errors.Is(err, api.ErrBlocked) {
				log.Info().
					Str("client", conn.RemoteAddr().String()).
					Str("host", handshake.ServerHost).
					Msg("managed upstream plugin blocked connection")
				return nil
			}
			log.Err(err).
				Str("client", conn.RemoteAddr().String()).
				Str("host", handshake.ServerHost).
				Str("mc", host).
				Msg("failed to invoke managed upstream plugin")
			return nil
		}
		if result.Handled {
			if result.Proxied {
				return nil
			}
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

	if err := writeAll(client, initialData); err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Str("host", handshake.ServerHost).
			Str("mc", host).
			Msg("failed to write initial packet to upstream")
		client.Close()
		return nil
	}

	return client
}

func newUpstreamConnectRequest(conn net.Conn, upstream string, handshake protocol.Handshake, initialData []byte, routeHit bool) api.UpstreamConnectRequest {
	target := upstreamtarget.Parse(upstream)
	transport, serviceName, listenerPort := connectionIngress(conn)
	req := api.UpstreamConnectRequest{
		Source:           conn,
		Host:             handshake.ServerHost,
		Upstream:         upstream,
		InitialData:      append([]byte(nil), initialData...),
		Metadata:         map[string]string{"route_hit": boolString(routeHit)},
		ConnectionID:     randomHexID(8),
		TraceID:          randomHexID(16),
		SourceAddr:       conn.RemoteAddr().String(),
		ServerHost:       handshake.ServerHost,
		RawServerHost:    handshake.RawServerHost,
		ProtocolVersion:  handshake.ProtocolVersion,
		NextState:        handshake.NextState,
		RouteID:          handshake.ServerHost,
		RouteTags:        []string{},
		UpstreamRaw:      upstream,
		UpstreamProtocol: string(target.Protocol),
		UpstreamAddress:  target.Address,
		Transport:        transport,
		ServiceName:      serviceName,
		ListenerPort:     listenerPort,
	}
	return req
}

func connectionIngress(conn net.Conn) (transport string, serviceName string, listenerPort int) {
	transport = "tcp"
	serviceName = serviceNameTCPAdmin
	switch conn.(type) {
	case *webSocketConn:
		transport = "websocket"
		serviceName = serviceNameWebSocket
	case quicConn:
		transport = "quic"
		serviceName = serviceNameQUIC
	}
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		listenerPort = addr.Port
	}
	return transport, serviceName, listenerPort
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func randomHexID(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte("fallback"))
	}
	return hex.EncodeToString(buf)
}
