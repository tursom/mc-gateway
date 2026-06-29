// examples/plugins/upstream-rewrite/main_test.go verifies the upstream rewrite example behavior.

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestPluginFactory(t *testing.T) {
	if plugin := Plugin(); plugin == nil {
		t.Fatal("Plugin() returned nil")
	} else if _, ok := plugin.(api.Plugin); !ok {
		t.Fatal("Plugin() did not return api.Plugin")
	}
}

func TestUpstreamRewriteDialerAndPass(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_, _ = conn.Write([]byte("rewritten"))
			_ = conn.Close()
		}
		close(accepted)
	}()

	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{MatchHost: "play.example", Upstream: listener.Addr().String()}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	gateway := &recordingGateway{hooks: make(map[string]any)}
	if err := plugin.Init(gateway); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	hook, ok := gateway.hooks[api.HookUpstreamConnect.Key()].(api.HookHandler[api.UpstreamConnectAcceptor, api.UpstreamConnectHandler])
	if !ok {
		t.Fatalf("hook %q not registered: %#v", api.HookUpstreamConnect.Key(), gateway.hooks)
	}
	if !hook.Acceptor()(api.UpstreamConnectRequest{Host: "play.example"}) {
		t.Fatal("acceptor rejected matching host")
	}
	if hook.Acceptor()(api.UpstreamConnectRequest{Host: "other.example", Upstream: "vanilla:25565"}) {
		t.Fatal("acceptor accepted non-matching host/upstream")
	}
	conn, err := hook.Handler()(api.UpstreamConnectRequest{Host: "play.example"})
	if err != nil {
		t.Fatalf("handler matching host error = %v", err)
	}
	defer conn.Close()
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll(rewritten upstream) error = %v", err)
	}
	if string(response) != "rewritten" {
		t.Fatalf("rewritten upstream response = %q, want rewritten", response)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("rewritten upstream was not dialed")
	}
	if _, err := hook.Handler()(api.UpstreamConnectRequest{Host: "other.example", Upstream: "vanilla:25565"}); !errors.Is(err, api.ErrPass) {
		t.Fatalf("handler non-match error = %v, want api.ErrPass", err)
	}
}

type recordingGateway struct {
	hooks map[string]any
	wg    sync.WaitGroup
}

func (g *recordingGateway) HandleConn(net.Conn)            {}
func (g *recordingGateway) ExitWaitGroup() *sync.WaitGroup { return &g.wg }
func (g *recordingGateway) Hook(hook string, handler any) error {
	g.hooks[hook] = handler
	return nil
}
func (g *recordingGateway) EmitEvent(context.Context, string, map[string]string) error {
	return nil
}
func (g *recordingGateway) ObserveMetric(context.Context, string, float64, map[string]string) error {
	return nil
}
func (g *recordingGateway) Logger() api.Logger                              { return nil }
func (g *recordingGateway) DataStore() api.DataStore                        { return nil }
func (g *recordingGateway) FileStore() api.FileStore                        { return nil }
func (g *recordingGateway) ExternalClient(string) api.ExternalClient        { return nil }
func (g *recordingGateway) RegisterBackgroundTask(api.BackgroundTask) error { return nil }
