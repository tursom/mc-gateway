package main

import (
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

func TestRouteStatusEventAndProviderFixtures(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{
		OverrideHost:      "blue.example",
		OverrideUpstream:  "10.0.0.10:25565",
		RejectHost:        "blocked.example",
		DenySourceCIDR:    "203.0.113.0/24",
		AllowSourceCIDR:   "198.51.100.0/24",
		RewriteHost:       "legacy.example",
		RewriteTarget:     "blue.example",
		UpstreamRewrite:   "10.0.0.20:25565",
		RateLimit:         2,
		RateWindow:        "1m",
		Maintenance:       true,
		MaintenanceWindow: "02:00-03:00 UTC",
		Favicon:           "data:image/png;base64,fixture",
		VersionText:       "mc-gateway",
		OnlinePlayers:     1,
		MaxPlayers:        20,
	}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	allowedConnection, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "198.51.100.10:25565"})
	if err != nil || !allowedConnection.Allow || allowedConnection.Reject {
		t.Fatalf("allowed connection = %+v err=%v, want allow", allowedConnection, err)
	}
	rejectedConnection, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "203.0.113.10:25565"})
	if err != nil || rejectedConnection.Allow || !rejectedConnection.Reject {
		t.Fatalf("rejected connection = %+v err=%v, want CIDR reject", rejectedConnection, err)
	}
	notAllowedConnection, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "192.0.2.10:25565"})
	if err != nil || notAllowedConnection.Allow || !notAllowedConnection.Reject {
		t.Fatalf("not allowed connection = %+v err=%v, want allow CIDR reject", notAllowedConnection, err)
	}
	limited, err := plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "198.51.100.20:25565"})
	if err != nil || !limited.Allow || limited.Reject {
		t.Fatalf("first rate connection = %+v err=%v, want allow", limited, err)
	}
	limited, err = plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "198.51.100.20:25565"})
	if err != nil || !limited.Allow || limited.Reject {
		t.Fatalf("second rate connection = %+v err=%v, want allow", limited, err)
	}
	limited, err = plugin.filterConnection(api.ConnectionFilterRequest{SourceAddr: "198.51.100.20:25565"})
	if err != nil || limited.Allow || !limited.Reject {
		t.Fatalf("third rate connection = %+v err=%v, want rate limit reject", limited, err)
	}
	rewriteHandshake, err := plugin.filterHandshake(api.HandshakeFilterRequest{ServerHost: "LEGACY.EXAMPLE"})
	if err != nil || !rewriteHandshake.Allow || rewriteHandshake.RewriteHost != "blue.example" {
		t.Fatalf("rewrite handshake = %+v err=%v, want host rewrite", rewriteHandshake, err)
	}
	allowedHandshake, err := plugin.filterHandshake(api.HandshakeFilterRequest{ServerHost: "blue.example"})
	if err != nil || !allowedHandshake.Allow || allowedHandshake.RewriteHost != "" {
		t.Fatalf("allowed handshake = %+v err=%v, want allow without rewrite", allowedHandshake, err)
	}

	override, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "BLUE.EXAMPLE"})
	if err != nil || override.Action != api.RouteDecisionOverride || override.Upstream != "10.0.0.10:25565" || override.ProviderID != "extension-ecosystem" {
		t.Fatalf("override route = %+v err=%v, want extension override", override, err)
	}
	if override.CacheTTL <= 0 || override.Metadata["explain"] == "" || override.Explanation == "" {
		t.Fatalf("override route = %+v, want TTL and decision explain", override)
	}
	upstreamRewrite, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "legacy.example"})
	if err != nil || upstreamRewrite.Action != api.RouteDecisionOverride || upstreamRewrite.Upstream != "10.0.0.20:25565" {
		t.Fatalf("upstream rewrite route = %+v err=%v, want upstream rewrite", upstreamRewrite, err)
	}
	reject, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "blocked.example"})
	if err != nil || reject.Action != api.RouteDecisionReject {
		t.Fatalf("reject route = %+v err=%v, want reject", reject, err)
	}
	fallback, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "other.example", FallbackHit: true, FallbackUpstream: "sqlite:25565"})
	if err != nil || fallback.Action != api.RouteDecisionFallback || fallback.Upstream != "sqlite:25565" {
		t.Fatalf("fallback route = %+v err=%v, want sqlite fallback", fallback, err)
	}
	pass, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "other.example"})
	if err != nil || pass.Action != api.RouteDecisionPass {
		t.Fatalf("pass route = %+v err=%v, want pass", pass, err)
	}
	allowedRule, err := plugin.evaluateRule(api.RuleEvaluateRequest{SourceAddr: "198.51.100.30:25565"})
	if err != nil || !allowedRule.Allow || allowedRule.Deny {
		t.Fatalf("allowed rule = %+v err=%v, want allow", allowedRule, err)
	}
	deniedRule, err := plugin.evaluateRule(api.RuleEvaluateRequest{SourceAddr: "203.0.113.30:25565"})
	if err != nil || deniedRule.Allow || !deniedRule.Deny {
		t.Fatalf("denied rule = %+v err=%v, want deny", deniedRule, err)
	}

	if !plugin.acceptStatus(api.StatusPingRequest{Host: "blue.example"}) || plugin.acceptStatus(api.StatusPingRequest{Host: "green.example"}) {
		t.Fatal("acceptStatus() did not match fixture hosts")
	}
	status, err := plugin.statusPing(api.StatusPingRequest{Host: "red.example", ProtocolVersion: 765})
	if err != nil {
		t.Fatalf("statusPing() error = %v", err)
	}
	if status.MOTD != "Maintenance red.example" ||
		status.Favicon != "data:image/png;base64,fixture" ||
		status.OnlinePlayers != 1 ||
		status.MaxPlayers != 20 ||
		status.VersionText != "mc-gateway" ||
		status.ProtocolVersion != 765 ||
		!status.Maintenance ||
		status.MaintenanceWindow != "02:00-03:00 UTC" {
		t.Fatalf("status response = %+v, want maintenance fixture fields", status)
	}

	retry, err := plugin.handleEvent(api.EventDeliveryRequest{Name: "fixture.retry", Attempt: 1})
	if err != nil || retry.OK || !retry.Retry {
		t.Fatalf("retry event = %+v err=%v, want retry request", retry, err)
	}
	delivered, err := plugin.handleEvent(api.EventDeliveryRequest{Name: "fixture.retry", Attempt: 2})
	if err != nil || !delivered.OK || delivered.Retry {
		t.Fatalf("delivered event = %+v err=%v, want success after retry", delivered, err)
	}

	provider, err := plugin.adminAuthProvider()
	if err != nil {
		t.Fatalf("adminAuthProvider() error = %v", err)
	}
	if provider.Type != api.HookAdminAuthProvider.Key() || provider.Name != "external-identity" || provider.Priority != 100 || !provider.Fallback || len(provider.Dependencies) != 1 || provider.Dependencies[0] != "local-admin-break-glass" || provider.Metadata["break_glass"] != "local-admin" {
		t.Fatalf("provider = %+v, want break-glass admin auth provider", provider)
	}
}
