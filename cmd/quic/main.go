// cmd/quic/main.go 提供独立的 QUIC 到 TCP 代理工具，用于测试或演示 QUIC 传输行为。

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/protocol"
)

var (
	localPort = 25565

	mcHost = "bh.mc.tursom.cn"
	mcPort = 25565
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

func handlerConn(conn net.Conn) {
	defer conn.Close()

	setSocketOptions(conn)

	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // 跳过证书检查
		NextProtos:         []string{"minecraft", "quic", "raw", "h3"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // 3s handshake timeout
	defer cancel()

	quicConn, err := quic.DialAddr(ctx, fmt.Sprintf("%s:%d", mcHost, mcPort), tlsConf, nil)
	if err != nil {
		log.Err(err).Msg("Failed to dial QUIC")
		return
	}

	stream, err := quicConn.OpenStream()
	if err != nil {
		log.Err(err).Msg("Failed to open stream")
		return
	}
	defer stream.Close()
	log.Info().
		Msg("QUIC stream opened")

	// 读写 QUIC 流数据。

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Msg("Error reading hostname")
		return
	}
	if n == 0 {
		log.Err(errors.New("empty buffer")).
			Str("client", conn.RemoteAddr().String()).
			Msg("Error: buffer is empty")
		return
	}
	buf = buf[:n]

	buf = protocol.ReplaceMcHost(buf, mcHost)
	_, err = stream.Write(buf) // 写入数据到 QUIC 流
	if err != nil {
		log.Err(err).
			Str("client", conn.RemoteAddr().String()).
			Str("host", mcHost).
			Int("port", mcPort).
			Msg("Error writing to QUIC stream")
		return
	}
	// 释放 buf
	buf = nil

	var wg sync.WaitGroup
	wg.Add(1)
	go copyData(stream, conn, &wg)
	copyData(conn, stream, nil)
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
