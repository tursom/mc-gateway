// internal/pluginmanager/governance_test.go 包含用于约束 governance 行为的测试。

package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestGovernanceHighRiskProtocolProxyRequiresReview(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithCapabilities(t, manager, "proxy-review", testProtocolProxyCapabilities())
	if _, err := manager.SetDesired(context.Background(), "admin", "proxy-review", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	_, err := manager.Enable(context.Background(), "admin", "proxy-review")
	if err == nil || !strings.Contains(err.Error(), "review_required") {
		t.Fatalf("Enable() error = %v, want review_required", err)
	}
	review, err := manager.CreateReview(context.Background(), "admin", "proxy-review", GovernanceReviewRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Decision:   ReviewDecisionApproved,
	})
	if err != nil {
		t.Fatalf("CreateReview() error = %v", err)
	}
	for name, value := range map[string]string{
		"artifact_hash":       review.ArtifactHash,
		"config_hash":         review.ConfigHash,
		"scope_hash":          review.ScopeHash,
		"rollout_hash":        review.RolloutHash,
		"runtime_limits_hash": review.RuntimeLimitsHash,
		"features_hash":       review.FeaturesHash,
		"policy_hash":         review.PolicyHash,
	} {
		if value == "" {
			t.Fatalf("review %s is empty: %+v", name, review)
		}
	}
	if review.ArtifactHash != artifact.SHA256 {
		t.Fatalf("review artifact hash = %q, want artifact sha %q", review.ArtifactHash, artifact.SHA256)
	}
	if _, err := manager.Enable(context.Background(), "admin", "proxy-review"); err != nil {
		t.Fatalf("Enable(after review) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "proxy-review", artifact.ID, DesiredEnabled, `{"canary":true}`, 10); err != nil {
		t.Fatalf("SetDesired(config change) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "proxy-review"); err == nil || !strings.Contains(err.Error(), "review_required") {
		t.Fatalf("Enable(after reviewed config drift) error = %v, want review_required", err)
	}
}

func TestGovernanceBlocksFailedPackagedConformance(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytes(t, "conformance-gate"),
		"plugin.so":     []byte("fake plugin bytes"),
		"conformance.json": []byte(`{
			"fixtures":[{"name":"protocol-proxy.panic","status":"fail","expected":"panic_recovered"}]
		}`),
	})
	artifact, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "conformance-gate.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "conformance-gate", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	preflight, err := manager.RunPreflight(context.Background(), "admin", "conformance-gate", PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileDev,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["conformance_fixture_failed"] {
		t.Fatalf("preflight = %+v, want conformance_fixture_failed block", preflight)
	}
	if _, err := manager.Enable(context.Background(), "admin", "conformance-gate"); err == nil || !strings.Contains(err.Error(), "conformance_fixture_failed") {
		t.Fatalf("Enable() error = %v, want conformance fixture gate", err)
	}
}

