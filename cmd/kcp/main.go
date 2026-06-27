// cmd/kcp/main.go 提供独立的 KCP 到 TCP 代理工具，用于测试或演示 KCP 传输行为。

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/protocol"
	"github.com/xtaci/kcp-go"
)

var (
	localPort = 25565

	mcHost = "bh.mc.tursom.cn"
	mcPort = 25566
)

func main() {
	if len(os.Args) > 1 {
		mcHost = os.Args[1]
	}

	// 监听TCP端口
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		log.Fatal().Err(err).
			Int("port", localPort).
			Msg("Failed to listen on port")
	}
	defer listener.Close()
	log.Info().
		Int("port", localPort).
		Msg("Listening for TCP connections")

	for {
		// 接受传入的连接
		conn, err := listener.Accept()
		if err != nil {
			log.Err(err).Msg("Error accepting")
			continue
		}
		log.Info().
			Str("client", conn.RemoteAddr().String()).
			Int("port", localPort).
			Msg("Accepted connection")
		// 处理连接
		go handlerConn(conn)
	}
}

func handlerConn(client net.Conn) {
	defer client.Close()
	setSocketOptions(client)

	conn, err := kcp.DialWithOptions(fmt.Sprintf("%s:%d", mcHost, mcPort), nil, 10, 5)
	if err != nil {
		log.Error().Err(err).
			Msg("Failed to dial KCP server")
	}
	defer conn.Close()

	conn.SetACKNoDelay(true)

	buf := make([]byte, 1024)
	n, err := client.Read(buf)
	if err != nil {
		log.Err(err).
			Str("client", client.RemoteAddr().String()).
			Msg("Error reading hostname")
		return
	}
	if n == 0 {
		log.Err(errors.New("empty buffer")).
			Str("client", client.RemoteAddr().String()).
			Msg("Error: buffer is empty")
		return
	}
	buf = buf[:n]

	buf = protocol.ReplaceMcHost(buf, mcHost)
	_, err = conn.Write(buf) // 写入数据到 QUIC 流
	if err != nil {
		log.Err(err).
			Str("client", client.RemoteAddr().String()).
			Str("host", mcHost).
			Int("port", mcPort).
			Msg("Error writing to QUIC stream")
		return
	}
	// 释放 buf
	buf = nil

	var wg sync.WaitGroup
	wg.Add(1)
	go copyData(conn, client, &wg)
	copyData(client, conn, nil)
	wg.Wait()
}

func copyData(src io.Reader, dst io.Writer, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	_, err := io.Copy(dst, src)
	if err != nil && err != io.EOF {
		log.Err(err).Msg("Error copying data")
	}
}

func setSocketOptions(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true) // 禁用 Nagle 算法
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}
