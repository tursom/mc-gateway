package main

import (
	"context"
	"encoding/json"
	"log"
	"os/signal"
	"syscall"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

func main() {
	client, err := sandboxsdk.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := client.Run(ctx, newService()); err != nil {
		log.Fatal(err)
	}
}

func newService() sandboxsdk.Service {
	return sandboxsdk.Service{
		Registrations: []sandboxsdk.HandlerRegistration{
			{
				ExtensionPoint: "route.resolve/v1",
				HandlerID:      "route-main",
				FailPolicy:     api.FailPolicyClose,
				TimeoutMS:      1000,
				SchemaVersion:  1,
			},
			{
				ExtensionPoint: "config.validate/v1",
				HandlerID:      "config-main",
				FailPolicy:     api.FailPolicyClose,
				TimeoutMS:      1000,
				SchemaVersion:  1,
			},
		},
		Capabilities: []string{"runtime.cpu_memory", "runtime.process_restricted"},
		ConfigValidate: func(_ context.Context, raw json.RawMessage) (bool, string, error) {
			var cfg struct {
				Upstream string `json:"upstream"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err.Error(), nil
			}
			if cfg.Upstream == "" {
				return false, "upstream is required", nil
			}
			return true, "route config accepted", nil
		},
		RouteResolve: func(_ context.Context, req api.RouteResolveRequest) (api.RouteDecision, error) {
			if req.Host == "reject.example" {
				return api.RouteDecision{Action: api.RouteDecisionReject, Host: req.Host, Reason: "blocked by sandbox route example"}, nil
			}
			if req.FallbackHit {
				return api.RouteDecision{Action: api.RouteDecisionFallback, Upstream: req.FallbackUpstream, Host: req.Host, Reason: "using fallback"}, nil
			}
			return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "sandbox-route:25565", Host: req.Host, Reason: "sandbox route override"}, nil
		},
	}
}