func TestGovernanceStrictPolicyBlocksMissingConformance(t *testing.T) {
	manager := New(Options{
		DB:                        openPluginManagerTestDB(t),
		ArtifactRoot:              t.TempDir(),
		Adapter:                   &fakeAdapter{},
		PolicyProfile:             PolicyProfileProd,
		RequireConformanceFixture: true,
	})
	artifact := uploadTestArtifact(t, manager, "missing-conformance")
	if _, err := manager.SetDesired(context.Background(), "admin", "missing-conformance", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	preflight, err := manager.RunPreflight(context.Background(), "admin", "missing-conformance", PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["conformance_fixture_missing"] {
		t.Fatalf("preflight = %+v, want conformance_fixture_missing block", preflight)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "missing-conformance", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !governanceIssueCodes(decision.Issues)["conformance_fixture_missing"] {
		t.Fatalf("decision = %+v, want conformance_fixture_missing issue", decision)
	}
	if _, err := manager.Enable(context.Background(), "admin", "missing-conformance"); err == nil || !strings.Contains(err.Error(), "conformance_fixture_missing") {
		t.Fatalf("Enable() error = %v, want missing conformance fixture gate", err)
	}
}

func TestSandboxUnenforceableCapabilityBlocksGovernanceAndOverride(t *testing.T) {
	manager := newSandboxServiceModeManagerForTest(t, SandboxPolicy{})
	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-unenforceable", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.Capabilities = json.RawMessage(`{"runtime":{"required_capabilities":["network.egress"]}}`)
	})
	if _, err := manager.repo.UpsertDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("UpsertDesired() error = %v", err)
	}

	preflight, err := manager.RunPreflight(context.Background(), "admin", artifact.PluginID, PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["capability_enforcement_unavailable"] {
		t.Fatalf("preflight = %+v, want capability enforcement block", preflight)
	}

	decision, err := manager.EvaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	issueCodes := governanceIssueCodes(decision.Issues)
	if decision.OK || !decision.ReviewRequired || !issueCodes["capability_enforcement_unavailable"] || !issueCodes["review_required"] {
		t.Fatalf("decision = %+v, want capability block and high-risk review requirement", decision)
	}
	releaseDecision, err := manager.EvaluateReleaseGate(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err == nil || releaseDecision.OK || !governanceIssueCodes(releaseDecision.Issues)["capability_enforcement_unavailable"] {
		t.Fatalf("EvaluateReleaseGate() decision=%+v err=%v, want enforcement block", releaseDecision, err)
	}
	if _, err := manager.CreateWarningOverride(context.Background(), "admin", artifact.PluginID, WarningOverrideRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		Reason:     "attempt to override sandbox enforcement",
	}); err == nil || !strings.Contains(err.Error(), "blocking governance issues cannot be overridden") {
		t.Fatalf("CreateWarningOverride() error = %v, want blocking override rejection", err)
	}
}

func TestSandboxPreflightBlocksProtocolABIMetadataMismatch(t *testing.T) {
	manager := newSandboxServiceModeManagerForTest(t, SandboxPolicy{ExternalIsolation: true})
	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-abi-preflight", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
	})
	var metadata map[string]any
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &metadata); err != nil {
		t.Fatalf("Unmarshal metadata error = %v", err)
	}
	runtimeMetadata := jsonMapFromAny(metadata["runtime"])
	runtimeMetadata["abi_version"] = "mc-gateway.sandbox-process.abi/v0"
	metadata["runtime"] = runtimeMetadata
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal tampered metadata error = %v", err)
	}
	artifact.MetadataJSON = string(data)
	if err := manager.repo.SaveArtifact(context.Background(), artifact); err != nil {
		t.Fatalf("SaveArtifact(tampered) error = %v", err)
	}
	if _, err := manager.repo.UpsertDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("UpsertDesired() error = %v", err)
	}
	preflight, err := manager.RunPreflight(context.Background(), "admin", artifact.PluginID, PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileDev,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["sandbox_artifact_metadata_invalid"] {
		t.Fatalf("preflight = %+v, want sandbox artifact metadata block", preflight)
	}
}

func TestWASMGovernanceBlocksHighRiskExtensionPoint(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifestBytes(t, manager, "wasm-status-risk", wasmOKModule, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.Runtime.ABI = wasmHostABIV1
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
		manifest.RuntimeLimits.MemoryBytes = 64 * 1024
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionStatusPing}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["status.ping/v1"],"status":{"hosts":["*"]}}`)
	})
	manifest := mustManifestFromArtifact(t, artifact)
	preflight := manager.preflightChecks(context.Background(), PluginRecord{ID: artifact.PluginID, ConfigJSON: `{}`}, artifact, manifest, PolicyProfileProd, GovernanceActionEnable, `{}`)
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["wasm_extension_point_unsupported"] {
		t.Fatalf("preflight = %+v, want wasm_extension_point_unsupported block", preflight)
	}
	decision, _, err := manager.evaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`, true)
	if err != nil {
		t.Fatalf("evaluateGovernance() error = %v", err)
	}
	if decision.OK || !governanceIssueCodes(decision.Issues)["wasm_extension_point_unsupported"] {
		t.Fatalf("decision = %+v, want wasm_extension_point_unsupported issue", decision)
	}
}

