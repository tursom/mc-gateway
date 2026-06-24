package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

func runTcp(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	port := normalizedTCPPort()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal().Err(err).
			Int("port", port).
			Msg("Failed to listen on port")
	}
	defer listener.Close()
	log.Info().
		Int("port", port).
		Msg("Listening for TCP connections")

	for {
		// 接受传入的连接
		conn, err := listener.Accept()
		if err != nil {
			log.Err(err).Msg("Error accepting")
			continue
		}
		setSocketOptions(conn)
		// 处理连接
		go handleRequest(conn)
	}
}

func upstreamTcp(host string) net.Conn {
	conn, err := tcpDialer.Dial("tcp", host)
	if err != nil {
		log.Err(err).Str("host", host).Msg("Error dialing upstream")
		return nil
	}
	setSocketOptions(conn)
	return conn

}

var tcpDialer = net.Dialer{
	Timeout:   3 * time.Second,
	KeepAlive: 30 * time.Second,
}

func setSocketOptions(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true) // 禁用 Nagle 算法
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}
