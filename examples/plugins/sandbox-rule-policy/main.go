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
	service := sandboxsdk.Service{
		Registrations: []sandboxsdk.HandlerRegistration{
			{
				ExtensionPoint: "rule.evaluate/v1",
				HandlerID:      "rule-main",
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
				DeniedSubjects []string `json:"denied_subjects"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err.Error(), nil
			}
			return true, "rule config accepted", nil
		},
		RuleEvaluate: func(_ context.Context, req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
			if req.Subject == "blocked" || req.Action == "deny" {
				return api.RuleEvaluateDecision{Deny: true, Reject: true, Reason: "blocked by sandbox rule example"}, nil
			}
			return api.RuleEvaluateDecision{Allow: true, Reason: "allowed by sandbox rule example"}, nil
		},
	}
	if err := client.Run(ctx, service); err != nil {
		log.Fatal(err)
	}
}