func TestWASMPreflightAndGovernanceBlockHostCapabilities(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileDev)
	artifact := uploadTestArtifactWithManifestBytes(t, manager, "wasm-host-capability", wasmOKModule, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.Runtime.ABI = wasmHostABIV1
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
		manifest.RuntimeLimits.MemoryBytes = 64 * 1024
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}}
		manifest.Capabilities = json.RawMessage(`{"env":{"FOO":"bar"},"network":{"egress":["*"]}}`)
		manifest.Secrets = []SecretSpec{{Name: "api_token", Required: true}}
		manifest.ExternalDeps = []ExternalSpec{{Name: "profile-api", Endpoint: "https://profile.example", Required: true}}
		manifest.FileStores = []FileStoreSpec{{Namespace: "cache"}}
	})
	result, err := manager.DryRunConfig(context.Background(), artifact.PluginID, artifact.ID, `{}`)
	if err == nil || result.OK || !strings.Contains(err.Error(), "wasm runtime does not support host capabilities") {
		t.Fatalf("DryRunConfig() = %+v err=%v, want wasm capability block", result, err)
	}
	manifest := mustManifestFromArtifact(t, artifact)
	preflight := manager.preflightChecks(context.Background(), PluginRecord{ID: artifact.PluginID, ConfigJSON: `{}`}, artifact, manifest, PolicyProfileDev, GovernanceActionEnable, `{}`)
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["wasm_capability_blocked"] {
		t.Fatalf("preflight = %+v, want wasm_capability_blocked", preflight)
	}
	decision, _, err := manager.evaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileDev, `{}`, true)
	if err != nil {
		t.Fatalf("evaluateGovernance() error = %v", err)
	}
	if decision.OK || !governanceIssueCodes(decision.Issues)["wasm_capability_blocked"] {
		t.Fatalf("decision = %+v, want wasm_capability_blocked issue", decision)
	}
}

