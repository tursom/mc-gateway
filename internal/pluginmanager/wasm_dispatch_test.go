package pluginmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestWASMDispatchRuleEvaluateUsesWASM(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "deny",
		Reason:         "blocked by wasm",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-rule", module, []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-rule", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(wasm-rule) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-rule"); err != nil {
		t.Fatalf("Enable(wasm-rule) error = %v", err)
	}

	result, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{
		Subject:  "player-a",
		Action:   "join",
		Resource: "server-a",
		Host:     "play.example",
	})
	if err != nil {
		t.Fatalf("EvaluateRule() error = %v", err)
	}
	if !result.Handled || result.PluginID != "wasm-rule" || !result.Decision.Deny || result.Decision.Allow || result.Decision.Reason != "blocked by wasm" {
		t.Fatalf("EvaluateRule() = %+v, want wasm deny decision", result)
	}
}

func TestWASMDispatchRuleEvaluateAllowDenyAndError(t *testing.T) {
	_, _, plugin := startWASMLifecyclePlugin(t, "wasm-rule-matrix", `{}`)
	tests := []struct {
		name      string
		resp      WASMABIResponse
		err       error
		wantAllow bool
		wantDeny  bool
		wantErr   string
	}{
		{
			name: "allow",
			resp: WASMABIResponse{
				ExtensionPoint: ExtensionRuleEvaluate,
				OK:             true,
				Decision:       "allow",
				Reason:         "allow from wasm",
			},
			wantAllow: true,
		},
		{
			name: "deny",
			resp: WASMABIResponse{
				ExtensionPoint: ExtensionRuleEvaluate,
				OK:             true,
				Decision:       "deny",
				Reason:         "deny from wasm",
			},
			wantDeny: true,
		},
		{
			name:    "error",
			err:     newWASMABIError(wasmABIErrorTrap, ExtensionRuleEvaluate, "rule trap"),
			wantErr: wasmABIErrorTrap,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
				if point != ExtensionRuleEvaluate {
					return WASMABIResponse{}, nil, false
				}
				return tt.resp, tt.err, true
			}
			decision, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
			if tt.wantErr != "" {
				if got := wasmABIErrorCode(err); got != tt.wantErr {
					t.Fatalf("evaluateRule() error = %v code=%q, want %s", err, got, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluateRule() error = %v", err)
			}
			if decision.Allow != tt.wantAllow || decision.Deny != tt.wantDeny || decision.Reject != tt.wantDeny || decision.ProviderID != "wasm-rule-matrix" {
				t.Fatalf("evaluateRule() = %+v, want allow=%v deny=%v provider", decision, tt.wantAllow, tt.wantDeny)
			}
		})
	}
}

func TestWASMDispatchRouteResolveUsesWASMAndDisableRemoves(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRouteResolveV1, WASMABIResponse{
		ExtensionPoint: ExtensionRouteResolve,
		OK:             true,
		Decision:       "override",
		Route:          "wasm-upstream:25565",
		Reason:         "resolved by wasm",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-route", module, []ExtensionPoint{{Type: "provider", Key: ExtensionRouteResolve}})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-route", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(wasm-route) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-route"); err != nil {
		t.Fatalf("Enable(wasm-route) error = %v", err)
	}

	result, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example"}, nil)
	if err != nil {
		t.Fatalf("ResolveRoute() error = %v", err)
	}
	if result.Decision.Action != api.RouteDecisionOverride || result.Decision.Upstream != "wasm-upstream:25565" || result.Decision.ProviderID != "wasm-route" {
		t.Fatalf("ResolveRoute() = %+v, want wasm override route", result)
	}
	if !dispatchHasRoutePlugin(manager, "wasm-route") {
		t.Fatalf("route dispatch missing wasm-route after enable")
	}

	if _, err := manager.Disable(context.Background(), "admin", "wasm-route"); err != nil {
		t.Fatalf("Disable(wasm-route) error = %v", err)
	}
	if dispatchHasRoutePlugin(manager, "wasm-route") {
		t.Fatalf("route dispatch still contains wasm-route after disable")
	}
}

