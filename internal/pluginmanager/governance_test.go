// internal/pluginmanager/governance_test.go 包含用于约束 governance 行为的测试。

package pluginmanager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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
	if _, err := manager.CreateReview(context.Background(), "admin", "proxy-review", GovernanceReviewRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Decision:   ReviewDecisionApproved,
	}); err != nil {
		t.Fatalf("CreateReview() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "proxy-review"); err != nil {
		t.Fatalf("Enable(after review) error = %v", err)
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
	_, err := manager.RollbackArtifact(context.Background(), "admin", "revoke-plugin", oldArtifact.ID)
	if err == nil || !strings.Contains(err.Error(), "advisory_revoke") {
		t.Fatalf("RollbackArtifact() error = %v, want advisory_revoke", err)
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