func TestWASMProdReleaseGateRequiresConformanceAndResourceSmoke(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifestBytes(t, manager, "wasm-release-gate", wasmOKModule, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.Runtime.ABI = wasmHostABIV1
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
		manifest.RuntimeLimits.MemoryBytes = 64 * 1024
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}}
		manifest.Capabilities = json.RawMessage(`{}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	preflight := manager.preflightChecks(context.Background(), PluginRecord{ID: artifact.PluginID, ConfigJSON: `{}`}, artifact, mustManifestFromArtifact(t, artifact), PolicyProfileProd, GovernanceActionEnable, `{}`)
	codes := preflightCheckCodes(preflight.Checks)
	if preflight.OK || !codes["conformance_fixture_missing"] || !codes["wasm_resource_limit_smoke_passed"] {
		t.Fatalf("preflight = %+v, want conformance block and resource smoke pass", preflight)
	}
	decision, err := manager.EvaluateReleaseGate(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err == nil || decision.OK || !governanceIssueCodes(decision.Issues)["conformance_fixture_missing"] {
		t.Fatalf("EvaluateReleaseGate() decision=%+v err=%v, want missing conformance block", decision, err)
	}
}

func TestWASMAdvisoryGateBlocksArtifact(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json":  testWASMManifestBytes(t, "wasm-advisory", wasmHostABIV1),
		RuntimeWASMEntry: wasmOKModule,
		"conformance.json": []byte(`{
			"fixtures":[{"name":"wasm.release","status":"pass","extension":"rule.evaluate/v1"}]
		}`),
	})
	artifact, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "wasm-advisory.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.UpsertAdvisory(context.Background(), "security", AdvisoryRequest{
		AdvisoryID:     "MCG-WASM-2026-0001",
		Status:         AdvisoryStatusRevoked,
		Action:         AdvisoryActionRevoke,
		ArtifactSHA256: artifact.SHA256,
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}
	decision, err := manager.EvaluateReleaseGate(context.Background(), artifact.PluginID, artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err == nil || decision.OK || !governanceIssueCodes(decision.Issues)["advisory_revoke"] {
		t.Fatalf("EvaluateReleaseGate() decision=%+v err=%v, want advisory_revoke block", decision, err)
	}
}

func TestGovernanceBlocksProtocolProxyScopeOverlap(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	first := enableProtocolProxyTestPlugin(t, manager, "proxy-a")
	if first.ID == "" {
		t.Fatal("first protocol proxy artifact id is empty")
	}
	second := uploadTestArtifactWithCapabilities(t, manager, "proxy-b", testProtocolProxyCapabilities())
	if _, err := manager.SetDesired(context.Background(), "admin", "proxy-b", second.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(second) error = %v", err)
	}
	approveGovernanceForTest(t, manager, "proxy-b", second.ID)
	_, err := manager.Enable(context.Background(), "admin", "proxy-b")
	if err == nil || !strings.Contains(err.Error(), "scope_overlap") {
		t.Fatalf("Enable(second) error = %v, want scope_overlap", err)
	}
}

func TestGovernanceBlocksMissingFeatureAndSecret(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "feature-secret", func(manifest *Manifest) {
		manifest.Capabilities = json.RawMessage(`{"required_features":["wasm-sandbox"]}`)
		manifest.Secrets = []SecretSpec{{Name: "api_token", Required: true}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "feature-secret", artifact.ID, DesiredEnabled, `{}`, 10); err == nil {
		t.Fatal("SetDesired() error = nil, want missing secret from dry-run")
	}
	if _, err := manager.UpsertSecret(context.Background(), "admin", "feature-secret", artifact.ID, "api_token", "secret", true, false); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "feature-secret", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(after secret) error = %v", err)
	}
	_, err := manager.Enable(context.Background(), "admin", "feature-secret")
	if err == nil || !strings.Contains(err.Error(), "feature_missing") {
		t.Fatalf("Enable() error = %v, want feature_missing", err)
	}
}

func TestGovernanceAdvisoryRevokeBlocksRollback(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	oldArtifact := uploadTestArtifact(t, manager, "revoke-plugin")
	if _, err := manager.SetDesired(context.Background(), "admin", "revoke-plugin", oldArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "revoke-plugin"); err != nil {
		t.Fatalf("Enable(old) error = %v", err)
	}
	newArtifact := uploadTestArtifactWithManifest(t, manager, "revoke-plugin", func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "revoke-plugin", newArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "revoke-plugin"); err != nil {
		t.Fatalf("Enable(new) error = %v", err)
	}
	if _, err := manager.UpsertAdvisory(context.Background(), "admin", AdvisoryRequest{
		AdvisoryID:     "MCG-2026-0001",
		Status:         AdvisoryStatusRevoked,
		Action:         AdvisoryActionRevoke,
		ArtifactSHA256: oldArtifact.SHA256,
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}
	before, err := manager.Plugin(context.Background(), "revoke-plugin")
	if err != nil {
		t.Fatalf("Plugin(before rollback) error = %v", err)
	}
	_, err = manager.RollbackArtifact(context.Background(), "admin", "revoke-plugin", oldArtifact.ID)
	if err == nil || !strings.Contains(err.Error(), "advisory_revoke") {
		t.Fatalf("RollbackArtifact() error = %v, want advisory_revoke", err)
	}
	after, err := manager.Plugin(context.Background(), "revoke-plugin")
	if err != nil {
		t.Fatalf("Plugin(after rollback) error = %v", err)
	}
	if after.DesiredArtifactID != before.DesiredArtifactID || after.DesiredGeneration != before.DesiredGeneration {
		t.Fatalf("plugin after blocked rollback = %+v, want unchanged %+v", after, before)
	}
}

func TestGovernanceExternalCIProvenanceBlocksRollback(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileProd)
	oldArtifact := uploadTestArtifactWithProvenance(t, manager, "external-ci-rollback", map[string]any{
		"external_ci": map[string]any{
			"required":        true,
			"artifact_sha256": "wrong-artifact-sha",
			"run_id":          "github-actions/run-rollback",
		},
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-rollback", oldArtifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	newArtifact := uploadTestArtifactWithManifestBytes(t, manager, "external-ci-rollback", []byte("new plugin bytes external-ci-rollback"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-rollback", newArtifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	before, err := manager.Plugin(context.Background(), "external-ci-rollback")
	if err != nil {
		t.Fatalf("Plugin(before rollback) error = %v", err)
	}
	_, err = manager.RollbackArtifact(context.Background(), "admin", "external-ci-rollback", oldArtifact.ID)
	if err == nil || !strings.Contains(err.Error(), "external_ci_artifact_hash_mismatch") {
		t.Fatalf("RollbackArtifact() error = %v, want external_ci_artifact_hash_mismatch", err)
	}
	after, err := manager.Plugin(context.Background(), "external-ci-rollback")
	if err != nil {
		t.Fatalf("Plugin(after rollback) error = %v", err)
	}
	if after.DesiredArtifactID != before.DesiredArtifactID || after.DesiredGeneration != before.DesiredGeneration {
		t.Fatalf("plugin after blocked rollback = %+v, want unchanged %+v", after, before)
	}
}

func TestGovernanceAdvisoryQuarantineRemovesExtensionDispatch(t *testing.T) {
	adapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			if err := api.RegisterHookHandler(gateway, api.HookUpstreamConnect,
				func(api.UpstreamConnectRequest) bool { return true },
				func(api.UpstreamConnectRequest) (net.Conn, error) {
					left, right := net.Pipe()
					_ = right.Close()
					return left, nil
				}); err != nil {
				return err
			}
			if err := api.RegisterHookHandler(gateway, api.HookRouteResolve,
				func(api.RouteResolveRequest) bool { return true },
				func(req api.RouteResolveRequest) (api.RouteDecision, error) {
					return api.RouteDecision{
						Action:     api.RouteDecisionOverride,
						Upstream:   "10.0.0.10:25565",
						Reason:     "cached quarantine fixture",
						CacheTTL:   time.Minute,
						ProviderID: "quarantine-plugin",
					}, nil
				}); err != nil {
				return err
			}
			if err := api.RegisterHookHandler(gateway, api.HookStatusPing,
				func(api.StatusPingRequest) bool { return true },
				func(req api.StatusPingRequest) (api.StatusPingResponse, error) {
					return api.StatusPingResponse{MOTD: "quarantine " + req.Host}, nil
				}); err != nil {
				return err
			}
			return gateway.RegisterBackgroundTask(api.BackgroundTask{
				ID:     "sync",
				Name:   "Quarantine Sync",
				Manual: true,
				Run: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			})
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "quarantine-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{
			{Type: "hook", Key: ExtensionUpstreamConnect},
			{Type: "provider", Key: ExtensionRouteResolve},
			{Type: "hook", Key: ExtensionStatusPing},
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["upstream.connect/v1","route.resolve/v1","status.ping/v1"],"upstream_connect":{"mode":"dialer"},"route":{"cache_ttl_ms":60000},"status":{"hosts":["play.example"]}}`)
		manifest.BackgroundTasks = []TaskSpec{{ID: "sync", Mode: "manual", Manual: true, Timeout: "1s"}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "quarantine-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "quarantine-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	upstream, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example"})
	if err != nil || !upstream.Handled {
		t.Fatalf("ConnectUpstream(before quarantine) = %+v err=%v, want upstream dispatch", upstream, err)
	}
	route, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example"}, nil)
	if err != nil || route.Source != "provider" || route.Decision.Upstream != "10.0.0.10:25565" {
		t.Fatalf("ResolveRoute(before quarantine) = %+v err=%v, want provider override", route, err)
	}
	status, err := manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "play.example"})
	if err != nil || !status.Handled {
		t.Fatalf("StatusPing(before quarantine) = %+v err=%v, want handled", status, err)
	}
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "quarantine-plugin", "sync", manager.operations.plugins["quarantine-plugin"].tasks["sync"].confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	taskRuntime := manager.operations.plugins["quarantine-plugin"].tasks["sync"]
	waitForPluginManagerTest(t, func() bool {
		return taskRuntime.summary().Running
	})
	if _, err := manager.UpsertAdvisory(context.Background(), "admin", AdvisoryRequest{
		AdvisoryID: "MCG-2026-QUARANTINE",
		Action:     AdvisoryActionQuarantine,
		PluginID:   "quarantine-plugin",
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return !taskRuntime.summary().Running
	})
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Routes) != 0 || len(plan.Statuses) != 0 {
		t.Fatalf("dispatch plan after quarantine = %+v, want extension dispatch removed", plan)
	}
	upstream, err = manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example"})
	if err != nil || upstream.Handled {
		t.Fatalf("ConnectUpstream(after quarantine) = %+v err=%v, want upstream dispatch removed", upstream, err)
	}
	status, err = manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "play.example"})
	if err != nil || status.Handled {
		t.Fatalf("StatusPing(after quarantine) = %+v err=%v, want default fallback", status, err)
	}
	route, err = manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
	if err != nil || route.Source != "sqlite_fallback" || route.Decision.Upstream != "sqlite:25565" {
		t.Fatalf("ResolveRoute(after quarantine) = %+v err=%v, want sqlite fallback without stale cache", route, err)
	}
	plugin, err := manager.Plugin(context.Background(), "quarantine-plugin")
	if err != nil {
		t.Fatalf("Plugin(after quarantine) error = %v", err)
	}
	if plugin.RuntimeState != RuntimeDraining || plugin.DesiredState != DesiredEnabled {
		t.Fatalf("plugin after quarantine = %+v, want draining runtime with desired state preserved", plugin)
	}
}

func TestGovernanceWarningOverrideTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	db := openPluginManagerTestDB(t)
	manager := New(Options{
		DB:           db,
		ArtifactRoot: t.TempDir(),
		Adapter:      &fakeAdapter{},
	})
	manager.repo.now = func() time.Time { return now }
	artifact := uploadTestArtifact(t, manager, "bench-plugin")
	if _, err := manager.SetDesired(context.Background(), "admin", "bench-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.SaveBenchmark(context.Background(), "admin", BenchmarkRequest{
		ArtifactID:       artifact.ID,
		Profile:          PolicyProfileProd,
		BenchmarkProfile: "release",
		P95MS:            10,
		P99MS:            20,
		BaselineDiff:     0.30,
	}); err != nil {
		t.Fatalf("SaveBenchmark() error = %v", err)
	}
	_, err := manager.Enable(context.Background(), "admin", "bench-plugin")
	if err == nil || !strings.Contains(err.Error(), "benchmark_regression_warning") {
		t.Fatalf("Enable() error = %v, want benchmark warning", err)
	}
	if _, err := manager.CreateWarningOverride(context.Background(), "admin", "bench-plugin", WarningOverrideRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		Reason:     "accepted for canary",
		TTLSeconds: 60,
	}); err != nil {
		t.Fatalf("CreateWarningOverride() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "bench-plugin"); err != nil {
		t.Fatalf("Enable(with override) error = %v", err)
	}
	if _, err := manager.Disable(context.Background(), "admin", "bench-plugin"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	now = now.Add(2 * time.Minute)
	_, err = manager.Enable(context.Background(), "admin", "bench-plugin")
	if err == nil || !strings.Contains(err.Error(), "benchmark_regression_warning") {
		t.Fatalf("Enable(after override expiry) error = %v, want benchmark warning", err)
	}
	overrides, err := manager.ListWarningOverrides(context.Background(), "bench-plugin")
	if err != nil {
		t.Fatalf("ListWarningOverrides() error = %v", err)
	}
	if len(overrides) != 1 || overrides[0].ExpiresAt > now.Unix() || overrides[0].PolicyHash == "" {
		t.Fatalf("overrides = %+v, want retained expired override audit with policy hash", overrides)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "bench-plugin", 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	if !operationRecorded(ops, "governance_benchmark:succeeded") ||
		!operationRecorded(ops, "governance_warning_override:succeeded") ||
		!operationRecorded(ops, "enable_gate:failed") {
		t.Fatalf("operations = %+v, want benchmark, override, and expired re-block audit", ops)
	}
}

func TestGovernanceBenchmarkBlocking(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "bench-block", func(manifest *Manifest) {
		manifest.RuntimeLimits.HandlerTimeoutMS = 100
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "bench-block", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.SaveBenchmark(context.Background(), "admin", BenchmarkRequest{
		ArtifactID:       artifact.ID,
		Profile:          PolicyProfileProd,
		BenchmarkProfile: "release",
		P95MS:            80,
		P99MS:            200,
		BaselineDiff:     0.60,
	}); err != nil {
		t.Fatalf("SaveBenchmark() error = %v", err)
	}
	_, err := manager.Enable(context.Background(), "admin", "bench-block")
	if err == nil || !strings.Contains(err.Error(), "benchmark_regression_blocking") {
		t.Fatalf("Enable() error = %v, want benchmark_regression_blocking", err)
	}
}

func TestGovernanceProdBlocksLocalProcessSourceBuildProvenance(t *testing.T) {
	builder := &fakeBuilder{result: fakeSourceBuildResult(t, "source-local", BuilderTypeLocalProcess, "", "")}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	}, PolicyProfileDev)
	_ = uploadTestSource(t, manager, "source-local")
	builds, err := manager.ListBuilds(context.Background(), "source-local")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].BuilderType != BuilderTypeLocalProcess {
		t.Fatalf("builds = %+v, want one local-process build", builds)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusSucceeded || build.ArtifactID == "" {
		t.Fatalf("build = %+v, want succeeded with artifact", build)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "source-local", build.ArtifactID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if err := manager.SetPolicyProfile(PolicyProfileProd); err != nil {
		t.Fatalf("SetPolicyProfile() error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "source-local", build.ArtifactID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !governanceIssueCodes(decision.Issues)["source_build_local_process_prod"] {
		t.Fatalf("decision = %+v, want source_build_local_process_prod block", decision)
	}
	result, err := manager.RunPreflight(context.Background(), "admin", "source-local", PreflightRequest{
		ArtifactID: build.ArtifactID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	if result.OK || !preflightCheckCodes(result.Checks)["source_build_local_process_prod"] {
		t.Fatalf("preflight = %+v, want source_build_local_process_prod block", result)
	}
}

func TestGovernanceProdAllowsContainerSourceBuildWithProvenance(t *testing.T) {
	builderImage, builderDigest := testPinnedBuilderImage()
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", builderImage)
	builder := &fakeBuilder{result: fakeSourceBuildResult(t, "source-container", BuilderTypeContainer, builderImage, builderDigest)}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
		BuilderTypeContainer:    builder,
	}, PolicyProfileProd)
	_ = uploadTestSource(t, manager, "source-container")
	builds, err := manager.ListBuilds(context.Background(), "source-container")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].BuilderType != BuilderTypeContainer {
		t.Fatalf("builds = %+v, want one container build", builds)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusSucceeded || build.ArtifactID == "" {
		t.Fatalf("build = %+v, want succeeded with artifact", build)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "source-container", build.ArtifactID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "source-container", build.ArtifactID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	issueCodes := governanceIssueCodes(decision.Issues)
	if !decision.OK || issueCodes["source_build_builder_digest_missing"] || issueCodes["source_build_builder_image_not_pinned"] {
		t.Fatalf("decision = %+v, want container source build admitted without builder image warnings", decision)
	}
	if issueCodes["source_build_builder_image_release_unbound"] {
		t.Fatalf("decision = %+v, want release-bound builder image admitted without release warning", decision)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "source-container", build.ArtifactID, map[string]any{})
	if err != nil {
		t.Fatalf("AssessSupplyChain() error = %v", err)
	}
	sourceBuild := jsonMapFromAny(assessment.Metadata["source_build"])
	if sourceBuild == nil ||
		sourceBuild["prod_admission"] != "allowed" ||
		sourceBuild["builder_image_digest"] != builderDigest ||
		sourceBuild["builder_image_pinned"] != true ||
		sourceBuild["builder_image_gateway_release_bound"] != true ||
		sourceBuild["builder_image_go_version_bound"] != true ||
		sourceBuild["builder_image_api_version_bound"] != true ||
		sourceBuild["builder_image_target_bound"] != true ||
		sourceBuild["builder_image_release_bound"] != true ||
		sourceBuild["builder_go_version_matches_gateway"] != true ||
		sourceBuild["builder_target_matches_gateway"] != true {
		t.Fatalf("source_build assessment metadata = %+v, want allowed provenance with digest", sourceBuild)
	}
}

func TestGovernanceProdWarnsContainerSourceBuildFloatingBuilderImage(t *testing.T) {
	digest := strings.Repeat("b", 64)
	builderImage := "golang:" + strings.TrimPrefix(runtime.Version(), "go")
	builderDigest := "golang@sha256:" + digest
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", builderImage)
	builder := &fakeBuilder{result: fakeSourceBuildResult(t, "source-floating-builder", BuilderTypeContainer, builderImage, builderDigest)}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
		BuilderTypeContainer:    builder,
	}, PolicyProfileProd)
	_ = uploadTestSource(t, manager, "source-floating-builder")
	builds, err := manager.ListBuilds(context.Background(), "source-floating-builder")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "source-floating-builder", build.ArtifactID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "source-floating-builder", build.ArtifactID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	issueCodes := governanceIssueCodes(decision.Issues)
	if decision.OK || !issueCodes["source_build_builder_image_not_pinned"] || issueCodes["source_build_builder_digest_missing"] {
		t.Fatalf("decision = %+v, want floating builder image warning without digest missing warning", decision)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "source-floating-builder", build.ArtifactID, map[string]any{})
	if err != nil {
		t.Fatalf("AssessSupplyChain() error = %v", err)
	}
	sourceBuild := jsonMapFromAny(assessment.Metadata["source_build"])
	if sourceBuild == nil || sourceBuild["prod_admission"] != "warning" || sourceBuild["builder_image_pinned"] != false || sourceBuild["builder_image_go_version_bound"] != true {
		t.Fatalf("source_build assessment metadata = %+v, want warning provenance for floating builder image", sourceBuild)
	}
}

func TestGovernanceProdWarnsContainerBuilderImageWithoutReleaseBinding(t *testing.T) {
	digest := strings.Repeat("c", 64)
	version := strings.TrimPrefix(runtime.Version(), "go")
	builderImage := "ghcr.io/tursom/mc-gateway-plugin-builder:go" + version + "@sha256:" + digest
	builderDigest := "ghcr.io/tursom/mc-gateway-plugin-builder@sha256:" + digest
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", builderImage)
	builder := &fakeBuilder{result: fakeSourceBuildResult(t, "source-unbound-builder", BuilderTypeContainer, builderImage, builderDigest)}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
		BuilderTypeContainer:    builder,
	}, PolicyProfileProd)
	_ = uploadTestSource(t, manager, "source-unbound-builder")
	builds, err := manager.ListBuilds(context.Background(), "source-unbound-builder")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "source-unbound-builder", build.ArtifactID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "source-unbound-builder", build.ArtifactID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	issueCodes := governanceIssueCodes(decision.Issues)
	if decision.OK || !issueCodes["source_build_builder_image_release_unbound"] || issueCodes["source_build_builder_image_not_pinned"] || issueCodes["source_build_builder_digest_missing"] {
		t.Fatalf("decision = %+v, want release binding warning without digest/tag warnings", decision)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "source-unbound-builder", build.ArtifactID, map[string]any{})
	if err != nil {
		t.Fatalf("AssessSupplyChain() error = %v", err)
	}
	sourceBuild := jsonMapFromAny(assessment.Metadata["source_build"])
	if sourceBuild == nil ||
		sourceBuild["prod_admission"] != "warning" ||
		sourceBuild["builder_image_pinned"] != true ||
		sourceBuild["builder_image_gateway_release_bound"] != false ||
		sourceBuild["builder_image_go_version_bound"] != true ||
		sourceBuild["builder_image_api_version_bound"] != false ||
		sourceBuild["builder_image_target_bound"] != false ||
		sourceBuild["builder_image_release_bound"] != false {
		t.Fatalf("source_build assessment metadata = %+v, want release-unbound warning metadata", sourceBuild)
	}
}