func TestWASMDispatchRouteResolveFallbackRejectAndError(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRouteResolveV1, WASMABIResponse{
		ExtensionPoint: ExtensionRouteResolve,
		OK:             true,
		Decision:       "use_default",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-route-matrix", module, []ExtensionPoint{{Type: "provider", Key: ExtensionRouteResolve}})
	adapter := WASMAdapter{Runner: NewWASMRunner(), Mode: PluginServiceModeInProcess}
	prepared, err := adapter.Prepare(context.Background(), artifact, PluginRecord{ID: "wasm-route-matrix"})
	if err != nil {
		t.Fatalf("Prepare(wasm-route-matrix) error = %v", err)
	}
	instance, err := adapter.Start(context.Background(), prepared, artifact, PluginRecord{ID: "wasm-route-matrix", ConfigJSON: `{}`}, nil)
	if err != nil {
		t.Fatalf("Start(wasm-route-matrix) error = %v", err)
	}
	plugin := instance.Plugin.(*wasmHostedPlugin)
	tests := []struct {
		name       string
		resp       WASMABIResponse
		err        error
		wantAction string
		wantErr    error
	}{
		{
			name: "fallback",
			resp: WASMABIResponse{
				ExtensionPoint: ExtensionRouteResolve,
				OK:             true,
				Decision:       "use_default",
				Reason:         "default route",
			},
			wantAction: api.RouteDecisionPass,
		},
		{
			name: "reject",
			resp: WASMABIResponse{
				ExtensionPoint: ExtensionRouteResolve,
				OK:             true,
				Decision:       "reject",
				Reason:         "reject route",
			},
			wantAction: api.RouteDecisionReject,
		},
		{
			name:    "error",
			err:     newWASMABIError(wasmABIErrorTrap, ExtensionRouteResolve, "route trap"),
			wantErr: api.ErrPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
				if point != ExtensionRouteResolve {
					return WASMABIResponse{}, nil, false
				}
				return tt.resp, tt.err, true
			}
			decision, err := plugin.resolveRoute(api.RouteResolveRequest{Host: "play.example", Context: context.Background()})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveRoute() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRoute() error = %v", err)
			}
			if decision.Action != tt.wantAction || decision.ProviderID != "wasm-route-matrix" {
				t.Fatalf("resolveRoute() = %+v, want action %s provider", decision, tt.wantAction)
			}
		})
	}
}

func TestWASMDispatchFailureContainmentAllowsNextCall(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		counterKey string
	}{
		{name: "timeout", err: newWASMABIError(wasmABIErrorTimeout, ExtensionRuleEvaluate, "deadline"), counterKey: "timeout_count"},
		{name: "trap", err: newWASMABIError(wasmABIErrorTrap, ExtensionRuleEvaluate, "trap"), counterKey: "trap_count"},
		{name: "memory exceeded", err: newWASMABIError(wasmABIErrorMemoryExceeded, ExtensionRuleEvaluate, "memory limit exceeded"), counterKey: "memory_error_count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, plugin := startWASMLifecyclePlugin(t, "wasm-containment-"+strings.ReplaceAll(tt.name, " ", "-"), `{}`)
			plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
				if point != ExtensionRuleEvaluate {
					return WASMABIResponse{}, nil, false
				}
				return WASMABIResponse{}, tt.err, true
			}
			if _, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()}); wasmABIErrorCode(err) != wasmABIErrorCode(tt.err) {
				t.Fatalf("evaluateRule(first) error = %v code=%q, want %s", err, wasmABIErrorCode(err), wasmABIErrorCode(tt.err))
			}

			plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
				if point != ExtensionRuleEvaluate {
					return WASMABIResponse{}, nil, false
				}
				return WASMABIResponse{ExtensionPoint: ExtensionRuleEvaluate, OK: true, Decision: "allow"}, nil, true
			}
			decision, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
			if err != nil || !decision.Allow {
				t.Fatalf("evaluateRule(next) = %+v err=%v, want successful allow after %s", decision, err, tt.name)
			}
			summary := plugin.diagnosticsSummary()
			if summary[tt.counterKey] != int64(1) || summary["last_dispatch_ok"] != true || summary["last_error"] != "" {
				t.Fatalf("diagnostics after recovery = %+v, want %s=1 and last success", summary, tt.counterKey)
			}
		})
	}
}

