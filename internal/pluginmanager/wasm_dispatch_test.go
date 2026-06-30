package pluginmanager

import (
	"context"
	"encoding/json"
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
