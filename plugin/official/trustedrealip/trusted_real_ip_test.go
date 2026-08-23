package trustedrealip

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

type recordingGateway struct {
	hooks map[string]any
	wg    sync.WaitGroup
}

func (g *recordingGateway) ExitWaitGroup() *sync.WaitGroup { return &g.wg }
func (g *recordingGateway) Hook(key string, handler any) error {
	g.hooks[key] = handler
	return nil
}
func (*recordingGateway) EmitEvent(context.Context, string, map[string]string) error { return nil }
func (*recordingGateway) ObserveMetric(context.Context, string, float64, map[string]string) error {
	return nil
}
func (*recordingGateway) Logger() api.Logger                              { return nil }
func (*recordingGateway) DataStore() api.DataStore                        { return nil }
func (*recordingGateway) FileStore() api.FileStore                        { return nil }
func (*recordingGateway) ExternalClient(string) api.ExternalClient        { return nil }
func (*recordingGateway) RegisterBackgroundTask(api.BackgroundTask) error { return nil }

type testFlow struct {
	state api.ConnectionState
	next  bool
}

func (f *testFlow) Next(state api.ConnectionState) error { f.state, f.next = state, true; return nil }
func (f *testFlow) Core(api.ConnectionState) error       { return nil }

func TestTrustedRealIP(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.ReloadConfig(&Config{Header: "X-Real-IP", TrustedPeers: []string{"127.0.0.1/32", "::1/128"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, value, want string
	}{
		{name: "ipv4", peer: "127.0.0.1:1234", value: "198.51.100.7", want: "198.51.100.7:0"},
		{name: "ipv6", peer: "[::1]:1234", value: "2001:db8::7", want: "[2001:db8::7]:0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer right.Close()
			flow := &testFlow{}
			req := api.UpstreamConnectRequestV2{
				Context: context.Background(), PeerAddr: test.peer,
				Connection: api.ConnectionState{Stream: left, EffectiveSourceAddr: test.peer},
				Ingress:    api.IngressContext{Transport: "websocket", HTTP: &api.HTTPIngressContext{Headers: http.Header{"X-Real-Ip": []string{test.value}}}},
				Flow:       flow,
			}
			if err := plugin.handle(req); err != nil {
				t.Fatal(err)
			}
			if !flow.next || flow.state.EffectiveSourceAddr != test.want {
				t.Fatalf("next=%v source=%q, want true %q", flow.next, flow.state.EffectiveSourceAddr, test.want)
			}
		})
	}
}

func TestTrustedRealIPRejectsInvalidInput(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.ReloadConfig(&Config{Header: "X-Real-IP", TrustedPeers: []string{"127.0.0.1/32"}}); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]string{nil, {"198.51.100.1", "198.51.100.2"}, {"198.51.100.1, 198.51.100.2"}, {"invalid"}} {
		left, right := net.Pipe()
		flow := &testFlow{}
		req := api.UpstreamConnectRequestV2{PeerAddr: "127.0.0.1:1", Connection: api.ConnectionState{Stream: left}, Ingress: api.IngressContext{Transport: "websocket", HTTP: &api.HTTPIngressContext{Headers: http.Header{"X-Real-Ip": values}}}, Flow: flow}
		if err := plugin.handle(req); err != nil {
			t.Fatal(err)
		}
		if flow.next {
			t.Fatal("invalid header entered continuation")
		}
		_ = right.Close()
	}
}

func TestTrustedRealIPConfigValidationAndTransportPassThrough(t *testing.T) {
	for _, config := range []Config{
		{Header: "", TrustedPeers: []string{"127.0.0.1/32"}},
		{Header: "X-Real-IP"},
		{Header: "X-Real-IP", TrustedPeers: []string{"invalid"}},
	} {
		if err := (&Plugin{}).ReloadConfig(&config); err == nil {
			t.Fatalf("ReloadConfig(%+v) error = nil", config)
		}
	}

	plugin := &Plugin{}
	if err := plugin.ReloadConfig(&Config{Header: "X-Real-IP", TrustedPeers: []string{"127.0.0.1/32"}}); err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	flow := &testFlow{}
	state := api.ConnectionState{Stream: left, EffectiveSourceAddr: "192.0.2.1:1234"}
	if err := plugin.handle(api.UpstreamConnectRequestV2{
		PeerAddr: "203.0.113.1:1", Connection: state,
		Ingress: api.IngressContext{Transport: "tcp"}, Flow: flow,
	}); err != nil {
		t.Fatal(err)
	}
	if !flow.next || flow.state.EffectiveSourceAddr != state.EffectiveSourceAddr {
		t.Fatalf("non-WebSocket flow = %+v", flow)
	}
}

func TestTrustedRealIPRejectsUntrustedPeer(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.ReloadConfig(&Config{Header: "X-Real-IP", TrustedPeers: []string{"127.0.0.1/32"}}); err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer right.Close()
	flow := &testFlow{}
	if err := plugin.handle(api.UpstreamConnectRequestV2{
		PeerAddr: "203.0.113.1:1", Connection: api.ConnectionState{Stream: left},
		Ingress: api.IngressContext{Transport: "websocket", HTTP: &api.HTTPIngressContext{Headers: http.Header{"X-Real-Ip": []string{"198.51.100.1"}}}}, Flow: flow,
	}); err != nil {
		t.Fatal(err)
	}
	if flow.next {
		t.Fatal("untrusted peer entered continuation")
	}
	if _, err := right.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after rejection error = %v, want closed pipe", err)
	}
}

func TestTrustedRealIPPublicLifecycleRegistersHook(t *testing.T) {
	plugin, ok := New().(*Plugin)
	if !ok {
		t.Fatalf("New() = %T, want *Plugin", New())
	}
	config, ok := plugin.NewConfigObj().(*Config)
	if !ok || config.Header != "X-Real-IP" || len(config.TrustedPeers) != 1 {
		t.Fatalf("NewConfigObj() = %#v, want secure loopback defaults", config)
	}
	if err := plugin.ReloadConfig(config); err != nil {
		t.Fatalf("ReloadConfig(defaults) error = %v", err)
	}
	gateway := &recordingGateway{hooks: map[string]any{}}
	if err := plugin.Init(gateway); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if gateway.hooks[api.HookUpstreamConnectV2.Key()] == nil {
		t.Fatalf("Init() did not register %s", api.HookUpstreamConnectV2.Key())
	}
}