func TestWASMDispatchConfigValidateBlocksBadConfig(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	valid := false
	module := wasmStaticResponseModule(t, wasmExportConfigValidateV1, WASMABIResponse{
		ExtensionPoint: ExtensionConfigValidate,
		OK:             true,
		Valid:          &valid,
		Reason:         "bad config from wasm",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-config", module, []ExtensionPoint{{Type: "validator", Key: ExtensionConfigValidate}})

	if result, err := manager.DryRunConfig(context.Background(), "wasm-config", artifact.ID, `{}`); err == nil || result.OK || !strings.Contains(err.Error(), "bad config from wasm") {
		t.Fatalf("DryRunConfig(bad wasm config) = %+v err=%v, want blocking validation error", result, err)
	}

	if _, err := manager.repo.UpsertDesired(context.Background(), "admin", "wasm-config", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("UpsertDesired(wasm-config) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-config"); err == nil || !strings.Contains(err.Error(), "bad config from wasm") {
		t.Fatalf("Enable(wasm-config) error = %v, want blocking validation error", err)
	}
}

func TestWASMDispatchConfigValidateAcceptsGoodConfig(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	valid := true
	module := wasmStaticResponseModule(t, wasmExportConfigValidateV1, WASMABIResponse{
		ExtensionPoint: ExtensionConfigValidate,
		OK:             true,
		Valid:          &valid,
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-config-ok", module, []ExtensionPoint{{Type: "validator", Key: ExtensionConfigValidate}})

	if result, err := manager.DryRunConfig(context.Background(), "wasm-config-ok", artifact.ID, `{"enabled":true}`); err != nil || !result.OK {
		t.Fatalf("DryRunConfig(good wasm config) = %+v err=%v, want ok", result, err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-config-ok", artifact.ID, DesiredEnabled, `{"enabled":true}`, 10); err != nil {
		t.Fatalf("SetDesired(wasm-config-ok) error = %v", err)
	}
}

func TestWASMPrepareRejectsInvalidLimits(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	adapter := WASMAdapter{Mode: PluginServiceModeInProcess}
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{
			name: "timeout too low",
			mutate: func(manifest *Manifest) {
				manifest.RuntimeLimits.HandlerTimeoutMS = wasmMinHandlerTimeoutMS - 1
			},
			want: "handler_timeout_ms",
		},
		{
			name: "timeout too high",
			mutate: func(manifest *Manifest) {
				manifest.RuntimeLimits.HandlerTimeoutMS = wasmMaxHandlerTimeoutMS + 1
			},
			want: "handler_timeout_ms",
		},
		{
			name: "memory too low",
			mutate: func(manifest *Manifest) {
				manifest.RuntimeLimits.MemoryBytes = wasmMinMemoryBytes - 1
			},
			want: "memory_bytes",
		},
		{
			name: "memory too high",
			mutate: func(manifest *Manifest) {
				manifest.RuntimeLimits.MemoryBytes = wasmMaxMemoryBytes + 1
			},
			want: "memory_bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := uploadWASMDispatchArtifact(t, manager, strings.ReplaceAll(tt.name, " ", "-"), wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
				ExtensionPoint: ExtensionRuleEvaluate,
				OK:             true,
				Decision:       "allow",
			}), []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
			var manifest Manifest
			if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
				t.Fatalf("Unmarshal manifest error = %v", err)
			}
			tt.mutate(&manifest)
			artifact.MetadataJSON = mustJSON(t, manifest)
			if _, err := adapter.Prepare(context.Background(), artifact, PluginRecord{ID: artifact.PluginID}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prepare(%s) error = %v, want %q", tt.name, err, tt.want)
			}
		})
	}
}

func TestWASMDispatchOutputLimitRejectsOversizedResponse(t *testing.T) {
	manifest := wasmTestManifest([]ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	manifest.RuntimeLimits.HandlerTimeoutMS = 100
	manifest.RuntimeLimits.MemoryBytes = 64 * 1024
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
		Reason:         strings.Repeat("x", 64),
	})
	_, err := NewWASMRunner().Dispatch(context.Background(), WASMDispatchInvocation{
		ArtifactID:     "wasm-output-limit",
		PluginID:       "wasm-output-limit",
		Module:         module,
		Manifest:       manifest,
		ExtensionPoint: ExtensionRuleEvaluate,
		Request:        WASMABIRequest{RuleContext: json.RawMessage(`{"action":"join"}`)},
		Timeout:        100 * time.Millisecond,
		MemoryBytes:    64 * 1024,
		MaxOutputBytes: 32,
	})
	if got := wasmABIErrorCode(err); got != wasmABIErrorOversizedOutput {
		t.Fatalf("Dispatch(output limit) error = %v code=%q, want %s", err, got, wasmABIErrorOversizedOutput)
	}
}

