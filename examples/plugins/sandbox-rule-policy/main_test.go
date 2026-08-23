package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

func TestRulePolicyServiceValidatesConfigAndEvaluatesRules(t *testing.T) {
	service := newService()

	configResp := rulePolicyInvoke(t, service, sandboxsdk.InvokeRequest{
		ExtensionPoint: "config.validate/v1",
		HandlerID:      "config-main",
		ConfigJSON:     json.RawMessage(`{"denied_subjects":["blocked"]}`),
	})
	if !configResp.OK || configResp.Invoke == nil || configResp.Invoke.Valid == nil || !*configResp.Invoke.Valid || configResp.Invoke.Reason != "rule config accepted" {
		t.Fatalf("config response = %+v, want accepted", configResp)
	}

	tests := []struct {
		name      string
		req       api.RuleEvaluateRequest
		wantAllow bool
		wantDeny  bool
	}{
		{name: "blocked subject", req: api.RuleEvaluateRequest{Subject: "blocked", Action: "connect"}, wantDeny: true},
		{name: "denied action", req: api.RuleEvaluateRequest{Subject: "player", Action: "deny"}, wantDeny: true},
		{name: "ordinary request", req: api.RuleEvaluateRequest{Subject: "player", Action: "connect"}, wantAllow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := rulePolicyInvoke(t, service, sandboxsdk.InvokeRequest{
				ExtensionPoint: "rule.evaluate/v1",
				HandlerID:      "rule-main",
				RuleEvaluate:   &tt.req,
			})
			if !resp.OK || resp.Invoke == nil || resp.Invoke.RuleDecision == nil {
				t.Fatalf("rule response = %+v, want decision", resp)
			}
			decision := resp.Invoke.RuleDecision
			if decision.Allow != tt.wantAllow || decision.Deny != tt.wantDeny || decision.Reject != tt.wantDeny {
				t.Fatalf("rule decision = %+v, want allow=%v deny=%v reject=%v", decision, tt.wantAllow, tt.wantDeny, tt.wantDeny)
			}
		})
	}
}

func rulePolicyInvoke(t *testing.T, service sandboxsdk.Service, invoke sandboxsdk.InvokeRequest) sandboxsdk.ControlResponse {
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
