// cmd/gateway/test_helpers_test.go 包含用于约束 test helpers 行为的测试。

package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/adminsession"
	"github.com/tursom/mc-gateway/internal/gatewayconfig"
	"github.com/tursom/mc-gateway/internal/gatewaymetrics"
)

func saveGatewayState(t *testing.T) func() {
	t.Helper()

	oldConfig := config
	oldCurrentPidFile := currentPidFile
	oldCurrentLogFile := currentLogFile
	oldLogger := log.Logger
	oldAdminStartup := adminStartup
	oldAdminDB := adminDB
	oldAdminDBPath := adminDBPath
	oldPluginsManager := pluginsManager
	oldAdminSessionManager := adminSessionManager
	oldRouteSnapshot := routeSnapshot.Clone()
	oldGatewayMetrics := gatewayMetrics

	config = gatewayconfig.Config{}
	currentPidFile = ""
	currentLogFile = ""
	adminStartup = adminconfig.Config{
		DBPath:               defaultAdminDBPath,
		TCPAdminPort:         defaultTCPPort,
		AdminPath:            defaultAdminPath,
		AdminAPIPrefix:       defaultAdminAPIPrefix,
		SessionTTL:           defaultAdminSessionTTL,
		PrometheusMode:       adminconfig.DefaultPrometheusMode,
		PrometheusListenAddr: adminconfig.DefaultPrometheusListenAddr,
	}
	adminDB = nil
	adminDBPath = ""
	pluginsManager = nil
	adminSessionManager = adminsession.NewManager()
	publishRouteSnapshot(nil)
	gatewayMetrics = gatewaymetrics.New()
	log.Logger = zerolog.New(io.Discard)

	return func() {
		if adminDB != nil && adminDB != oldAdminDB {
			_ = adminDB.Close()
		}
		if currentPidFile != "" && currentPidFile != oldCurrentPidFile {
			_ = os.Remove(currentPidFile)
		}

		config = oldConfig
		currentPidFile = oldCurrentPidFile
		currentLogFile = oldCurrentLogFile
		adminStartup = oldAdminStartup
		adminDB = oldAdminDB
		adminDBPath = oldAdminDBPath
		pluginsManager = oldPluginsManager
		adminSessionManager = oldAdminSessionManager
		publishRouteSnapshot(oldRouteSnapshot)
		gatewayMetrics = oldGatewayMetrics
		log.Logger = oldLogger
	}
}

func setGatewayTestRoutes(routes map[string]string) {
	publishRouteSnapshot(routes)
}

func gatewayTestPacket(host string, tail ...byte) []byte {
	protocolVersion := byte(0x63)
	nextState := byte(0x02)
	extra := []byte(nil)
	if len(tail) > 0 {
		protocolVersion = tail[0]
	}
	if len(tail) > 1 {
		nextState = tail[1]
	}
	if len(tail) > 2 {
		extra = tail[2:]
	}
	payload := []byte{0x00, protocolVersion, byte(len(host))}
	payload = append(payload, host...)
	payload = append(payload, 0x63, 0xdd, nextState)
	payload = append(payload, extra...)
	packet := []byte{byte(len(payload))}
	return append(packet, payload...)
}

type gatewayTestConn struct {
	mu       sync.Mutex
	readBuf  []byte
	readErr  error
	writeBuf bytes.Buffer
	writeErr error

	closed bool
	local  net.Addr
	remote net.Addr
}

func newGatewayTestConn(readBuf []byte) *gatewayTestConn {
	return &gatewayTestConn{
		readBuf: append([]byte(nil), readBuf...),
		local:   &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25565},
		remote:  &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 45678},
	}
}

func (c *gatewayTestConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.readErr != nil {
		return 0, c.readErr
	}
	if len(c.readBuf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *gatewayTestConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.writeBuf.Write(p)
}

func (c *gatewayTestConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	return nil
}

func (c *gatewayTestConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closed
}

func (c *gatewayTestConn) LocalAddr() net.Addr {
	return c.local
}

func (c *gatewayTestConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *gatewayTestConn) SetDeadline(time.Time) error {
	return nil
}

func (c *gatewayTestConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *gatewayTestConn) SetWriteDeadline(time.Time) error {
	return nil
}

type gatewayTestBackendResult struct {
	packet []byte
	err    error
}

func startGatewayTestUpstream(t testing.TB, packetLen int, reply []byte) (string, <-chan gatewayTestBackendResult) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan gatewayTestBackendResult, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- gatewayTestBackendResult{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

		packet := make([]byte, packetLen)
		if _, err := io.ReadFull(conn, packet); err != nil {
			done <- gatewayTestBackendResult{err: err}
			return
		}
		if len(reply) > 0 {
			if _, err := conn.Write(reply); err != nil {
				done <- gatewayTestBackendResult{err: err}
				return
			}
		}
		if err := conn.Close(); err != nil {
			done <- gatewayTestBackendResult{err: err}
			return
		}
		done <- gatewayTestBackendResult{packet: packet}
	}()

	return listener.Addr().String(), done
}

func waitGatewayTestUpstream(t testing.TB, done <-chan gatewayTestBackendResult) []byte {
	t.Helper()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("upstream server error = %v", result.err)
		}
		return result.packet
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream server")
		return nil
	}
}

func readGatewayTestPacketOnce(reader io.Reader, conn net.Conn, packetLen int, isTransportTimeout func(error) bool) gatewayTestBackendResult {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return gatewayTestBackendResult{err: err}
	}
	packet := make([]byte, packetLen)
	if _, err := io.ReadFull(reader, packet); err != nil {
		return gatewayTestBackendResult{err: err}
	}

	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return gatewayTestBackendResult{err: err}
	}
	extra := make([]byte, 1)
	n, err := reader.Read(extra)
	if n > 0 || err == nil {
		return gatewayTestBackendResult{err: errors.New("upstream received duplicate initial packet data")}
	}
	var netErr net.Error
	transportTimedOut := isTransportTimeout != nil && isTransportTimeout(err)
	if !errors.Is(err, io.EOF) && (!errors.As(err, &netErr) || !netErr.Timeout()) && !transportTimedOut {
		return gatewayTestBackendResult{err: err}
	}
	return gatewayTestBackendResult{packet: packet}
}