func TestWASMReloadConfigDoesNotPolluteInFlightDispatch(t *testing.T) {
	adapter, instance, plugin := startWASMLifecyclePlugin(t, "wasm-reload", `{"version":"old"}`)
	entered := make(chan WASMABIRequest, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, req WASMABIRequest) (WASMABIResponse, error, bool) {
		if point != ExtensionRuleEvaluate {
			return WASMABIResponse{}, nil, false
		}
		entered <- req
		<-release
		return WASMABIResponse{
			ExtensionPoint: ExtensionRuleEvaluate,
			OK:             true,
			Decision:       "allow",
		}, nil, true
	}
	go func() {
		_, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
		done <- err
	}()
	first := <-entered
	if string(first.Config) != `{"version":"old"}` {
		t.Fatalf("in-flight config = %s, want old snapshot", first.Config)
	}
	if err := adapter.ReloadConfig(context.Background(), instance, `{"version":"new"}`); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight evaluateRule() error = %v", err)
	}

	nextReq := make(chan WASMABIRequest, 1)
	plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, req WASMABIRequest) (WASMABIResponse, error, bool) {
		if point != ExtensionRuleEvaluate {
			return WASMABIResponse{}, nil, false
		}
		nextReq <- req
		return WASMABIResponse{
			ExtensionPoint: ExtensionRuleEvaluate,
			OK:             true,
			Decision:       "allow",
		}, nil, true
	}
	if _, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()}); err != nil {
		t.Fatalf("evaluateRule(after reload) error = %v", err)
	}
	if got := <-nextReq; string(got.Config) != `{"version":"new"}` {
		t.Fatalf("post-reload config = %s, want new snapshot", got.Config)
	}
}

func TestWASMRollbackConfigSnapshotRestoresPreviousArtifactAndConfig(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	oldArtifact := uploadWASMDispatchArtifact(t, manager, "wasm-rollback", wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
		Reason:         "old",
	}), []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	newArtifact := uploadWASMDispatchArtifact(t, manager, "wasm-rollback", wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "deny",
		Reason:         "new",
	}), []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-rollback", oldArtifact.ID, DesiredEnabled, `{"version":"old"}`, 10); err != nil {
		t.Fatalf("SetDesired(old wasm) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-rollback", newArtifact.ID, DesiredEnabled, `{"version":"new"}`, 20); err != nil {
		t.Fatalf("SetDesired(new wasm) error = %v", err)
	}
	snapshots, err := manager.ListConfigSnapshots(context.Background(), "wasm-rollback")
	if err != nil {
		t.Fatalf("ListConfigSnapshots(wasm-rollback) error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	rolledBack, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, true)
	if err != nil {
		t.Fatalf("RollbackConfigSnapshot(wasm full desired) error = %v", err)
	}
	if rolledBack.DesiredArtifactID != oldArtifact.ID ||
		rolledBack.ConfigJSON != `{"version":"old"}` ||
		rolledBack.DesiredState != DesiredEnabled ||
		rolledBack.Priority != 10 {
		t.Fatalf("rolled back wasm plugin = %+v, want old artifact/config/state/priority", rolledBack)
	}
}

func TestWASMDrainRejectsNewDispatchAndWaitsForActiveCalls(t *testing.T) {
	adapter, instance, plugin := startWASMLifecyclePlugin(t, "wasm-drain", `{}`)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	activeDone := make(chan error, 1)
	plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
		if point != ExtensionRuleEvaluate {
			return WASMABIResponse{}, nil, false
		}
		entered <- struct{}{}
		<-release
		return WASMABIResponse{
			ExtensionPoint: ExtensionRuleEvaluate,
			OK:             true,
			Decision:       "allow",
		}, nil, true
	}
	go func() {
		_, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
		activeDone <- err
	}()
	<-entered
	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drainDone <- adapter.Drain(ctx, instance)
	}()
	waitForWASMState(t, plugin, RuntimeDraining)
	if _, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()}); err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("evaluateRule(while draining) error = %v, want draining refusal", err)
	}
	if active := adapter.Diagnostics(context.Background(), instance).Details["active_calls"]; active != 1 {
		t.Fatalf("active_calls while draining = %v, want 1", active)
	}
	close(release)
	if err := <-activeDone; err != nil {
		t.Fatalf("active evaluateRule() error = %v", err)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if state := adapter.Diagnostics(context.Background(), instance).State; state != RuntimeDraining {
		t.Fatalf("diagnostics state after Drain = %s, want draining", state)
	}
}

