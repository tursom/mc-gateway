// cmd/gateway/kcp.go 启动可选的 KCP 监听器，并把接收到的会话转入统一网关请求处理流程。

package main

import (
	"context"
	"fmt"
	"net"

	"github.com/rs/zerolog/log"
	"github.com/xtaci/kcp-go"
)

type kcpIngressConn struct {
	net.Conn
}

func (kcpIngressConn) IngressTransport() (string, string) {
	return "kcp", serviceNameKCP
}

func runKcp(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	// KCP 监听使用运行态服务配置中的分片参数，和上游拨号保持一致。
	listener, err := kcp.ListenWithOptions(fmt.Sprintf(":%d", config.Kcp.Port), nil, config.Kcp.DataShards, config.Kcp.ParityShards)
	if err != nil {
		return fmt.Errorf("listen on KCP port %d: %w", config.Kcp.Port, err)
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()

	log.Info().Int("port", config.Kcp.Port).Msg("KCP server is listening")

	for {
		conn, err := listener.AcceptKCP()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Err(err).
				Msg("Failed to accept KCP connection")
			continue
		}
		log.Debug().
			Str("remote_addr", conn.RemoteAddr().String()).
			Msg("Accepted KCP connection")

		tuneKcpConn(conn)

		// KCP session 实现 net.Conn，可以直接进入统一网关请求流程。
		go handleRequest(kcpIngressConn{Conn: conn})
	}
}

func upstreamKcp(host string) net.Conn {
	// KCP 上游使用与入口相同的 data/parity shards，确保两端编码参数匹配。
	conn, err := kcp.DialWithOptions(host, nil, config.Kcp.DataShards, config.Kcp.ParityShards)
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Error().Err(err).
			Msg("Failed to dial KCP server")
		return nil
	}

	tuneKcpConn(conn)
	return conn
}

func tuneKcpConn(conn *kcp.UDPSession) {
	// 这里偏向低延迟交互：stream mode 模拟 TCP 字节流，禁用写延迟并打开快速 ACK。
	conn.SetStreamMode(true)
	conn.SetWriteDelay(false)
	conn.SetNoDelay(1, 10, 2, 1)
	conn.SetWindowSize(256, 256)
	conn.SetACKNoDelay(true)
}
