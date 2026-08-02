// plugin/api/api_test.go 包含用于约束 api 行为的测试。

package api

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestAbstractPluginDefaults(t *testing.T) {
	var plugin AbstractPlugin

	if err := plugin.Init(nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := plugin.Destroy(); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if err := plugin.ReloadConfig(struct{}{}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	if cfg := plugin.NewConfigObj(); cfg == nil {
		t.Fatal("NewConfigObj() returned nil")
	}
}

func TestHookTypesAndHandlers(t *testing.T) {
	if got := HookUpstreamConnectV2.Key(); got != "upstream.connect/v2" {
		t.Fatalf("HookUpstreamConnectV2.Key() = %q, want upstream.connect/v2", got)
	}

	acceptor := func(net.Conn, string) bool { return true }
	handler := func(net.Conn, string) (net.Conn, error) { return nil, nil }
	hookHandler := HookHandler[
		func(net.Conn, string) bool,
		func(net.Conn, string) (net.Conn, error),
	]{acceptor: acceptor, handler: handler}

	if hookHandler.Acceptor() == nil {
		t.Fatal("Acceptor() returned nil")
	}
	if hookHandler.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestRegisterHookHandler(t *testing.T) {
	gateway := &recordingGateway{hooks: make(map[string]any)}
	handler := UpstreamConnectHandlerV2(func(UpstreamConnectRequestV2) error { return nil })

	if err := RegisterUpstreamConnectHandlerV2(gateway, handler); err != nil {
		t.Fatalf("RegisterUpstreamConnectHandlerV2() error = %v", err)
	}

	rawHandler, ok := gateway.hooks[HookUpstreamConnectV2.Key()]
	if !ok {
		t.Fatalf("hook %q was not registered", HookUpstreamConnectV2.Key())
	}
	registered, ok := rawHandler.(UpstreamConnectHandlerV2)
	if !ok {
		t.Fatalf("registered hook type = %T", rawHandler)
	}
	if registered == nil {
		t.Fatal("registered handler is nil")
	}
}

func TestIngressContextCloneCopiesMutableFacts(t *testing.T) {
	original := IngressContext{
		HTTP: &HTTPIngressContext{Headers: http.Header{"X-Test": []string{"original"}}},
		QUIC: &QUICIngressContext{ApplicationProtocol: "minecraft"},
	}
	cloned := original.Clone()
	cloned.HTTP.Headers.Set("X-Test", "changed")
	cloned.QUIC.ApplicationProtocol = "changed"
	if original.HTTP.Headers.Get("X-Test") != "original" || original.QUIC.ApplicationProtocol != "minecraft" {
		t.Fatalf("Clone() mutated original ingress: %+v", original)
	}
}

type recordingGateway struct {
	hooks map[string]any
	wg    sync.WaitGroup
}

func (g *recordingGateway) ExitWaitGroup() *sync.WaitGroup {
	return &g.wg
}

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

func (g *recordingGateway) Logger() Logger {
	return testLogger{}
}

func (g *recordingGateway) DataStore() DataStore {
	return testDataStore{}
}

func (g *recordingGateway) FileStore() FileStore {
	return testFileStore{}
}

func (g *recordingGateway) ExternalClient(string) ExternalClient {
	return testExternalClient{}
}

func (g *recordingGateway) RegisterBackgroundTask(BackgroundTask) error {
	return nil
}

type testLogger struct{}

func (testLogger) Debug(context.Context, string, map[string]string) {}
func (testLogger) Info(context.Context, string, map[string]string)  {}
func (testLogger) Warn(context.Context, string, map[string]string)  {}
func (testLogger) Error(context.Context, string, map[string]string) {}

type testDataStore struct{}

func (testDataStore) Put(context.Context, DataRecord) error { return nil }
func (testDataStore) Get(context.Context, string) (DataRecord, error) {
	return DataRecord{}, nil
}
func (testDataStore) Delete(context.Context, string) error { return nil }

type testFileStore struct{}

func (testFileStore) ResourcePath(string) (string, error) { return "", nil }
func (testFileStore) Write(context.Context, string, string, []byte, string, time.Duration) error {
	return nil
}
func (testFileStore) Read(context.Context, string, string, int64) ([]byte, error) {
	return nil, nil
}
func (testFileStore) Delete(context.Context, string, string) error { return nil }

type testExternalClient struct{}

func (testExternalClient) DoHTTP(context.Context, ExternalRequest) (ExternalResponse, error) {
	return ExternalResponse{}, nil
}
func (testExternalClient) DialTCP(context.Context, string, time.Duration) (net.Conn, error) {
	return nil, nil
}
func (testExternalClient) HealthCheck(context.Context) error { return nil }
