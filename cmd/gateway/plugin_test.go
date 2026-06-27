// cmd/gateway/plugin_test.go 包含用于约束 plugin 行为的测试。

package main

import (
	"errors"
	"net"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestGatewayHookStoresHandler(t *testing.T) {
	defer saveGatewayState(t)()

	pluginLock.Lock()
	hooks["plugin-a"] = make(map[string]any)
	pluginLock.Unlock()

	handler := "handler"
	if err := (&Gateway{pluginId: "plugin-a"}).Hook("hook-a", handler); err != nil {
		t.Fatalf("Hook() error = %v", err)
	}
	if got := hooks["plugin-a"]["hook-a"]; got != handler {
		t.Fatalf("stored hook = %v, want %v", got, handler)
	}
}

func TestHandlerAdaptors(t *testing.T) {
	if got := Handler1[int, bool](7)(func(v int) bool { return v == 7 }); !got {
		t.Fatal("Handler1 did not pass its argument")
	}
	if got := Handler2[int, string, bool](7, "x")(func(v int, s string) bool {
		return v == 7 && s == "x"
	}); !got {
		t.Fatal("Handler2 did not pass its arguments")
	}
	gotString, gotBool := Handler1R2[int, string, bool](7)(func(v int) (string, bool) {
		return "ok", v == 7
	})
	if gotString != "ok" || !gotBool {
		t.Fatalf("Handler1R2() = (%q, %v), want (ok, true)", gotString, gotBool)
	}
}

func TestInvokeFirstHookHandler(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(nil)
	upstream := newGatewayTestConn(nil)
	registerGatewayUpstreamHook(
		t,
		func(gotSource net.Conn, host string) bool {
			return gotSource == source && host == "backend.example:25565"
		},
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	var handled bool
	ok, err := invokeFirstHookHandler(
		api.HookUpstream,
		Handler2[net.Conn, string, bool](source, "backend.example:25565"),
		func(handler func(net.Conn, string) (net.Conn, error)) error {
			got, err := handler(source, "backend.example:25565")
			if err != nil {
				return err
			}
			handled = got == upstream
			return nil
		},
	)
	if err != nil {
		t.Fatalf("invokeFirstHookHandler() error = %v", err)
	}
	if !ok {
		t.Fatal("invokeFirstHookHandler() ok = false, want true")
	}
	if !handled {
		t.Fatal("first matching handler was not invoked")
	}
}

func TestInvokeFirstHookHandlerNoMatch(t *testing.T) {
	defer saveGatewayState(t)()

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return false },
		func(net.Conn, string) (net.Conn, error) {
			t.Fatal("handler should not be invoked")
			return nil, nil
		},
	)

	ok, err := invokeFirstHookHandler(
		api.HookUpstream,
		Handler2[net.Conn, string, bool](newGatewayTestConn(nil), "backend.example:25565"),
		func(func(net.Conn, string) (net.Conn, error)) error {
			t.Fatal("callback should not be invoked")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("invokeFirstHookHandler() error = %v", err)
	}
	if ok {
		t.Fatal("invokeFirstHookHandler() ok = true, want false")
	}
}

func TestInvokeFirstHookHandlerReturnsCallbackError(t *testing.T) {
	defer saveGatewayState(t)()

	wantErr := errors.New("callback failed")
	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) { return nil, nil },
	)

	ok, err := invokeFirstHookHandler(
		api.HookUpstream,
		Handler2[net.Conn, string, bool](newGatewayTestConn(nil), "backend.example:25565"),
		func(func(net.Conn, string) (net.Conn, error)) error {
			return wantErr
		},
	)
	if !ok {
		t.Fatal("invokeFirstHookHandler() ok = false, want true")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("invokeFirstHookHandler() error = %v, want %v", err, wantErr)
	}
}

