// cmd/gateway/main.go 负责网关进程启动、监听器选择、Minecraft 握手路由、插件钩子分发以及上游转发交接。

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/internal/upstreamtarget"
	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
	"golang.org/x/sync/errgroup"
)

const gatewayShutdownTimeout = 30 * time.Second

var pluginExitWaitGroup sync.WaitGroup

func main() {
	os.Exit(runMain())
}

func runMain() int {
	// 插件和 plugin-host 子命令复用网关二进制。这里先于运行态配置加载
	// 处理它们，这样本地构建、清单和 host 握手命令不需要一份可用的网关部署配置。
	if handled, code := runPluginHostCLI(os.Args[1:]); handled {
		return code
	}
	if handled, code := runPluginCLI(os.Args[1:]); handled {
		return code
	}

	if err := loadConfig(); err != nil {
		log.Err(err).Msg("Failed to initialize gateway runtime")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gatewayShutdownTimeout)
		defer cancel()
		_ = closeGatewayRuntime(shutdownCtx)
		return 1
	}

	if err := writePIDFile(); err != nil {
		log.Err(err).Msg("Failed to write PID file")
	}
	defer removePIDFile()

	go handleLogRotate()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serviceCtx, cancelServices := context.WithCancel(context.Background())
	serviceErrCh := make(chan error, 1)
	go func() { serviceErrCh <- runEnabledServices(serviceCtx) }()

	var serviceErr error
	servicesDone := false
	select {
	case serviceErr = <-serviceErrCh:
		servicesDone = true
		if serviceErr == nil {
			serviceErr = errors.New("gateway services stopped unexpectedly")
		}
	case <-signalCtx.Done():
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), gatewayShutdownTimeout)
	cancelServices()
	if !servicesDone {
		select {
		case serviceErr = <-serviceErrCh:
		case <-shutdownCtx.Done():
			serviceErr = fmt.Errorf("stop gateway services: %w", shutdownCtx.Err())
		}
	}
	closeErr := closeGatewayRuntime(shutdownCtx)
	cancelShutdown()
	if serviceErr != nil {
		log.Err(serviceErr).Msg("Gateway service stopped unexpectedly")
	}
	if closeErr != nil {
		log.Err(closeErr).Msg("Gateway runtime shutdown failed")
	}
	if serviceErr != nil || closeErr != nil {
		return 1
	}
	return 0
}

func runEnabledServices(ctx context.Context) error {
	group, serviceCtx := errgroup.WithContext(ctx)
	startService := func(run func(context.Context) error) {
		group.Go(func() error { return run(serviceCtx) })
	}
	// TCP 和 Admin HTTP 始终通过共享监听器启动。共享监听器按每条连接
	// 的首包判断它是 HTTP 还是 Minecraft 协议数据，因此不需要额外维护
	// 一个手动模式开关。
	startService(runTcpWebPortReuse)

	// 可选传输最终仍进入 handleRequest，这让插件过滤、路由解析和上游拨号
	// 在 TCP、KCP、QUIC 和 WebSocket 入口之间保持一致。
	if config.Kcp.Enable {
		startService(runKcp)
	}
	if config.Quic.Enable {
		startService(runQuic)
	}
	if config.WebSocket.Enable && normalizedWebSocketPort() != normalizedTCPPort() {
		startService(runWebSocket)
	}
	return group.Wait()
}

func handleRequest(conn net.Conn) {
	gatewayMetrics.ConnectionStarted()
	defer gatewayMetrics.ConnectionFinished()
	defer conn.Close()

	peerAddr := conn.RemoteAddr().String()
	localAddr := conn.LocalAddr().String()
	ingress := connectionIngressContext(conn)
	req := api.UpstreamConnectRequestV2{
		Context:      context.Background(),
		ConnectionID: randomHexID(8),
		TraceID:      randomHexID(16),
		PeerAddr:     peerAddr,
		LocalAddr:    localAddr,
		Connection: api.ConnectionState{
			Stream:              conn,
			EffectiveSourceAddr: peerAddr,
			Metadata:            map[string]string{},
		},
		Ingress: ingress,
	}

	// 插件或协议解析器的 panic 不能杀掉监听协程；当前连接会被放弃，
	// 进程继续服务其他客户端。
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

	core := func(ctx context.Context, state api.ConnectionState) error {
		return runCorePipeline(ctx, req, state)
	}
	if pluginsManager != nil {
		if err := pluginsManager.HandleConnection(req.Context, req, core); err != nil {
			log.Err(err).Str("client", peerAddr).Msg("connection takeover failed")
		}
		return
	}
	if err := core(req.Context, req.Connection); err != nil {
		log.Err(err).Str("client", peerAddr).Msg("connection core failed")
	}
}