func TestWASMStopReleasesRuntimeAndDiagnosticsReflectStopped(t *testing.T) {
	adapter, instance, plugin := startWASMLifecyclePlugin(t, "wasm-stop", `{}`)
	plugin.recordDispatch(ExtensionRuleEvaluate, newWASMABIError(wasmABIErrorTrap, ExtensionRuleEvaluate, "trap"))
	plugin.recordDispatch(ExtensionRuleEvaluate, newWASMABIError(wasmABIErrorTimeout, ExtensionRuleEvaluate, "timeout"))
	plugin.recordDispatch(ExtensionRuleEvaluate, newWASMABIError(wasmABIErrorMemoryExceeded, ExtensionRuleEvaluate, "memory"))
	if err := adapter.Stop(context.Background(), instance); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	plugin.mu.Lock()
	moduleReleased := plugin.module == nil
	runnerReleased := plugin.runner == nil
	plugin.mu.Unlock()
	if !moduleReleased || !runnerReleased {
		t.Fatalf("Stop() released module=%v runner=%v, want both true", moduleReleased, runnerReleased)
	}
	if _, err := plugin.evaluateRule(api.RuleEvaluateRequest{Action: "join", Context: context.Background()}); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("evaluateRule(after Stop) error = %v, want stopped refusal", err)
	}
	health := adapter.HealthCheck(context.Background(), instance)
	if health.OK || health.Status != RuntimeStopped {
		t.Fatalf("HealthCheck(after Stop) = %+v, want stopped unhealthy", health)
	}
	diag := adapter.Diagnostics(context.Background(), instance)
	if diag.State != RuntimeStopped {
		t.Fatalf("Diagnostics(after Stop).State = %s, want stopped", diag.State)
	}
	details := diag.Details
	for _, key := range []string{"host_abi", "module_hash", "module_cache_status", "last_error", "trap_count", "timeout_count", "memory_error_count", "active_calls"} {
		if _, ok := details[key]; !ok {
			t.Fatalf("Diagnostics details missing %q: %+v", key, details)
		}
	}
	if details["host_abi"] != wasmHostABIV1 || details["module_hash"] == "" || details["module_cache_status"] == "" || details["active_calls"] != 0 {
		t.Fatalf("Diagnostics details = %+v, want ABI/hash/cache status/zero active calls", details)
	}
	if details["trap_count"] != int64(1) || details["timeout_count"] != int64(1) || details["memory_error_count"] != int64(1) {
		t.Fatalf("Diagnostics counters = trap %v timeout %v memory %v, want 1/1/1", details["trap_count"], details["timeout_count"], details["memory_error_count"])
	}
	if details["runtime_module_loaded"] != false || details["runtime_runner_loaded"] != false {
		t.Fatalf("Diagnostics release flags = %+v, want module/runner released", details)
	}
}

