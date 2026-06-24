package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	defaultTCPPort             = 25565
	defaultWebSocketPort       = 25566
	tcpWebInitialPacketTimeout = time.Second
	httpConnBacklog            = 128
	maxHTTPMethodPrefixLen     = len("OPTIONS ")
)

var httpMethodPrefixes = [][]byte{
	[]byte("GET "),
	[]byte("POST "),
	[]byte("HEAD "),
	[]byte("PUT "),
	[]byte("PATCH "),
	[]byte("DELETE "),
	[]byte("OPTIONS "),
	[]byte("CONNECT "),
	[]byte("TRACE "),
}

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type chanListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      net.Addr
}

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
		Str("path", normalizedWebSocketPath()).
		Msg("Listening for shared TCP and WebSocket connections")

	if err := serveTcpWebPortReuse(listener, newWebSocketHandler(), handleRequest); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatal().Err(err).
			Int("port", port).
			Msg("Shared TCP/WebSocket server stopped")
	}
}

func serveTcpWebPortReuse(listener net.Listener, handler http.Handler, tcpHandler func(net.Conn)) error {
	defer listener.Close()

	webListener := newChanListener(listener.Addr())
	webServer := &http.Server{Handler: handler}
	webServerDone := make(chan error, 1)

	go func() {
		err := webServer.Serve(webListener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			webServerDone <- err
			return
		}
		webServerDone <- nil
	}()

	defer func() {
		_ = webListener.Close()
		_ = webServer.Close()
		<-webServerDone
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}

			log.Err(err).Msg("Error accepting shared TCP/WebSocket connection")
			continue
		}

		setSocketOptions(conn)
		go handleTcpWebPortReuseConn(conn, webListener, tcpHandler, tcpWebInitialPacketTimeout)
	}
}

func handleTcpWebPortReuseConn(conn net.Conn, webListener *chanListener, tcpHandler func(net.Conn), timeout time.Duration) {
	peeked, err := readInitialPacket(conn, timeout)
	if err != nil {
		log.Debug().Err(err).
			Str("client", conn.RemoteAddr().String()).
			Msg("failed to read initial packet")
		conn.Close()
		return
	}
	if len(peeked) == 0 {
		log.Debug().
			Str("client", conn.RemoteAddr().String()).
			Msg("initial packet is empty")
		conn.Close()
		return
	}

	replayed := newReplayConn(conn, peeked)
	if isHTTPInitialPacket(peeked) {
		if !webListener.deliver(replayed) {
			log.Debug().
				Str("client", conn.RemoteAddr().String()).
				Msg("failed to deliver HTTP connection")
			conn.Close()
		}
		return
	}

	tcpHandler(replayed)
}

func readInitialPacket(conn net.Conn, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}

	buf := getProxyBuffer()
	defer putProxyBuffer(buf)

	var peeked []byte
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			peeked = append(peeked, buf[:n]...)
			if isHTTPInitialPacket(peeked) ||
				!isPotentialHTTPInitialPacket(peeked) ||
				len(peeked) >= maxHTTPMethodPrefixLen {
				return peeked, nil
			}
		}
		if err != nil {
			if len(peeked) > 0 && errors.Is(err, io.EOF) {
				return peeked, nil
			}
			return peeked, err
		}
		if n == 0 {
			return peeked, io.ErrNoProgress
		}
	}
}

func newReplayConn(conn net.Conn, peeked []byte) net.Conn {
	return &replayConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(peeked), conn),
	}
}

func isHTTPInitialPacket(buf []byte) bool {
	for _, prefix := range httpMethodPrefixes {
		if bytes.HasPrefix(buf, prefix) {
			return true
		}
	}
	return false
}

func isPotentialHTTPInitialPacket(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}

	for _, prefix := range httpMethodPrefixes {
		if len(buf) <= len(prefix) && bytes.HasPrefix(prefix, buf) {
			return true
		}
	}
	return false
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		conns:  make(chan net.Conn, httpConnBacklog),
		closed: make(chan struct{}),
		addr:   addr,
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *chanListener) Addr() net.Addr {
	return l.addr
}

func (l *chanListener) deliver(conn net.Conn) bool {
	select {
	case <-l.closed:
		return false
	default:
	}

	select {
	case l.conns <- conn:
		return true
	case <-l.closed:
		return false
	default:
		return false
	}
}
