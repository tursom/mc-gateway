package main

import (
	"bytes"
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
	"github.com/tursom/mc-gateway/plugin/api"
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

	pluginLock.Lock()
	oldPlugins := plugins
	oldHooks := hooks
	plugins = make(map[string]api.Plugin)
	hooks = make(map[string]map[string]any)
	pluginLock.Unlock()

	config = gatewayconfig.Config{}
	currentPidFile = ""
	currentLogFile = ""
	adminStartup = adminconfig.Config{
		DBPath:         defaultAdminDBPath,
		TCPAdminPort:   defaultTCPPort,
		AdminPath:      defaultAdminPath,
		AdminAPIPrefix: defaultAdminAPIPrefix,
		SessionTTL:     defaultAdminSessionTTL,
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

		pluginLock.Lock()
		plugins = oldPlugins
		hooks = oldHooks
		pluginLock.Unlock()
	}
}

func setGatewayTestRoutes(routes map[string]string) {
	publishRouteSnapshot(routes)
}

func gatewayTestPacket(host string, tail ...byte) []byte {
	packet := []byte{
		byte(4 + 1 + len(host) + len(tail)),
		0x00,
		0x00,
		0x00,
		byte(len(host)),
	}
	packet = append(packet, host...)
	packet = append(packet, tail...)
	return packet
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

func registerGatewayUpstreamHook(
	t *testing.T,
	acceptor func(net.Conn, string) bool,
	handler func(net.Conn, string) (net.Conn, error),
) {
	t.Helper()

	pluginLock.Lock()
	hooks["test-plugin"] = make(map[string]any)
	pluginLock.Unlock()

	if err := api.RegisterHookHandler(&Gateway{pluginId: "test-plugin"}, api.HookUpstream, acceptor, handler); err != nil {
		t.Fatalf("RegisterHookHandler() error = %v", err)
	}
}
