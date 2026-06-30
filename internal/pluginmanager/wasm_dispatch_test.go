package pluginmanager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
