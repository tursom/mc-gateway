package rulepolicy

import (
	"context"
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

func TestRulePolicyFiltersRewritesAndStatus(t *testing.T) {
	plugin := New()
	if err := plugin.ReloadConfig(&Config{
		HostRewrite:     map[string]string{"play.example": "internal.example"},
		UpstreamRewrite: map[string]string{"play.example": "backend:25565"},
		SourceAllowCIDR: []string{"192.168.0.0/16"},
		SourceDenyCIDR:  []string{"10.0.0.0/8"},
		RateLimit:       RateLimitConfig{Requests: 1, Window: "1m"},
		Maintenance: MaintenanceConfig{
			Enabled:       true,
			Hosts:         []string{"play.example"},
			MOTD:          "Maintenance soon",
			Favicon:       "data:image/png;base64,fixture",
			OnlinePlayers: 3,
			MaxPlayers:    100,
			Version:       "1.20.4",
			Window:        "02:00-03:00 UTC",
			StatusByHost:  map[string]string{"play.example": "Host maintenance"},
		},
	}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	denied, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "10.1.2.3:25565"})
	if err != nil || !denied.Reject || denied.Allow {
		t.Fatalf("deny CIDR decision = %+v err=%v, want reject", denied, err)
	}
	notAllowed, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "172.16.0.10:25565"})
	if err != nil || !notAllowed.Reject {
		t.Fatalf("allow CIDR decision = %+v err=%v, want reject", notAllowed, err)
	}
	allowed, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "192.168.1.10:25565"})
	if err != nil || !allowed.Allow || allowed.Reject {
		t.Fatalf("first rate decision = %+v err=%v, want allow", allowed, err)
	}
	limited, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "192.168.1.10:25565"})
	if err != nil || !limited.Reject {
		t.Fatalf("second rate decision = %+v err=%v, want rate limit reject", limited, err)
	}

	handshake, err := plugin.filterHandshake(api.HandshakeFilterRequest{ServerHost: "PLAY.EXAMPLE"})
	if err != nil || handshake.RewriteHost != "internal.example" || !handshake.Allow {
		t.Fatalf("handshake decision = %+v err=%v, want host rewrite", handshake, err)
	}
	route, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "PLAY.EXAMPLE"})
	if err != nil || route.Action != api.RouteDecisionOverride || route.Upstream != "backend:25565" {
		t.Fatalf("route decision = %+v err=%v, want upstream rewrite", route, err)
	}
	if !plugin.acceptStatus(api.StatusPingRequest{Host: "play.example"}) || plugin.acceptStatus(api.StatusPingRequest{Host: "other.example"}) {
		t.Fatal("acceptStatus host filtering did not match maintenance hosts")
	}
	status, err := plugin.statusPing(api.StatusPingRequest{Host: "play.example", ProtocolVersion: 765})
	if err != nil {
		t.Fatalf("statusPing() error = %v", err)
	}
	if status.MOTD != "Host maintenance" ||
		status.Favicon != "data:image/png;base64,fixture" ||
		status.OnlinePlayers != 3 ||
		status.MaxPlayers != 100 ||
		status.VersionText != "1.20.4" ||
		status.ProtocolVersion != 765 ||
		!status.Maintenance ||
		status.MaintenanceWindow != "02:00-03:00 UTC" {
		t.Fatalf("status response = %+v, want configured maintenance status", status)
	}
}

func TestRulePolicyPublicLifecycleRegistersHooks(t *testing.T) {
	plugin := New()
	if _, ok := plugin.NewConfigObj().(*Config); !ok {
		t.Fatalf("NewConfigObj() = %T, want *Config", plugin.NewConfigObj())
	}
	gateway := &recordingGateway{hooks: map[string]any{}}
	if err := plugin.Init(gateway); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	for _, hook := range []string{
		api.HookConnectionFilter.Key(),
		api.HookHandshakeFilter.Key(),
		api.HookRouteResolve.Key(),
		api.HookRuleEvaluate.Key(),
		api.HookStatusPing.Key(),
	} {
		if gateway.hooks[hook] == nil {
			t.Fatalf("Init() did not register %s", hook)
		}
	}
}
