package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
)

func TestHandleRequestProxiesAndClosesConnections(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn([]byte("reply"))
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed")
	}
	if !upstream.closed {
		t.Fatal("upstream connection was not closed")
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
	if got := source.writeBuf.String(); got != "reply" {
		t.Fatalf("proxied reply = %q, want reply", got)
	}
}

func TestHandleRequestProtocolProxyReplaysInitialDataOnce(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example", 0x63, 0x02)
	source := newGatewayTestConn(packet)
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{handler: func(req api.UpstreamConnectRequest) (net.Conn, error) {
			if req.ServerHost != "play.example" || req.Host != "play.example" {
				t.Fatalf("request host = %q/%q, want play.example", req.ServerHost, req.Host)
			}
			if req.ProtocolVersion != 0x63 || req.NextState != 0x02 {
				t.Fatalf("protocol/next state = %d/%d, want 99/2", req.ProtocolVersion, req.NextState)
			}
			if !bytes.Equal(req.InitialData, packet) {
				t.Fatalf("initial data = %v, want %v", req.InitialData, packet)
			}
			gatewayEnd, pluginEnd := net.Pipe()
			go func() {
				defer pluginEnd.Close()
				buf := make([]byte, len(packet))
				if _, err := io.ReadFull(pluginEnd, buf); err != nil {
					t.Errorf("plugin endpoint ReadFull() error = %v", err)
					return
				}
				if !bytes.Equal(buf, packet) {
					t.Errorf("plugin endpoint initial data = %v, want %v", buf, packet)
					return
				}
				if host := protocol.GetMcHost(buf); host != "play.example" {
					t.Errorf("plugin endpoint host = %q, want play.example", host)
					return
				}
				_, _ = pluginEnd.Write([]byte("login rejected"))
			}()
			return gatewayEnd, nil
		}},
	})
	artifact := uploadGatewayTestArtifactWithCapabilities(t, pluginsManager, "proxy-plugin", gatewayProtocolProxyCapabilities())
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "proxy-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGatewayPluginGovernanceForTest(t, "proxy-plugin", artifact.ID)
	if _, err := pluginsManager.Enable(context.Background(), "admin", "proxy-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	handleRequest(source)

	if got := source.writeBuf.String(); got != "login rejected" {
		t.Fatalf("source response = %q, want login rejected", got)
	}
	plan := pluginsManager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 {
		t.Fatalf("dispatch handlers = %d, want 1", len(plan.Handlers))
	}
	if plan.Handlers[0].Mode != pluginmanager.UpstreamModeProtocolProxy {
		t.Fatalf("handler mode = %q, want protocol-proxy", plan.Handlers[0].Mode)
	}
	if plan.Handlers[0].ProxyStarted != 1 || plan.Handlers[0].ProxyCompleted != 1 {
		t.Fatalf("proxy lifecycle = started %d completed %d, want 1/1", plan.Handlers[0].ProxyStarted, plan.Handlers[0].ProxyCompleted)
	}
}

func TestHandleRequestManagedErrBlockedClosesSource(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(gatewayTestPacket("play.example"))
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{handler: func(api.UpstreamConnectRequest) (net.Conn, error) {
			return nil, api.ErrBlocked
		}},
	})
	artifact := uploadGatewayTestArtifact(t, pluginsManager, "blocked-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "blocked-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "blocked-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	handleRequest(source)

	if !source.closed {
		t.Fatal("source was not closed")
	}
	if source.writeBuf.Len() != 0 {
		t.Fatalf("source response length = %d, want 0", source.writeBuf.Len())
	}
}

func TestHandleRequestProtocolProxyPanicOnlyFailsCurrentConnection(t *testing.T) {
	defer saveGatewayState(t)()

	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	calls := 0
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{handler: func(api.UpstreamConnectRequest) (net.Conn, error) {
			calls++
			if calls == 1 {
				panic("boom")
			}
			upstream := newGatewayTestConn(nil)
			upstream.writeBuf.WriteString("ok")
			return upstream, nil
		}},
	})
	artifact := uploadGatewayTestArtifact(t, pluginsManager, "panic-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "panic-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "panic-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	first := newGatewayTestConn(gatewayTestPacket("play.example"))
	handleRequest(first)
	if !first.closed {
		t.Fatal("first connection was not closed")
	}

	second := newGatewayTestConn(gatewayTestPacket("play.example"))
	handleRequest(second)
	if !second.closed {
		t.Fatal("second connection was not closed")
	}
	plan := pluginsManager.DispatchPlan(context.Background())
	if got := plan.Handlers[0].Panics; got != 1 {
		t.Fatalf("panic count = %d, want 1", got)
	}
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2", calls)
	}
}

func TestHandleRequestProtocolProxyDisableSkipsNewConnections(t *testing.T) {
	defer saveGatewayState(t)()

	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	proxyCalls := 0
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{handler: func(req api.UpstreamConnectRequest) (net.Conn, error) {
			proxyCalls++
			gatewayEnd, pluginEnd := net.Pipe()
			initialLen := len(req.InitialData)
			go func() {
				defer pluginEnd.Close()
				_, _ = io.ReadFull(pluginEnd, make([]byte, initialLen))
			}()
			return gatewayEnd, nil
		}},
	})
	artifact := uploadGatewayTestArtifactWithCapabilities(t, pluginsManager, "proxy-plugin", gatewayProtocolProxyCapabilities())
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "proxy-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGatewayPluginGovernanceForTest(t, "proxy-plugin", artifact.ID)
	if _, err := pluginsManager.Enable(context.Background(), "admin", "proxy-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	first := newGatewayTestConn(gatewayTestPacket("play.example"))
	handleRequest(first)
	if proxyCalls != 1 {
		t.Fatalf("proxy calls after first request = %d, want 1", proxyCalls)
	}
	if _, err := pluginsManager.Disable(context.Background(), "admin", "proxy-plugin"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}

	legacyUpstream := newGatewayTestConn(nil)
	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) { return legacyUpstream, nil },
	)
	second := newGatewayTestConn(gatewayTestPacket("play.example"))
	handleRequest(second)
	if proxyCalls != 1 {
		t.Fatalf("proxy calls after disable = %d, want still 1", proxyCalls)
	}
	if legacyUpstream.writeBuf.Len() == 0 {
		t.Fatal("legacy upstream did not receive second request")
	}
}

func TestHandleRequestRecoversAndClosesConnection(t *testing.T) {
	defer saveGatewayState(t)()

	source := &panicReadGatewayConn{gatewayTestConn: newGatewayTestConn(nil)}

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed after panic")
	}
}

func TestGatewayHandleConnStartsRequestGoroutine(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(nil)
	source.readErr = errors.New("read failed")

	(&Gateway{}).HandleConn(source)

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for HandleConn goroutine")
		case <-ticker.C:
			if source.isClosed() {
				return
			}
		}
	}
}

func TestGatewayTestOpPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("TestOp() did not panic")
		}
	}()

	(&Gateway{}).TestOp()
}

type panicReadGatewayConn struct {
	*gatewayTestConn
}

func (c *panicReadGatewayConn) Read([]byte) (int, error) {
	panic("read panic")
}