func TestWASMOperationsDiagnosticsMetricsAuditAndRedaction(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
	})
	artifact := uploadTestArtifactWithManifestBytes(t, manager, "wasm-observe", module, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.Runtime.ABI = wasmHostABIV1
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
		manifest.RuntimeLimits.MemoryBytes = 64 * 1024
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}}
		manifest.Capabilities = json.RawMessage(`{}`)
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true}}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-observe", artifact.ID, DesiredEnabled, `{"token":"plain-secret"}`, 10); err != nil {
		t.Fatalf("SetDesired(wasm-observe) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-observe"); err != nil {
		t.Fatalf("Enable(wasm-observe) error = %v", err)
	}
	manager.mu.Lock()
	plugin := manager.loaded["wasm-observe"].instance.(*wasmHostedPlugin)
	plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorTrap, point, "trap saw plain-secret"), true
	}
	manager.mu.Unlock()

	_, _ = manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
	snapshot, err := manager.OperationsSnapshot(context.Background(), "wasm-observe")
	if err != nil {
		t.Fatalf("OperationsSnapshot(wasm-observe) error = %v", err)
	}
	if !hasCustomMetric(snapshot.CustomMetrics, "wasm.call_count") ||
		!hasCustomMetric(snapshot.CustomMetrics, "wasm.duration_sum_ms") ||
		!hasCustomMetric(snapshot.CustomMetrics, "wasm.trap_count") ||
		!hasCustomMetric(snapshot.CustomMetrics, "wasm.active_calls") {
		t.Fatalf("custom metrics = %+v, want wasm runtime metrics", snapshot.CustomMetrics)
	}
	data, summary, err := manager.DiagnosticPackage(context.Background(), "admin", "wasm-observe")
	if err != nil {
		t.Fatalf("DiagnosticPackage(wasm-observe) error = %v", err)
	}
	if !containsString(summary.Sections, "wasm_runtime") {
		t.Fatalf("diagnostic sections = %+v, want wasm_runtime", summary.Sections)
	}
	if bytes.Contains(data, []byte("plain-secret")) {
		t.Fatalf("diagnostic leaked config secret: %s", data)
	}
	if !bytes.Contains(data, []byte(`"wasm_runtime"`)) ||
		!bytes.Contains(data, []byte(`"recent_failures"`)) ||
		!bytes.Contains(data, []byte("[REDACTED]")) {
		t.Fatalf("diagnostic missing wasm runtime redacted failure: %s", data)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "wasm-observe", 20)
	if err != nil {
		t.Fatalf("ListOperations(wasm-observe) error = %v", err)
	}
	if !hasOperation(ops, "enable", "succeeded") {
		t.Fatalf("operations = %+v, want enable audit event", ops)
	}
}

func TestWASMReloadAuditIncludesRuntimeMetadata(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-reload-audit", module, []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-reload-audit", artifact.ID, DesiredEnabled, `{"version":"old"}`, 10); err != nil {
		t.Fatalf("SetDesired(initial) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-reload-audit"); err != nil {
		t.Fatalf("Enable(wasm-reload-audit) error = %v", err)
	}
	updated, err := manager.SetDesired(context.Background(), "admin", "wasm-reload-audit", artifact.ID, DesiredEnabled, `{"version":"new"}`, 10)
	if err != nil {
		t.Fatalf("SetDesired(reload) error = %v", err)
	}
	if updated.AppliedGeneration != updated.DesiredGeneration {
		t.Fatalf("plugin generation after reload = applied %d desired %d, want synced", updated.AppliedGeneration, updated.DesiredGeneration)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "wasm-reload-audit", 20)
	if err != nil {
		t.Fatalf("ListOperations(wasm-reload-audit) error = %v", err)
	}
	record, ok := findOperation(ops, "reload", "succeeded")
	if !ok {
		t.Fatalf("operations = %+v, want reload succeeded audit", ops)
	}
	metadata := jsonMap(record.MetadataJSON)
	if metadata["runtime_type"] != RuntimeWASM ||
		metadata["runtime_abi"] != wasmHostABIV1 ||
		metadata["host_abi"] != wasmHostABIV1 ||
		metadata["module_hash"] == "" ||
		metadata["reload_reason"] != "config_update" ||
		metadata["active_changed"] != true {
		t.Fatalf("reload metadata = %+v, want wasm ABI/module reload summary", metadata)
	}
	if _, ok := metadata["last_error"]; !ok {
		t.Fatalf("reload metadata = %+v, want last_error field", metadata)
	}
	if limits := jsonMapFromAny(metadata["limits"]); limits == nil || limits["memory_bytes"] == nil || limits["handler_timeout_ms"] == nil {
		t.Fatalf("reload limits metadata = %+v", metadata["limits"])
	}
	extensions, ok := metadata["supported_extensions"].([]any)
	if !ok || len(extensions) == 0 || extensions[0] != ExtensionRuleEvaluate {
		t.Fatalf("reload supported_extensions = %#v, want rule extension", metadata["supported_extensions"])
	}
}

