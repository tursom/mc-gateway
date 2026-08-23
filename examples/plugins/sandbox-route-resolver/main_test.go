package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

func TestRouteResolverServiceValidatesConfigAndResolvesRoutes(t *testing.T) {
	service := newService()

	missingConfig := routeResolverInvoke(t, service, sandboxsdk.InvokeRequest{
		ExtensionPoint: "config.validate/v1",
		HandlerID:      "config-main",
		ConfigJSON:     json.RawMessage(`{}`),
	})
	if missingConfig.Invoke == nil || missingConfig.Invoke.Valid == nil || *missingConfig.Invoke.Valid || missingConfig.Invoke.Reason != "upstream is required" {
		t.Fatalf("missing config response = %+v, want upstream-required rejection", missingConfig)
	}
	validConfig := routeResolverInvoke(t, service, sandboxsdk.InvokeRequest{
		ExtensionPoint: "config.validate/v1",
		HandlerID:      "config-main",
		ConfigJSON:     json.RawMessage(`{"upstream":"backend:25565"}`),
	})
	if validConfig.Invoke == nil || validConfig.Invoke.Valid == nil || !*validConfig.Invoke.Valid {
		t.Fatalf("valid config response = %+v, want accepted", validConfig)
	}

	tests := []struct {
		name string
		req  api.RouteResolveRequest
		want api.RouteDecision
	}{
		{
			name: "reject blocked host",
			req:  api.RouteResolveRequest{Host: "reject.example"},
			want: api.RouteDecision{Action: api.RouteDecisionReject, Host: "reject.example", Reason: "blocked by sandbox route example"},
		},
		{
			name: "preserve fallback",
			req:  api.RouteResolveRequest{Host: "known.example", FallbackHit: true, FallbackUpstream: "fallback:25565"},
			want: api.RouteDecision{Action: api.RouteDecisionFallback, Upstream: "fallback:25565", Host: "known.example", Reason: "using fallback"},
		},
		{
			name: "override unknown host",
			req:  api.RouteResolveRequest{Host: "unknown.example"},
			want: api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "sandbox-route:25565", Host: "unknown.example", Reason: "sandbox route override"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := routeResolverInvoke(t, service, sandboxsdk.InvokeRequest{
				ExtensionPoint: "route.resolve/v1",
				HandlerID:      "route-main",
				RouteResolve:   &tt.req,
			})
			if !resp.OK || resp.Invoke == nil || resp.Invoke.RouteDecision == nil || !reflect.DeepEqual(*resp.Invoke.RouteDecision, tt.want) {
				t.Fatalf("route response = %+v, want %+v", resp, tt.want)
			}
		})
	}
}

func routeResolverInvoke(t *testing.T, service sandboxsdk.Service, invoke sandboxsdk.InvokeRequest) sandboxsdk.ControlResponse {
	t.Helper()
	payload, err := json.Marshal(invoke)
	if err != nil {
		t.Fatalf("Marshal(invoke) error = %v", err)
	}
	return service.HandleControlRequest(context.Background(), sandboxsdk.ControlRequest{
		Command:  sandboxsdk.CommandInvoke,
		Protocol: sandboxsdk.Protocol,
		Payload:  payload,
	})
}
