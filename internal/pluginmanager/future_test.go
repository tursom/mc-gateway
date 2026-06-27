// internal/pluginmanager/future_test.go 包含用于约束 future 行为的测试。

package pluginmanager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPluginServiceModeRestartRequiredAndApply(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	state, err := manager.PluginServiceState(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceState() error = %v", err)
	}
	if state.DesiredMode != PluginServiceModeInProcess || state.ActiveMode != PluginServiceModeInProcess || state.RestartRequired {
		t.Fatalf("default service state = %+v, want in-process without restart", state)
	}
	state, err = manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeGoPluginProcess)
	if err != nil {
		t.Fatalf("SetPluginServiceDesired() error = %v", err)
	}
	if state.DesiredMode != PluginServiceModeGoPluginProcess || state.ActiveMode != PluginServiceModeInProcess || !state.RestartRequired {
		t.Fatalf("service state after desired switch = %+v, want restart required", state)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode() error = %v", err)
	}
	state, _ = manager.PluginServiceState(context.Background())
	if state.ActiveMode != PluginServiceModeGoPluginProcess || state.RestartRequired {
		t.Fatalf("service state after apply = %+v, want go-plugin-process active", state)
	}
}

func TestGoPluginProcessDrainOnlyAndCrashStatus(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	if _, err := manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeGoPluginProcess); err != nil {
		t.Fatalf("SetPluginServiceDesired() error = %v", err)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode() error = %v", err)
	}
	artifact := uploadTestArtifact(t, manager, "upstream-rewrite")
	if _, err := manager.SetDesired(context.Background(), "admin", "upstream-rewrite", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "upstream-rewrite"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	status, err := manager.PluginServiceStatus(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceStatus() error = %v", err)
	}
	host := findHostStatus(status.Hosts, "upstream-rewrite")
	if host.State != RuntimeEnabled || host.DrainMode != PluginMigrationDrainOnly {
		t.Fatalf("host after enable = %+v, want enabled drain-only", host)
	}
	if _, err := manager.Disable(context.Background(), "admin", "upstream-rewrite"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	status, _ = manager.PluginServiceStatus(context.Background())
	host = findHostStatus(status.Hosts, "upstream-rewrite")
	if host.State != RuntimeDisabled || host.ExitedAt == 0 {
		t.Fatalf("host after disable = %+v, want disabled/exited", host)
	}
	if err := manager.SimulatePluginHostCrash(context.Background(), "upstream-rewrite", "boom"); err != nil {
		t.Fatalf("SimulatePluginHostCrash() error = %v", err)
	}
	status, _ = manager.PluginServiceStatus(context.Background())
	host = findHostStatus(status.Hosts, "upstream-rewrite")
	if !host.CrashLoop || host.State != RuntimeFailed || !strings.Contains(host.LastError, "boom") {
		t.Fatalf("host after crash = %+v, want crash loop failed status", host)
	}
}

func TestSandboxRequiredCapabilityBlocksEnable(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-plugin", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.Runtime.Entry = RuntimeEntry
		manifest.Capabilities = json.RawMessage(`{"runtime":{"required_capabilities":["network.egress"]}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "sandbox-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err == nil || !strings.Contains(err.Error(), "sandbox-process runtime is disabled") {
		t.Fatalf("SetDesired(sandbox disabled) error = %v, want service mode block", err)
	}
	if _, err := manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeSandboxProcess); err != nil {
		t.Fatalf("SetPluginServiceDesired() error = %v", err)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "sandbox-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err == nil || !strings.Contains(err.Error(), "required capabilities") {
		t.Fatalf("SetDesired(sandbox capabilities) error = %v, want capability enforcement block", err)
	}
}

func TestWASMValidationContainment(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "wasm-plugin", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.RuntimeLimits.HandlerTimeoutMS = 10
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}, {Type: "validator", Key: ExtensionConfigValidate}}
	})
	for _, behavior := range []string{"timeout", "panic", "memory"} {
		start := time.Now()
		err := manager.RunWASMValidation(context.Background(), "wasm-plugin", artifact.ID, behavior)
		if err == nil {
			t.Fatalf("RunWASMValidation(%s) error = nil, want contained error", behavior)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("RunWASMValidation(%s) took too long", behavior)
		}
	}
	if err := manager.RunWASMValidation(context.Background(), "wasm-plugin", artifact.ID, "ok"); err != nil {
		t.Fatalf("RunWASMValidation(ok) error = %v", err)
	}
}

func TestRepositoryImportCreatesLocalArtifactWithoutEnable(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	repoDir := t.TempDir()
	artifactPath := filepath.Join(repoDir, "repo-plugin.mcgp")
	manifestBytes := testManifestBytesWithCapabilities(t, "repo-plugin", nil)
	tmpPackage := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("repo plugin bytes"),
	})
	data, err := os.ReadFile(tmpPackage)
	if err != nil {
		t.Fatalf("ReadFile(package) error = %v", err)
	}
	if err := os.WriteFile(artifactPath, data, 0644); err != nil {
		t.Fatalf("WriteFile(repository artifact) error = %v", err)
	}
	indexPath := filepath.Join(repoDir, "index.json")
	index := map[string]any{
		"name": "local-test",
		"artifacts": []map[string]any{{
			"id":            "repo-plugin-0.1.0",
			"plugin_id":     "repo-plugin",
			"version":       "0.1.0",
			"artifact_path": artifactPath,
		}},
	}
	indexBytes, _ := json.Marshal(index)
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}
	record, artifact, err := manager.ImportRepositoryArtifact(context.Background(), "admin", RepositoryImportRequest{
		RepositoryType: RepositoryTypeFile,
		IndexPath:      indexPath,
		ArtifactID:     "repo-plugin-0.1.0",
	})
	if err != nil {
		t.Fatalf("ImportRepositoryArtifact() error = %v", err)
	}
	if record.ArtifactID != artifact.ID || record.RepositoryName != "local-test" {
		t.Fatalf("import record = %+v artifact = %+v", record, artifact)
	}
	if _, err := manager.Plugin(context.Background(), "repo-plugin"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-plugin) error = %v, want not found because import must not auto-enable/create desired state", err)
	}
}

func TestSupplyChainAssessmentBlocksGovernance(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "supply-plugin", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"dependencies":[{"name":"example.com/bad","version":"v1.0.0"}]}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "supply-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() before assessment error = %v", err)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "supply-plugin", artifact.ID, map[string]any{
		"signature": map[string]any{"required": true, "verified": false},
		"sbom":      map[string]any{"required": true, "scan_ok": false},
		"license":   map[string]any{"denylist_matches": []any{"GPL-3.0"}},
		"advisory":  map[string]any{"blocked": true},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain() error = %v", err)
	}
	if assessment.Status != SupplyChainStatusBlocked || len(assessment.Issues) < 4 {
		t.Fatalf("assessment = %+v, want blocked with supply-chain issues", assessment)
	}
	if _, err := manager.Enable(context.Background(), "admin", "supply-plugin"); err == nil || !strings.Contains(err.Error(), "advisory_feed_blocked") {
		t.Fatalf("Enable() error = %v, want supply-chain governance block", err)
	}
}

func TestInstrumentationIsNotRuntimePlugin(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	record, err := manager.SaveInstrumentation(context.Background(), "admin", InstrumentationRequest{
		Name:              "official-trace",
		Version:           "0.1.0",
		Profile:           "official",
		GeneratedDiffHash: "abc123",
		Conformance:       map[string]any{"ok": true},
		Benchmark:         map[string]any{"ok": true},
		Smoke:             map[string]any{"ok": true},
	})
	if err != nil {
		t.Fatalf("SaveInstrumentation() error = %v", err)
	}
	if record.RunbookRollback == "" {
		t.Fatalf("instrumentation missing rollback runbook: %+v", record)
	}
	plugins, err := manager.ListPlugins(context.Background())
	if err != nil {
		t.Fatalf("ListPlugins() error = %v", err)
	}
	for _, plugin := range plugins {
		if plugin.ID == "official-trace" {
			t.Fatalf("instrumentation appeared as runtime plugin: %+v", plugin)
		}
	}
}

func findHostStatus(hosts []PluginHostRuntimeSummary, pluginID string) PluginHostRuntimeSummary {
	for _, host := range hosts {
		if host.PluginID == pluginID {
			return host
		}
	}
	return PluginHostRuntimeSummary{}
}
