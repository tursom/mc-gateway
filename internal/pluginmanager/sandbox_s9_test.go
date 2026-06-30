package pluginmanager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestSandboxS9OperationsDiagnosticsRedactionQuarantineAndGC(t *testing.T) {
	ctx := context.Background()
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	manager := newSandboxDispatchManagerForTest(t, &fakeSandboxControlPeer{}, registrations)
	artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-s9-ops", registrations)
	enableSandboxDispatchArtifactForTest(t, manager, artifact)

	loaded := manager.loaded[artifact.PluginID]
	if loaded == nil {
		t.Fatal("loaded sandbox plugin missing")
	}
	process := sandboxHostedPluginFromInstance(loaded.instance).process
	if process == nil {
		t.Fatal("loaded sandbox process missing")
	}
	process.stdoutSummary = newSandboxLogSummary(4096)
	process.stderrSummary = newSandboxLogSummary(4096)
	_, _ = process.stdoutSummary.Write([]byte("token=plain-secret-value\n"))
	_, _ = process.stderrSummary.Write([]byte("authorization bearer plain-secret-value\n"))

	snapshot, err := manager.OperationsSnapshot(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("OperationsSnapshot() error = %v", err)
	}
	if len(snapshot.SandboxRuntimes) != 1 {
		t.Fatalf("SandboxRuntimes = %+v, want one live sandbox runtime summary", snapshot.SandboxRuntimes)
	}
	runtimeSummary := snapshot.SandboxRuntimes[0]
	if runtimeSummary.RuntimeInstanceID == "" ||
		runtimeSummary.ControlSocket == "" ||
		runtimeSummary.ActiveCalls != 0 ||
		runtimeSummary.ActiveStreams != 0 ||
		!runtimeSummary.ControlRPC ||
		!runtimeSummary.SecretRPC ||
		len(runtimeSummary.EnforcementFacts) == 0 {
		t.Fatalf("sandbox runtime summary = %+v, want S9 runtime facts", runtimeSummary)
	}
	if stdout := runtimeSummary.EnforcementAttributes["stdout_summary"]; stdout != "[REDACTED]" {
		t.Fatalf("stdout summary = %q, want redacted", stdout)
	}
	if stderr := runtimeSummary.EnforcementAttributes["stderr_summary"]; stderr != "[REDACTED]" {
		t.Fatalf("stderr summary = %q, want redacted", stderr)
	}

	data, diagnostic, err := manager.DiagnosticPackage(ctx, "admin", artifact.PluginID)
	if err != nil {
		t.Fatalf("DiagnosticPackage() error = %v", err)
	}
	if !containsDiagnosticSection(diagnostic.Sections, "sandbox_runtime") {
		t.Fatalf("diagnostic sections = %+v, want sandbox_runtime", diagnostic.Sections)
	}
	diagnosticBody := string(data)
	if !strings.Contains(diagnosticBody, `"sandbox_runtime"`) ||
		!strings.Contains(diagnosticBody, `"runtime_instance_id"`) ||
		!strings.Contains(diagnosticBody, `"stdout_stderr"`) {
		t.Fatalf("diagnostic package = %s, want sandbox runtime section", diagnosticBody)
	}
	if strings.Contains(diagnosticBody, "plain-secret-value") {
		t.Fatalf("diagnostic package leaked secret plaintext: %s", diagnosticBody)
	}

	runtimeRoot := manager.operations.runtimeRoot()
	leftoverDir := filepath.Join(runtimeRoot, artifact.PluginID, "mc-gateway-sandbox-leftover")
	leftoverSocket := filepath.Join(runtimeRoot, artifact.PluginID, "mc-gateway-sandbox-leftover.sock")
	leftoverCgroup := filepath.Join(runtimeRoot, artifact.PluginID, "sandbox-cgroup-leftover")
	for _, dir := range []string{leftoverDir, leftoverCgroup} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	if err := os.WriteFile(leftoverSocket, []byte("stale"), 0600); err != nil {
		t.Fatalf("WriteFile(stale socket) error = %v", err)
	}
	candidates, err := manager.operations.GCCandidates(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("GCCandidates() error = %v", err)
	}
	kinds := gcCandidateKinds(candidates)
	for _, want := range []string{"sandbox_runtime_temp_dir", "sandbox_stale_socket", "sandbox_stale_cgroup"} {
		if !kinds[want] {
			t.Fatalf("GC candidates = %+v, missing %s", candidates, want)
		}
	}
	removed, err := manager.RunOperationsGC(ctx, "admin", artifact.PluginID, false)
	if err != nil {
		t.Fatalf("RunOperationsGC() error = %v", err)
	}
	removedKinds := gcCandidateKinds(removed)
	for _, want := range []string{"sandbox_runtime_temp_dir", "sandbox_stale_socket", "sandbox_stale_cgroup"} {
		if !removedKinds[want] {
			t.Fatalf("removed GC candidates = %+v, missing %s", removed, want)
		}
	}
	for _, path := range []string{leftoverDir, leftoverSocket, leftoverCgroup} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stat %s error = %v, want removed", path, err)
		}
	}

	process.crashLoop = true
	process.crashCount = 3
	process.lastError = "sandbox crash loop"
	snapshot, err = manager.OperationsSnapshot(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("OperationsSnapshot(crash loop) error = %v", err)
	}
	if len(snapshot.SandboxRuntimes) != 1 ||
		!snapshot.SandboxRuntimes[0].CrashLoop ||
		snapshot.SandboxRuntimes[0].ReasonCode != ReasonSandboxCrashLoop {
		t.Fatalf("crash-loop sandbox runtime summary = %+v, want machine-readable quarantine reason", snapshot.SandboxRuntimes)
	}
	plugin, err := manager.Plugin(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin(after quarantine) error = %v", err)
	}
	if plugin.RuntimeState != RuntimeFailed || !strings.Contains(plugin.LastError, "crash loop") {
		t.Fatalf("plugin after crash-loop quarantine = %+v, want failed quarantine state", plugin)
	}
	operations, err := manager.repo.ListOperations(ctx, artifact.PluginID, 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	if !hasOperationReasonCode(operations, "sandbox_crash_loop_quarantine", ReasonSandboxCrashLoop) {
		t.Fatalf("operations = %+v, want sandbox crash-loop quarantine reason code", operations)
	}
}