func testPinnedBuilderImage() (string, string) {
	digest := strings.Repeat("a", 64)
	version := strings.TrimPrefix(runtime.Version(), "go")
	image := "ghcr.io/tursom/mc-gateway-plugin-builder:release-" + builderImageToken(GatewayRelease) + "-plugin-api-v1-go" + builderImageToken(version) + "-" + runtime.GOOS + "-" + runtime.GOARCH + "@sha256:" + digest
	return image, "ghcr.io/tursom/mc-gateway-plugin-builder@sha256:" + digest
}

func fakeSourceBuildResult(t *testing.T, pluginID, builderType, builderImage, builderImageDigest string) BuildResult {
	t.Helper()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "Source Build",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type:  RuntimeGoPlugin,
			Entry: RuntimeEntry,
		},
		APIVersion:       APIVersion,
		SDKModule:        "github.com/tursom/mc-gateway/plugin/api",
		SDKModuleVersion: "v0.1.0",
		GoVersion:        runtime.Version(),
		GOOS:             runtime.GOOS,
		GOARCH:           runtime.GOARCH,
		ExtensionPoints: []ExtensionPoint{{
			Type: "hook",
			Key:  ExtensionUpstreamConnect,
		}},
		Capabilities: json.RawMessage(`{"extension_points":["upstream.connect/v1"]}`),
	}
	artifactBytes := []byte("fake built plugin bytes " + pluginID)
	sum := sha256.Sum256(artifactBytes)
	artifactSHA := hex.EncodeToString(sum[:])
	abi := abiFingerprint(manifest, runtime.Version())
	metadata := map[string]any{
		"artifact_sha256": artifactSHA,
		"builder_type":    builderType,
		"go_version":      runtime.Version(),
		"go_os":           runtime.GOOS,
		"go_arch":         runtime.GOARCH,
		"abi_fingerprint": abi,
	}
	if builderType == BuilderTypeContainer {
		metadata["builder_image"] = builderImage
		metadata["builder_image_digest"] = builderImageDigest
	}
	return BuildResult{
		Manifest:       manifest,
		ArtifactBytes:  artifactBytes,
		ArtifactSHA256: artifactSHA,
		GoVersion:      runtime.Version(),
		ModuleSummary:  "[]",
		GoVersionM:     "{}",
		ABIFingerprint: abi,
		Metadata:       metadata,
	}
}

func governanceIssueCodes(issues []GovernanceIssue) map[string]bool {
	codes := make(map[string]bool, len(issues))
	for _, issue := range issues {
		codes[issue.Code] = true
	}
	return codes
}

func preflightCheckCodes(checks []PreflightCheck) map[string]bool {
	codes := make(map[string]bool, len(checks))
	for _, check := range checks {
		codes[check.Code] = true
	}
	return codes
}