func TestWASMABIMismatchUploadFailureAuditIncludesRuntimeMetadata(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json":  testWASMManifestBytes(t, "wasm-abi-upload", "mc-gateway.wasm.host/v0"),
		RuntimeWASMEntry: wasmOKModule,
	})
	_, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "wasm-abi-upload.mcgp",
		Actor:      "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.abi") {
		t.Fatalf("UploadArtifact(abi mismatch) error = %v, want runtime.abi rejection", err)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "wasm-abi-upload", 10)
	if err != nil {
		t.Fatalf("ListOperations(wasm-abi-upload) error = %v", err)
	}
	record, ok := findOperation(ops, "artifact_upload", "failed")
	if !ok {
		t.Fatalf("operations = %+v, want artifact_upload failed audit", ops)
	}
	moduleHash := wasmModuleHash(wasmOKModule)
	if record.PluginID != "wasm-abi-upload" || record.ArtifactID != moduleHash {
		t.Fatalf("operation identity = plugin %q artifact %q, want parsed plugin and module hash %s", record.PluginID, record.ArtifactID, moduleHash)
	}
	metadata := jsonMap(record.MetadataJSON)
	if metadata["runtime_type"] != RuntimeWASM ||
		metadata["runtime_abi"] != "mc-gateway.wasm.host/v0" ||
		metadata["host_abi"] != wasmHostABIV1 ||
		metadata["abi_mismatch"] != true ||
		metadata["module_sha256"] != moduleHash ||
		metadata["module_hash"] != moduleHash ||
		metadata["entry"] != RuntimeWASMEntry ||
		metadata["plugin_id"] != "wasm-abi-upload" ||
		metadata["version"] != "0.1.0" ||
		metadata["package_sha256"] == "" {
		t.Fatalf("artifact_upload metadata = %+v, want wasm ABI mismatch package/module summary", metadata)
	}
	if limits := jsonMapFromAny(metadata["limits"]); limits == nil || limits["memory_bytes"] == nil || limits["handler_timeout_ms"] == nil {
		t.Fatalf("artifact_upload limits metadata = %+v", metadata["limits"])
	}
}

func TestWASMRepeatedTrapTriggersQuarantineAudit(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, "wasm-quarantine", module, []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	if _, err := manager.SetDesired(context.Background(), "admin", "wasm-quarantine", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(wasm-quarantine) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "wasm-quarantine"); err != nil {
		t.Fatalf("Enable(wasm-quarantine) error = %v", err)
	}
	manager.mu.Lock()
	plugin := manager.loaded["wasm-quarantine"].instance.(*wasmHostedPlugin)
	plugin.dispatchHook = func(_ context.Context, _ wasmRuntimeSnapshot, point string, _ WASMABIRequest) (WASMABIResponse, error, bool) {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorTrap, point, "trap"), true
	}
	manager.mu.Unlock()

	for i := 0; i < wasmTrapQuarantineThreshold; i++ {
		_, _ = manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{Action: "join", Context: context.Background()})
	}
	record, err := manager.Plugin(context.Background(), "wasm-quarantine")
	if err != nil {
		t.Fatalf("Plugin(wasm-quarantine) error = %v", err)
	}
	if record.RuntimeState != RuntimeDraining || !strings.Contains(record.LastError, "repeated trap quarantine") {
		t.Fatalf("plugin after traps = %+v, want draining quarantine", record)
	}
	if dispatchHasRulePlugin(manager, "wasm-quarantine") {
		t.Fatalf("rule dispatch still contains wasm-quarantine after quarantine")
	}
	ops, err := manager.repo.ListOperations(context.Background(), "wasm-quarantine", 20)
	if err != nil {
		t.Fatalf("ListOperations(wasm-quarantine) error = %v", err)
	}
	if !hasOperation(ops, "wasm_repeated_trap_quarantine", "warning") {
		t.Fatalf("operations = %+v, want repeated trap quarantine audit", ops)
	}
}

func uploadWASMDispatchArtifact(t *testing.T, manager *Manager, pluginID string, module []byte, points []ExtensionPoint) ArtifactRecord {
	t.Helper()
	return uploadTestArtifactWithManifestBytes(t, manager, pluginID, module, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.Runtime.ABI = wasmHostABIV1
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
		manifest.RuntimeLimits.MemoryBytes = 64 * 1024
		manifest.ExtensionPoints = points
		manifest.Capabilities = json.RawMessage(`{}`)
	})
}

