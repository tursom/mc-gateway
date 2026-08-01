// cmd/gateway/tcp_web_port_reuse.go 启动共享 TCP/Admin 监听器，按连接首包自动区分 HTTP 流量和 Minecraft 流量。

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/tcphttpmux"
)

const (
	defaultTCPPort = 25565
	// 首包超时沿用 tcphttpmux 默认值，保持同端口分流逻辑的单一来源。
	tcpWebInitialPacketTimeout = tcphttpmux.DefaultInitialPacketTimeout
)

func normalizedTCPPort() int {
	// 静态配置未指定端口时保持 Minecraft 默认端口。
	if config.Tcp.Port == 0 {
		return defaultTCPPort
	}
	return config.Tcp.Port
}

func normalizedWebSocketPort() int {
	if config.WebSocket.Port == 0 {
		return defaultWebSocketPort
	}
	return config.WebSocket.Port
}

func normalizedWebSocketPath() string {
	// WebSocket 路径为空时回退到根路径，避免生成空的 HTTP 路由。
	if config.WebSocket.Path == "" {
		return "/"
	}
	return config.WebSocket.Path
}

func tcpWebPortReuseEnabled() bool {
	// 是否共用端口完全由启用状态和端口相等推导，不引入额外配置开关。
	return config.Tcp.Enable &&
		config.WebSocket.Enable &&
		normalizedTCPPort() == normalizedWebSocketPort()
}

func runTcpWebPortReuse(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	port := normalizedTCPPort()
	// 同一个 listener 同时承载 Minecraft TCP 和 Admin HTTP，由 serveTcpWebPortReuse 分流。
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on shared TCP/WebSocket port %d: %w", port, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()

	log.Info().
		Int("port", port).
		Str("admin_path", adminStartup.AdminPath).
		Msg("Listening for shared TCP and Admin connections")

	if err := serveTcpWebPortReuse(listener, newGatewayHTTPHandler(), handleRequest); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("serve shared TCP/Admin port %d: %w", port, err)
	}
	return nil
}

func serveTcpWebPortReuse(listener net.Listener, handler http.Handler, tcpHandler func(net.Conn)) error {
	// tcphttpmux 只负责协议分流；指标、socket 选项和日志通过回调接回主包。
	return tcphttpmux.Serve(listener, handler, tcpHandler, tcphttpmux.Options{
		InitialPacketTimeout: tcpWebInitialPacketTimeout,
		SetSocketOptions:     setSocketOptions,
		OnTCPConnection:      gatewayMetrics.TCPConnectionStarted,
		OnAcceptError: func(err error) {
			log.Err(err).Msg("Error accepting shared TCP/WebSocket connection")
		},
		OnInitialPacketError: func(conn net.Conn, err error) {
			log.Debug().Err(err).
				Str("client", conn.RemoteAddr().String()).
				Msg("failed to read initial packet")
		},
		OnEmptyInitialPacket: func(conn net.Conn) {
			log.Debug().
				Str("client", conn.RemoteAddr().String()).
				Msg("initial packet is empty")
		},
		OnHTTPDeliveryFailed: func(conn net.Conn) {
			log.Debug().
				Str("client", conn.RemoteAddr().String()).
				Msg("failed to deliver HTTP connection")
		},
	})
}
