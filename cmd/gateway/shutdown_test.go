package main

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

func TestRunEnabledServicesStopsWhenContextIsCanceled(t *testing.T) {
	defer saveGatewayState(t)()
	port := reserveGatewayTestTCPPort(t)
	config.Tcp.Port = port

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runEnabledServices(ctx) }()
	waitForGatewayTestTCPListener(t, port, done)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runEnabledServices() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runEnabledServices() did not stop after context cancellation")
	}
}

func TestRunEnabledServicesReturnsListenerError(t *testing.T) {
	defer saveGatewayState(t)()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()
	config.Tcp.Port = listener.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runEnabledServices(ctx); err == nil {
		t.Fatal("runEnabledServices() error = nil, want listener error")
	}
}

func TestCloseGatewayRuntimeDestroysPluginsBeforeClosingDatabase(t *testing.T) {
	defer saveGatewayState(t)()
	db := newGatewayTestPluginDB(t)
	plugin := &databaseCheckingPlugin{db: db}
	adminDB = db
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           db,
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{
			plugin: plugin,
		},
	})
	artifact := uploadGatewayTestArtifact(t, pluginsManager, "shutdown-db-order")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "shutdown-db-order", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "shutdown-db-order"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := closeGatewayRuntime(ctx); err != nil {
		t.Fatalf("closeGatewayRuntime() error = %v", err)
	}
	if err := plugin.destroyError(); err != nil {
		t.Fatalf("database access during Destroy() error = %v", err)
	}
	if err := db.PingContext(context.Background()); err == nil {
		t.Fatal("database remains open after closeGatewayRuntime()")
	}
	if pluginsManager != nil || adminDB != nil {
		t.Fatalf("runtime globals after close = manager %v db %v, want nil", pluginsManager, adminDB)
	}
}

func reserveGatewayTestTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close(listener) error = %v", err)
	}
	return port
}

func waitForGatewayTestTCPListener(t *testing.T, port int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("service stopped before listener was ready: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for listener %s", address)
}

type databaseCheckingPlugin struct {
	api.AbstractPlugin
	db *sql.DB

	mu         sync.Mutex
	destroyErr error
}

func (p *databaseCheckingPlugin) Destroy() error {
	err := p.db.PingContext(context.Background())
	p.mu.Lock()
	p.destroyErr = err
	p.mu.Unlock()
	return nil
}

func (p *databaseCheckingPlugin) destroyError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.destroyErr
}
