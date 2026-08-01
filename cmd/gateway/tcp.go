// cmd/gateway/tcp.go 在未与 Admin HTTP 共用端口时启动普通 TCP Minecraft 监听器。

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/rs/zerolog/log"
)

func runTcp(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	port := normalizedTCPPort()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on TCP port %d: %w", port, err)
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	log.Info().
		Int("port", port).
		Msg("Listening for TCP connections")

	for {
		// 接受传入的连接
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			log.Err(err).Msg("Error accepting")
			continue
		}
		setSocketOptions(conn)
		// 处理连接；后续握手解析、插件过滤和路由解析都在 handleRequest 中完成。
		gatewayMetrics.TCPConnectionStarted()
		go handleRequest(conn)
	}
}

func upstreamTcp(host string) net.Conn {
	// TCP 是默认上游传输，路由值没有协议前缀时都会走这里。
	conn, err := tcpDialer.Dial("tcp", host)
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Str("host", host).Msg("Error dialing upstream")
		return nil
	}
	setSocketOptions(conn)
	return conn

}

var tcpDialer = net.Dialer{
	// 上游拨号失败应尽快返回给客户端连接处理流程，避免连接协程长期堆积。
	Timeout:   3 * time.Second,
	KeepAlive: 30 * time.Second,
}

func setSocketOptions(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true) // 禁用 Nagle 算法，降低 Minecraft 交互延迟。
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}