func TestInvokeAllHookHandler(t *testing.T) {
	defer saveGatewayState(t)()

	for _, pluginID := range []string{"plugin-a", "plugin-b"} {
		pluginLock.Lock()
		hooks[pluginID] = make(map[string]any)
		pluginLock.Unlock()
		if err := api.RegisterHookHandler(
			&Gateway{pluginId: pluginID},
			api.HookUpstream,
			func(net.Conn, string) bool { return true },
			func(net.Conn, string) (net.Conn, error) { return nil, nil },
		); err != nil {
			t.Fatalf("RegisterHookHandler(%s) error = %v", pluginID, err)
		}
	}

	calls := 0
	err := invokeAllHookHandler(
		api.HookUpstream,
		Handler2[net.Conn, string, bool](newGatewayTestConn(nil), "backend.example:25565"),
		func(func(net.Conn, string) (net.Conn, error)) error {
			calls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("invokeAllHookHandler() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("invokeAllHookHandler() calls = %d, want 2", calls)
	}
}

func TestInvokeAllHookHandlerReturnsCallbackError(t *testing.T) {
	defer saveGatewayState(t)()

	wantErr := errors.New("callback failed")
	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) { return nil, nil },
	)

	err := invokeAllHookHandler(
		api.HookUpstream,
		Handler2[net.Conn, string, bool](newGatewayTestConn(nil), "backend.example:25565"),
		func(func(net.Conn, string) (net.Conn, error)) error {
			return wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("invokeAllHookHandler() error = %v, want %v", err, wantErr)
	}
}

func TestLoadPluginsDisablesExistingPlugin(t *testing.T) {
	defer saveGatewayState(t)()

	plugin := &gatewayPluginStub{}
	plugins["plugin-a"] = plugin
	hooks["plugin-a"] = map[string]any{"hook": "handler"}
	config.Plugin = map[string]map[string]any{
		"plugin-a": {"enable": false},
	}

	loadPlugins()

	if plugin.destroyCalls != 1 {
		t.Fatalf("Destroy() calls = %d, want 1", plugin.destroyCalls)
	}
	if _, ok := plugins["plugin-a"]; ok {
		t.Fatal("disabled plugin was not removed")
	}
	if _, ok := hooks["plugin-a"]; ok {
		t.Fatal("disabled plugin hooks were not removed")
	}
}

func TestLoadPluginsSkipsAlreadyLoadedEnabledPlugin(t *testing.T) {
	defer saveGatewayState(t)()

	plugin := &gatewayPluginStub{}
	plugins["plugin-a"] = plugin
	hooks["plugin-a"] = make(map[string]any)
	config.Plugin = map[string]map[string]any{
		"plugin-a": {"enable": true, "file": "missing-plugin-file"},
	}

	loadPlugins()

	if plugins["plugin-a"] != plugin {
		t.Fatal("existing enabled plugin was replaced")
	}
	if plugin.destroyCalls != 0 {
		t.Fatalf("Destroy() calls = %d, want 0", plugin.destroyCalls)
	}
}

func TestLoadPluginsIgnoresMissingPluginFile(t *testing.T) {
	defer saveGatewayState(t)()

	config.Plugin = map[string]map[string]any{
		"plugin-a": {"enable": true, "file": "missing-plugin-file"},
	}

	loadPlugins()

	if _, ok := plugins["plugin-a"]; ok {
		t.Fatal("missing plugin file should not be registered")
	}
	if _, ok := hooks["plugin-a"]; ok {
		t.Fatal("missing plugin file should not create hooks")
	}
}

type gatewayPluginStub struct {
	destroyCalls int
}

func (p *gatewayPluginStub) Init(api.Gateway) error {
	return nil
}

func (p *gatewayPluginStub) Destroy() error {
	p.destroyCalls++
	return nil
}

func (p *gatewayPluginStub) NewConfigObj() any {
	return &struct{}{}
}

func (p *gatewayPluginStub) ReloadConfig(any) error {
	return nil
}

func TestGatewayExitWaitGroup(t *testing.T) {
	if got := (&Gateway{}).ExitWaitGroup(); got != &exitWaitGroup {
		t.Fatalf("ExitWaitGroup() = %p, want %p", got, &exitWaitGroup)
	}
}

func TestGatewayPluginStubSatisfiesInterface(t *testing.T) {
	var _ api.Plugin = (*gatewayPluginStub)(nil)
	var _ api.Gateway = (*Gateway)(nil)
}