func runCorePipeline(ctx context.Context, ingressReq api.UpstreamConnectRequestV2, state api.ConnectionState) error {
	client := mapToHost(ctx, state.Stream, state.EffectiveSourceAddr, ingressReq.PeerAddr, ingressReq.Ingress)
	if client == nil {
		return nil
	}
	defer client.Close()
	proxyConnections(state.Stream, client)
	return nil
}

func mapToHost(ctx context.Context, conn net.Conn, effectiveSourceAddr, peerAddr string, ingress api.IngressContext) net.Conn {
	// 连接过滤器在读取 Minecraft 握手前执行，因此可以按来源地址或传输类型
	// 拒绝连接，同时不消耗客户端发送的协议字节。
	if pluginsManager != nil {
		filter, err := pluginsManager.FilterConnection(ctx, api.ConnectionFilterRequest{
			SourceAddr: effectiveSourceAddr,
			Transport:  ingress.Transport,
		})
		if err != nil {
			log.Err(err).Str("client", peerAddr).Msg("connection filter failed")
			return nil
		}
		if !filter.Allowed {
			log.Info().Str("client", peerAddr).Str("plugin", filter.PluginID).Str("reason", filter.Reason).Msg("connection rejected by filter")
			return nil
		}
	}

	buf := getProxyBuffer()
	defer putProxyBuffer(buf)

	n, err := readMinecraftPacket(conn, buf)
	if err != nil {
		log.Err(err).
			Str("client", peerAddr).
			Msg("failed to reading hostname")
		return nil
	}
	if n == 0 {
		log.Err(errEmptyBuffer).
			Str("client", peerAddr).
			Msg("buffer is empty")
		return nil
	}

	// 第一次读取包含 Minecraft 握手数据。所有过滤器和路由决策完成后，
	// 这段数据必须原样或按插件改写后回放给选中的上游。
	initialData := append([]byte(nil), buf[:n]...)
	handshake := protocol.ParseHandshake(initialData)
	if handshake.ServerHost == "" {
		log.Err(errEmptyBuffer).
			Str("client", peerAddr).
			Msg("failed to parse mc host from buffer")
		return nil
	}

	// 握手过滤器可以改写目标主机名。发生改写时要立刻重建首包，
	// 确保上游看到的是改写后的 Minecraft 主机名，而不是客户端原始值。
	if pluginsManager != nil {
		filter, err := pluginsManager.FilterHandshake(ctx, api.HandshakeFilterRequest{
			SourceAddr:      effectiveSourceAddr,
			ServerHost:      handshake.ServerHost,
			RawServerHost:   handshake.RawServerHost,
			ProtocolVersion: handshake.ProtocolVersion,
			NextState:       handshake.NextState,
		})
		if err != nil {
			log.Err(err).Str("client", peerAddr).Str("host", handshake.ServerHost).Msg("handshake filter failed")
			return nil
		}
		if !filter.Allowed {
			log.Info().Str("client", peerAddr).Str("host", handshake.ServerHost).Str("plugin", filter.PluginID).Str("reason", filter.Reason).Msg("handshake rejected by filter")
			return nil
		}
		if filter.RewriteHost != "" && filter.RewriteHost != handshake.ServerHost {
			initialData = protocol.ReplaceMcHost(initialData, filter.RewriteHost)
			handshake = protocol.ParseHandshake(initialData)
		}
	}

	// 状态查询使用 NextState=1，并且可以由插件直接完整响应。
	// 如果这里已经处理，就不会再为该查询打开上游连接。
	if handshake.NextState == 1 {
		if handled := handleStatusPing(ctx, conn, effectiveSourceAddr, handshake); handled {
			return nil
		}
	}

	routeResult := resolveGatewayRoute(ctx, effectiveSourceAddr, peerAddr, handshake)
	host := routeResult.Decision.Upstream
	ok := routeResult.Source != "fallback_miss"
	if routeResult.Decision.Action == api.RouteDecisionReject || host == "" {
		gatewayMetrics.RouteMiss()
		log.Err(errEmptyBuffer).
			Str("client", peerAddr).
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
		Str("client", peerAddr).
		Str("host", handshake.ServerHost).
		Str("mc", host).
		Msg("map to host")

	var client net.Conn
	if client == nil {
		target := upstreamtarget.Parse(host)
		// 路由值可以通过前缀选择非 TCP 传输；普通地址仍按 TCP 处理，
		// 以保持旧配置的行为不变。
		switch target.Protocol {
		case upstreamtarget.ProtocolQUIC:
			client = upstreamQuic(target.Address)
		case upstreamtarget.ProtocolKCP:
			client = upstreamKcp(target.Address)
		case upstreamtarget.ProtocolHAProxy:
			client = haProxyUpstream(effectiveSourceAddr, target.Address)
		default:
			client = upstreamTcp(target.Address)
		}
	}
	if client == nil {
		return nil
	}

	// 只有在上游路径确定后才回放握手数据。这样插件在任何上游字节发出前，
	// 都还有机会阻断、代理或改写连接。
	if err := writeAll(client, initialData); err != nil {
		log.Err(err).
			Str("client", peerAddr).
			Str("host", handshake.ServerHost).
			Str("mc", host).
			Msg("failed to write initial packet to upstream")
		client.Close()
		return nil
	}

	return client
}

func readMinecraftPacket(reader io.Reader, buf []byte) (int, error) {
	length := 0
	prefixLength := 0
	for ; prefixLength < 5; prefixLength++ {
		if prefixLength >= len(buf) {
			return 0, io.ErrShortBuffer
		}
		if _, err := io.ReadFull(reader, buf[prefixLength:prefixLength+1]); err != nil {
			return 0, err
		}
		current := buf[prefixLength]
		length |= int(current&0x7f) << (7 * prefixLength)
		if current&0x80 == 0 {
			prefixLength++
			break
		}
	}
	if prefixLength == 5 && buf[prefixLength-1]&0x80 != 0 {
		return 0, errors.New("minecraft packet length VarInt is too long")
	}
	if length <= 0 {
		return 0, errors.New("minecraft packet length must be positive")
	}
	if length > len(buf)-prefixLength {
		return 0, fmt.Errorf("minecraft packet length %d exceeds ingress buffer", length)
	}
	if _, err := io.ReadFull(reader, buf[prefixLength:prefixLength+length]); err != nil {
		return 0, err
	}
	return prefixLength + length, nil
}

func resolveGatewayRoute(ctx context.Context, effectiveSourceAddr, peerAddr string, handshake protocol.Handshake) pluginmanager.RouteResolveResult {
	upstream, hit := lookupRoute(handshake.ServerHost)
	// SQLite 快照始终作为本地兜底。插件会同时拿到兜底决策和刷新回调，
	// 因此可以选择性覆盖路由，而不必在插件里复制一套路由仓库逻辑。
	req := api.RouteResolveRequest{
		Host:             handshake.ServerHost,
		RawServerHost:    handshake.RawServerHost,
		SourceAddr:       effectiveSourceAddr,
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
		result, err := pluginsManager.ResolveRoute(ctx, req, func(req api.RouteResolveRequest) (string, bool) {
			return lookupRoute(req.Host)
		})
		if err == nil {
			return result
		}
		log.Err(err).Str("client", peerAddr).Str("host", handshake.ServerHost).Msg("route resolver failed")
	}
	action := api.RouteDecisionFallback
	source := "sqlite_fallback"
	if upstream == "" {
		// 没有命中兜底路由时统一表示为拒绝决策，便于热路径记录一致的失败形态。
		action = api.RouteDecisionReject
		source = "fallback_miss"
	}
	return pluginmanager.RouteResolveResult{
		Decision: api.RouteDecision{Action: action, Upstream: upstream, ProviderID: "sqlite", Reason: "sqlite route snapshot fallback"},
		Source:   source,
	}
}

func handleStatusPing(ctx context.Context, conn net.Conn, effectiveSourceAddr string, handshake protocol.Handshake) bool {
	if pluginsManager == nil {
		return false
	}
	// Minecraft 状态响应是带长度前缀的 JSON 数据包。插件只提供高层字段，
	// Minecraft 协议封包由 protocol.StatusResponsePacket 统一完成。
	result, err := pluginsManager.StatusPing(ctx, api.StatusPingRequest{
		Host:            handshake.ServerHost,
		RawServerHost:   handshake.RawServerHost,
		SourceAddr:      effectiveSourceAddr,
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

func connectionIngressContext(conn net.Conn) api.IngressContext {
	ingress := api.IngressContext{Transport: "tcp", ServiceName: serviceNameTCPAdmin}
	// 具体连接包装类型记录了客户端来自哪个监听器。该元数据会传给插件，
	// 并出现在运维诊断中，同时不需要改变 net.Conn 接口。
	if tagged, ok := conn.(interface{ IngressTransport() (string, string) }); ok {
		ingress.Transport, ingress.ServiceName = tagged.IngressTransport()
	}
	if quicIngress, ok := conn.(interface {
		QUICIngressContext() *api.QUICIngressContext
	}); ok {
		ingress.QUIC = quicIngress.QUICIngressContext()
	}
	switch addr := conn.LocalAddr().(type) {
	case *net.TCPAddr:
		ingress.ListenerPort = addr.Port
	case *net.UDPAddr:
		ingress.ListenerPort = addr.Port
	}
	if httpIngress, ok := conn.(interface {
		HTTPIngressContext() *api.HTTPIngressContext
	}); ok {
		ingress.HTTP = httpIngress.HTTPIngressContext()
		if _, tagged := conn.(interface{ IngressTransport() (string, string) }); !tagged {
			ingress.Transport = "websocket"
			ingress.ServiceName = serviceNameWebSocket
		}
	}
	return ingress
}

func randomHexID(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte("fallback"))
	}
	return hex.EncodeToString(buf)
}
