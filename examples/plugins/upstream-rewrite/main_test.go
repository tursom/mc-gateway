// examples/plugins/upstream-rewrite/main_test.go verifies the upstream rewrite example behavior.

package main

import (
	"context"
	"sync"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestPluginFactory(t *testing.T) {
	if plugin := Plugin(); plugin == nil {
		t.Fatal("Plugin() returned nil")
	} else if _, ok := plugin.(api.Plugin); !ok {
		t.Fatal("Plugin() did not return api.Plugin")
	}
}

func TestUpstreamRewriteRouteOverrideAndPass(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{MatchHost: "play.example", Upstream: "rewritten:25565"}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	gateway := &recordingGateway{hooks: make(map[string]any)}
	if err := plugin.Init(gateway); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	hook, ok := gateway.hooks[api.HookRouteResolve.Key()].(api.HookHandler[api.RouteResolveAcceptor, api.RouteResolveHandler])
	if !ok {
		t.Fatalf("hook %q not registered: %#v", api.HookRouteResolve.Key(), gateway.hooks)
	}
	if !hook.Acceptor()(api.RouteResolveRequest{Host: "play.example"}) {
		t.Fatal("acceptor rejected matching host")
	}
	if hook.Acceptor()(api.RouteResolveRequest{Host: "other.example"}) {
		t.Fatal("acceptor accepted non-matching host")
	}
	decision, err := hook.Handler()(api.RouteResolveRequest{Host: "play.example"})
	if err != nil {
		t.Fatalf("matching route error = %v", err)
	}
	if decision.Action != api.RouteDecisionOverride || decision.Upstream != "rewritten:25565" {
		t.Fatalf("matching route decision = %+v", decision)
	}
	decision, err = hook.Handler()(api.RouteResolveRequest{Host: "other.example"})
	if err != nil || decision.Action != api.RouteDecisionPass {
		t.Fatalf("non-matching route decision = %+v, error = %v", decision, err)
	}
}

type recordingGateway struct {
	hooks map[string]any
	wg    sync.WaitGroup
}

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