func TestSandboxS9ReasonCodesForGateFailuresAndSecretDenial(t *testing.T) {
	ctx := context.Background()
	manager := newSandboxServiceModeManagerForTest(t, SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024})
	capabilityArtifact := uploadTestArtifactWithManifest(t, manager, "sandbox-s9-capability", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.Capabilities = json.RawMessage(`{"runtime":{"required_capabilities":["network.egress"]}}`)
	})
	preflight := manager.preflightChecks(ctx, PluginRecord{ID: capabilityArtifact.PluginID, ConfigJSON: `{}`}, capabilityArtifact, mustManifestFromArtifact(t, capabilityArtifact), PolicyProfileDev, GovernanceActionEnable, `{}`)
	if !preflightHasReasonCode(preflight.Checks, "capability_enforcement_unavailable", ReasonSandboxCapabilityBlock) {
		t.Fatalf("capability preflight = %+v, want %s reason code", preflight, ReasonSandboxCapabilityBlock)
	}

	abiArtifact := uploadTestArtifactWithManifest(t, manager, "sandbox-s9-abi", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
	})
	abiManifest := mustManifestFromArtifact(t, abiArtifact)
	abiManifest.Runtime.ABIVersion = "mc-gateway.sandbox-process.abi/v0"
	preflight = manager.preflightChecks(ctx, PluginRecord{ID: abiArtifact.PluginID, ConfigJSON: `{}`}, abiArtifact, abiManifest, PolicyProfileDev, GovernanceActionEnable, `{}`)
	if !preflightHasReasonCode(preflight.Checks, "sandbox_artifact_metadata_invalid", ReasonSandboxABIMismatch) {
		t.Fatalf("ABI preflight = %+v, want %s reason code", preflight, ReasonSandboxABIMismatch)
	}

	secretArtifact := uploadTestArtifactWithManifest(t, manager, "sandbox-s9-secret", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.Secrets = []SecretSpec{{Name: "api_token"}}
	})
	plugin, err := manager.repo.UpsertDesired(ctx, "admin", secretArtifact.PluginID, secretArtifact.ID, DesiredDisabled, `{}`, 10)
	if err != nil {
		t.Fatalf("UpsertDesired(secret) error = %v", err)
	}
	denied, err := manager.ResolveSandboxSecret(ctx, SandboxSecretRequest{
		PluginID:   secretArtifact.PluginID,
		ArtifactID: secretArtifact.ID,
		Generation: plugin.DesiredGeneration,
		Handle:     "missing",
	})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(missing) error = %v", err)
	}
	if denied.OK || denied.ErrorCode != "secret_not_declared" || denied.ReasonCode != ReasonSandboxSecretDenied {
		t.Fatalf("secret denial = %+v, want error code and %s reason code", denied, ReasonSandboxSecretDenied)
	}
	operations, err := manager.repo.ListOperations(ctx, secretArtifact.PluginID, 20)
	if err != nil {
		t.Fatalf("ListOperations(secret denial) error = %v", err)
	}
	if !hasOperationReasonCode(operations, "sandbox_secret_resolve", ReasonSandboxSecretDenied) {
		t.Fatalf("secret denial operations = %+v, want %s reason code", operations, ReasonSandboxSecretDenied)
	}
}

func containsDiagnosticSection(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func gcCandidateKinds(candidates []GCCandidate) map[string]bool {
	out := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		out[candidate.Kind] = true
	}
	return out
}

func preflightHasReasonCode(checks []PreflightCheck, code, reasonCode string) bool {
	for _, check := range checks {
		if check.Code != code {
			continue
		}
		if check.Details["reason_code"] == reasonCode {
			return true
		}
	}
	return false
}

func hasOperationReasonCode(records []OperationRecord, operation, reasonCode string) bool {
	for _, record := range records {
		if record.Operation != operation {
			continue
		}
		var metadata map[string]any
		if json.Unmarshal([]byte(defaultJSONObject(record.MetadataJSON)), &metadata) != nil {
			continue
		}
		if metadata["reason_code"] == reasonCode {
			return true
		}
	}
	return false
}