func startWASMLifecyclePlugin(t *testing.T, pluginID, configJSON string) (WASMAdapter, RuntimeInstance, *wasmHostedPlugin) {
	t.Helper()
	manager := newManagerForTestWithBuildersProfile(t, nil, nil, PolicyProfileDev)
	module := wasmStaticResponseModule(t, wasmExportRuleEvaluateV1, WASMABIResponse{
		ExtensionPoint: ExtensionRuleEvaluate,
		OK:             true,
		Decision:       "allow",
	})
	artifact := uploadWASMDispatchArtifact(t, manager, pluginID, module, []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	adapter := WASMAdapter{Runner: NewWASMRunner(), Mode: PluginServiceModeInProcess}
	record := PluginRecord{ID: pluginID, ConfigJSON: configJSON, DesiredGeneration: 7}
	prepared, err := adapter.Prepare(context.Background(), artifact, record)
	if err != nil {
		t.Fatalf("Prepare(%s) error = %v", pluginID, err)
	}
	instance, err := adapter.Start(context.Background(), prepared, artifact, record, nil)
	if err != nil {
		t.Fatalf("Start(%s) error = %v", pluginID, err)
	}
	plugin, ok := instance.Plugin.(*wasmHostedPlugin)
	if !ok || plugin == nil {
		t.Fatalf("Start(%s) plugin = %T, want *wasmHostedPlugin", pluginID, instance.Plugin)
	}
	return adapter, instance, plugin
}

func waitForWASMState(t *testing.T, plugin *wasmHostedPlugin, state string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if plugin.lifecycleState() == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("wasm state = %s, want %s", plugin.lifecycleState(), state)
}

func dispatchHasRoutePlugin(manager *Manager, pluginID string) bool {
	for _, handler := range manager.extensionState().routes {
		if handler.pluginID == pluginID {
			return true
		}
	}
	return false
}

func dispatchHasRulePlugin(manager *Manager, pluginID string) bool {
	for _, handler := range manager.extensionState().rules {
		if handler.pluginID == pluginID {
			return true
		}
	}
	return false
}

func hasCustomMetric(metrics []CustomMetricSummary, name string) bool {
	for _, metric := range metrics {
		if metric.Name == name {
			return true
		}
	}
	return false
}

func hasOperation(records []OperationRecord, operation, status string) bool {
	_, ok := findOperation(records, operation, status)
	return ok
}

func findOperation(records []OperationRecord, operation, status string) (OperationRecord, bool) {
	for _, record := range records {
		if record.Operation == operation && record.Status == status {
			return record, true
		}
	}
	return OperationRecord{}, false
}

func wasmStaticResponseModule(t *testing.T, exportName string, response WASMABIResponse) []byte {
	t.Helper()
	responseBytes, err := EncodeWASMABIResponse(response)
	if err != nil {
		t.Fatalf("EncodeWASMABIResponse() error = %v", err)
	}
	const responsePtr = uint32(4096)
	responseLen := uint32(len(responseBytes))
	result := (uint64(responsePtr) << 32) | uint64(responseLen)

	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module = append(module, wasmSection(1, []byte{0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e})...)
	module = append(module, wasmSection(3, []byte{0x01, 0x00})...)
	module = append(module, wasmSection(5, []byte{0x01, 0x00, 0x01})...)
	module = append(module, wasmSection(7, wasmStaticResponseExportSection(exportName))...)
	body := []byte{0x00, 0x42}
	body = appendI64LEBForTest(body, int64(result))
	body = append(body, 0x0b)
	code := appendU32LEB(nil, 1)
	code = append(code, appendU32LEB(nil, uint32(len(body)))...)
	code = append(code, body...)
	module = append(module, wasmSection(10, code)...)
	data := []byte{0x01, 0x00, 0x41}
	data = append(data, appendU32LEB(nil, responsePtr)...)
	data = append(data, 0x0b)
	data = append(data, appendU32LEB(nil, responseLen)...)
	data = append(data, responseBytes...)
	module = append(module, wasmSection(11, data)...)
	return module
}

func wasmStaticResponseExportSection(exportName string) []byte {
	payload := appendU32LEB(nil, 2)
	payload = append(payload, byte(len(exportName)))
	payload = append(payload, exportName...)
	payload = append(payload, 0x00, 0x00)
	payload = append(payload, 0x06)
	payload = append(payload, "memory"...)
	payload = append(payload, 0x02, 0x00)
	return payload
}

func appendI64LEBForTest(out []byte, value int64) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		done := (value == 0 && b&0x40 == 0) || (value == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}
