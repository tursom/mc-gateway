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
	"github.com/tursom/mc-gateway/internal/pluginmanager"
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
	if pluginsManager != nil {
		transport, _, _ := connectionIngress(conn)
		filter, err := pluginsManager.FilterConnection(context.Background(), api.ConnectionFilterRequest{
			SourceAddr: conn.RemoteAddr().String(),
			Transport:  transport,
		})
		if err != nil {
			log.Err(err).Str("client", conn.RemoteAddr().String()).Msg("connection filter failed")
			return nil
		}
		if !filter.Allowed {
			log.Info().Str("client", conn.RemoteAddr().String()).Str("plugin", filter.PluginID).Str("reason", filter.Reason).Msg("connection rejected by filter")
			return nil
		}
	}

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

	if pluginsManager != nil {
		filter, err := pluginsManager.FilterHandshake(context.Background(), api.HandshakeFilterRequest{
			SourceAddr:      conn.RemoteAddr().String(),
			ServerHost:      handshake.ServerHost,
			RawServerHost:   handshake.RawServerHost,
			ProtocolVersion: handshake.ProtocolVersion,
			NextState:       handshake.NextState,
		})
		if err != nil {
			log.Err(err).Str("client", conn.RemoteAddr().String()).Str("host", handshake.ServerHost).Msg("handshake filter failed")
			return nil
		}
		if !filter.Allowed {
			log.Info().Str("client", conn.RemoteAddr().String()).Str("host", handshake.ServerHost).Str("plugin", filter.PluginID).Str("reason", filter.Reason).Msg("handshake rejected by filter")
			return nil
		}
		if filter.RewriteHost != "" && filter.RewriteHost != handshake.ServerHost {
			initialData = protocol.ReplaceMcHost(initialData, filter.RewriteHost)
			handshake = protocol.ParseHandshake(initialData)
		}
	}

	if handshake.NextState == 1 {
		if handled := handleStatusPing(conn, handshake); handled {
			return nil
		}
	}

	routeResult := resolveGatewayRoute(conn, handshake)
	host := routeResult.Decision.Upstream
	ok := routeResult.Source != "fallback_miss"
	if routeResult.Decision.Action == api.RouteDecisionReject || host == "" {
		gatewayMetrics.RouteMiss()
		log.Err(errEmptyBuffer).
			Str("client", conn.RemoteAddr().String()).
			Str("host", handshake.ServerHost).
			Str("route_source", routeResult.Source).
			Str("route_action", routeResult.Decision.Action).
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

func resolveGatewayRoute(conn net.Conn, handshake protocol.Handshake) pluginmanager.RouteResolveResult {
	upstream, hit := lookupRoute(handshake.ServerHost)
	req := api.RouteResolveRequest{
		Host:             handshake.ServerHost,
		RawServerHost:    handshake.RawServerHost,
		SourceAddr:       conn.RemoteAddr().String(),
		ProtocolVersion:  handshake.ProtocolVersion,
		NextState:        handshake.NextState,
		FallbackUpstream: upstream,
		FallbackHit:      hit,
		Handshake: api.UpstreamHandshakeRef{
			ServerHost:      handshake.ServerHost,
			RawServerHost:   handshake.RawServerHost,
			ProtocolVersion: handshake.ProtocolVersion,
			NextState:       handshake.NextState,
		},
	}
	if pluginsManager != nil {
		result, err := pluginsManager.ResolveRoute(context.Background(), req, func(req api.RouteResolveRequest) (string, bool) {
			return lookupRoute(req.Host)
		})
		if err == nil {
			return result
		}
		log.Err(err).Str("client", conn.RemoteAddr().String()).Str("host", handshake.ServerHost).Msg("route resolver failed")
	}
	action := api.RouteDecisionFallback
	source := "sqlite_fallback"
	if upstream == "" {
		action = api.RouteDecisionReject
		source = "fallback_miss"
	}
	return pluginmanager.RouteResolveResult{
		Decision: api.RouteDecision{Action: action, Upstream: upstream, ProviderID: "sqlite", Reason: "sqlite route snapshot fallback"},
		Source:   source,
	}
}

func handleStatusPing(conn net.Conn, handshake protocol.Handshake) bool {
	if pluginsManager == nil {
		return false
	}
	result, err := pluginsManager.StatusPing(context.Background(), api.StatusPingRequest{
		Host:            handshake.ServerHost,
		RawServerHost:   handshake.RawServerHost,
		SourceAddr:      conn.RemoteAddr().String(),
		ProtocolVersion: handshake.ProtocolVersion,
	})
	if err != nil || !result.Handled {
		if err != nil {
			log.Err(err).Str("host", handshake.ServerHost).Msg("status ping plugin failed")
		}
		return false
	}
	payload := map[string]any{
		"description": map[string]any{"text": result.Response.MOTD},
		"players": map[string]any{
			"online": result.Response.OnlinePlayers,
			"max":    result.Response.MaxPlayers,
		},
		"version": map[string]any{
			"name":     result.Response.VersionText,
			"protocol": result.Response.ProtocolVersion,
		},
	}
	if result.Response.Favicon != "" {
		payload["favicon"] = result.Response.Favicon
	}
	if result.Response.Maintenance {
		payload["maintenance"] = map[string]any{"window": result.Response.MaintenanceWindow}
	}
	packet, err := protocol.StatusResponsePacket(payload)
	if err != nil {
		log.Err(err).Str("host", handshake.ServerHost).Msg("failed to build status response")
		return true
	}
	if err := writeAll(conn, packet); err != nil {
		log.Err(err).Str("host", handshake.ServerHost).Msg("failed to write status response")
	}
	return true
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
