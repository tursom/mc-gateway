package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/tcphttpmux"
)

const (
	defaultTCPPort             = 25565
	tcpWebInitialPacketTimeout = tcphttpmux.DefaultInitialPacketTimeout
)

func normalizedTCPPort() int {
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
	if config.WebSocket.Path == "" {
		return "/"
	}
	return config.WebSocket.Path
}

func tcpWebPortReuseEnabled() bool {
	return config.Tcp.Enable &&
		config.WebSocket.Enable &&
		normalizedTCPPort() == normalizedWebSocketPort()
}

func runTcpWebPortReuse(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	port := normalizedTCPPort()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal().Err(err).
			Int("port", port).
			Msg("Failed to listen on shared TCP/WebSocket port")
	}

	log.Info().
		Int("port", port).
		Str("admin_path", adminStartup.AdminPath).
		Msg("Listening for shared TCP and Admin connections")

	if err := serveTcpWebPortReuse(listener, newGatewayHTTPHandler(), handleRequest); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatal().Err(err).
			Int("port", port).
			Msg("Shared TCP/Admin server stopped")
	}
}

func serveTcpWebPortReuse(listener net.Listener, handler http.Handler, tcpHandler func(net.Conn)) error {
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
