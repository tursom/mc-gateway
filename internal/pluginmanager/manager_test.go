// internal/pluginmanager/manager_test.go 包含用于约束 manager 行为的测试。

package pluginmanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol/smoke"
)

func TestManagerUploadDoesNotLoadPlugin(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)

	artifact := uploadTestArtifact(t, manager, "plugin-a")
	if artifact.PluginID != "plugin-a" {
		t.Fatalf("artifact plugin = %q, want plugin-a", artifact.PluginID)
	}
	if adapter.loads != 0 {
		t.Fatalf("adapter loads = %d, want 0 for upload-only validation", adapter.loads)
	}
}

func TestSandboxArtifactGateBlocksTargetMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{
			name: "os mismatch",
			mutate: func(manifest *Manifest) {
				manifest.Runtime.OS = "not-" + runtime.GOOS
			},
			want: "runtime.os",
		},
		{
			name: "abi mismatch",
			mutate: func(manifest *Manifest) {
				manifest.Runtime.ABIVersion = "mc-gateway.sandbox-process.abi/v0"
			},
			want: "runtime.abi_version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newManagerForTest(t, nil)
			var manifest Manifest
			if err := json.Unmarshal(testSandboxManifestBytes(t, "sandbox-gate-"+strings.ReplaceAll(tt.name, " ", "-"), runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1), &manifest); err != nil {
				t.Fatalf("Unmarshal sandbox manifest error = %v", err)
			}
			tt.mutate(&manifest)
			entryBytes := []byte("sandbox gate bytes " + tt.name)
			entrySum := sha256.Sum256(entryBytes)
			entrySHA := hex.EncodeToString(entrySum[:])
			packageSHA := "package-" + entrySHA
			metadata, err := artifactMetadataJSON(manifest, ConformanceSummary{}, false, map[string]any{
				"sandbox": runtimeArtifactMetadata(manifest, entryBytes, packageSHA),
			})
			if err != nil {
				t.Fatalf("artifactMetadataJSON() error = %v", err)
			}
			artifact := ArtifactRecord{
				ID:            entrySHA,
				PluginID:      manifest.ID,
				Version:       manifest.Version,
				FileName:      manifest.ID + ".mcgp",
				SHA256:        entrySHA,
				PackageSHA256: packageSHA,
				ArtifactType:  ArtifactTypeBinary,
				RuntimeType:   RuntimeSandbox,
				RuntimeEntry:  manifest.Runtime.Entry,
				Status:        ArtifactStatusLoadable,
				MetadataJSON:  string(metadata),
				APIVersion:    APIVersion,
				GOOS:          manifest.Runtime.OS,
				GOARCH:        manifest.Runtime.Arch,
				UploadedBy:    "test",
				CreatedAt:     time.Now().Unix(),
				UpdatedAt:     time.Now().Unix(),
			}
			if err := manager.repo.SaveArtifact(context.Background(), artifact); err != nil {
				t.Fatalf("SaveArtifact() error = %v", err)
			}
			_, err = manager.SetDesired(context.Background(), "admin", manifest.ID, artifact.ID, DesiredEnabled, `{}`, 10)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("SetDesired(%s) error = %v, want containing %q", tt.name, err, tt.want)
			}
		})
	}
}

func TestManagerBinaryArtifactLifecycleAndRestartRecovery(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	firstAdapter := &fakeAdapter{}
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      firstAdapter,
	})
	artifact := uploadTestArtifact(t, first, "plugin-a")
	inspected, err := first.Artifact(context.Background(), artifact.ID)
	if err != nil {
		t.Fatalf("Artifact() error = %v", err)
	}
	if inspected.PluginID != "plugin-a" || inspected.ArtifactType != ArtifactTypeBinary || inspected.Status != ArtifactStatusLoadable {
		t.Fatalf("inspected artifact = %+v, want loadable binary plugin-a", inspected)
	}
	if _, err := first.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	loaded, err := first.Load(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.RuntimeState != RuntimeLoaded || loaded.LoadedArtifactID != artifact.ID || firstAdapter.loads != 1 {
		t.Fatalf("loaded plugin = %+v loads=%d, want loaded artifact without dispatch", loaded, firstAdapter.loads)
	}
	result, err := first.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || result.Handled {
		t.Fatalf("ConnectUpstream(after load only) = %+v err=%v, want pass-through", result, err)
	}
	enabled, err := first.Enable(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if enabled.RuntimeState != RuntimeEnabled || enabled.ActiveArtifactID != artifact.ID || firstAdapter.loads != 1 {
		t.Fatalf("enabled plugin = %+v loads=%d, want reused loaded artifact", enabled, firstAdapter.loads)
	}
	result, err = first.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream(enabled) = %+v err=%v, want handled", result, err)
	}

	secondAdapter := &fakeAdapter{}
	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      secondAdapter,
	})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if secondAdapter.loads != 1 {
		t.Fatalf("restart recovery loads = %d, want 1", secondAdapter.loads)
	}
	result, err = second.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream(after reconcile) = %+v err=%v, want handled", result, err)
	}

	disabled, err := second.Disable(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if disabled.RuntimeState != RuntimeDisabled {
		t.Fatalf("disabled plugin = %+v, want disabled", disabled)
	}
	result, err = second.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || result.Handled {
		t.Fatalf("ConnectUpstream(after disable) = %+v err=%v, want pass-through", result, err)
	}
	if _, err := second.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(after disable) error = %v", err)
	}
	if err := second.Delete(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := second.Plugin(context.Background(), "plugin-a"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("Plugin(after delete) error = %v, want ErrPluginNotFound", err)
	}
	if got := second.DispatchPlan(context.Background()).Handlers; len(got) != 0 {
		t.Fatalf("dispatch after delete = %+v, want empty", got)
	}

	third := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      &fakeAdapter{},
	})
	if err := third.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(after delete) error = %v", err)
	}
	if got := third.DispatchPlan(context.Background()).Handlers; len(got) != 0 {
		t.Fatalf("dispatch after deleted restart = %+v, want empty", got)
	}
}

func TestManagerUploadSourceQueuesBuild(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileDev)
	source := uploadTestSource(t, manager, "plugin-a")
	builds, err := manager.ListBuilds(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want 1", len(builds))
	}
	if builds[0].SourceID != source.ID || builds[0].Status != BuildStatusQueued {
		t.Fatalf("queued build = %+v, want source %s queued", builds[0], source.ID)
	}
}

func TestManagerBuildSourceCreatesBinaryArtifact(t *testing.T) {
	t.Setenv("GOCACHE", t.TempDir())
	t.Setenv("GOWORK", "off")
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileDev)
	_ = uploadBuildableTestSource(t, manager, "source-enable")
	builds, err := manager.ListBuilds(context.Background(), "source-enable")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want 1", len(builds))
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusSucceeded || build.ArtifactID == "" {
		t.Fatalf("build = %+v, want succeeded with artifact", build)
	}
	artifact, err := manager.Artifact(context.Background(), build.ArtifactID)
	if err != nil {
		t.Fatalf("Artifact() error = %v", err)
	}
	if artifact.ArtifactType != ArtifactTypeBinary || artifact.Status != ArtifactStatusLoadable {
		t.Fatalf("artifact = %+v, want loadable binary", artifact)
	}
	if !manager.store.HasDistributionPackage(artifact) {
		t.Fatalf("distribution package missing for built artifact %s", artifact.ID)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	plugin, err := manager.Enable(context.Background(), "admin", artifact.PluginID)
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if plugin.RuntimeState != RuntimeEnabled || plugin.ActiveArtifactID != artifact.ID {
		t.Fatalf("plugin = %+v, want enabled built artifact", plugin)
	}
}

func TestManagerBuildFailureDoesNotChangeActiveArtifact(t *testing.T) {
	adapter := &fakeAdapter{}
	builder := &fakeBuilder{err: errors.New("compile failed")}
	manager := newManagerForTestWithBuildersProfile(t, adapter, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	}, PolicyProfileDev)
	active := uploadTestArtifact(t, manager, "plugin-a")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", active.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	source := uploadTestSource(t, manager, "plugin-a")
	builds, err := manager.ListBuilds(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) == 0 || builds[0].SourceID != source.ID {
		t.Fatalf("queued builds = %+v, want source %s", builds, source.ID)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() unexpected manager error = %v", err)
	}
	if build.Status != BuildStatusFailed {
		t.Fatalf("build status = %q, want failed", build.Status)
	}
	plugin, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if plugin.ActiveArtifactID != active.ID {
		t.Fatalf("active artifact = %q, want unchanged %q", plugin.ActiveArtifactID, active.ID)
	}
}

func TestManagerProdDefaultsSourceBuildToContainer(t *testing.T) {
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", "golang:test-builder")
	containerBuilder := &fakeBuilder{}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
		BuilderTypeContainer:    containerBuilder,
	}, PolicyProfileProd)
	source := uploadTestSource(t, manager, "plugin-a")
	builds, err := manager.ListBuilds(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].SourceID != source.ID {
		t.Fatalf("builds = %+v, want queued source build", builds)
	}
	build := builds[0]
	if build.BuilderType != BuilderTypeContainer || build.BuilderImage != "golang:test-builder" || build.BuilderVersion != "container/go-buildmode-plugin" {
		t.Fatalf("build builder = type %q image %q version %q, want container default", build.BuilderType, build.BuilderImage, build.BuilderVersion)
	}
}

func TestManagerProdRejectsExplicitLocalProcessBuild(t *testing.T) {
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", "golang:test-builder")
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
		BuilderTypeContainer:    &fakeBuilder{},
	}, PolicyProfileProd)
	source := uploadTestSource(t, manager, "plugin-a")
	_, err := manager.CreateBuild(context.Background(), "admin", BuildRequest{
		SourceID:    source.ID,
		BuilderType: BuilderTypeLocalProcess,
	})
	if err == nil || !strings.Contains(err.Error(), "disabled in prod profile") {
		t.Fatalf("CreateBuild(local-process prod) error = %v, want prod policy block", err)
	}
}

func TestManagerRunBuildRechecksProdLocalProcessPolicy(t *testing.T) {
	builder := &fakeBuilder{}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	}, PolicyProfileDev)
	_ = uploadTestSource(t, manager, "plugin-a")
	builds, err := manager.ListBuilds(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].BuilderType != BuilderTypeLocalProcess {
		t.Fatalf("builds = %+v, want local-process queued build", builds)
	}
	if err := manager.SetPolicyProfile(PolicyProfileProd); err != nil {
		t.Fatalf("SetPolicyProfile() error = %v", err)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusFailed || !strings.Contains(build.Error, "disabled in prod profile") {
		t.Fatalf("build = %+v, want failed by prod policy", build)
	}
	if builder.calls != 0 {
		t.Fatalf("builder calls = %d, want policy block before execution", builder.calls)
	}
}

func TestSanitizeLogRedactsBuildSecretsAndPrivatePolicy(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	log := strings.Join([]string{
		"/home/tester/work/private/repo",
		"/workspace/private/module/file.go",
		"TOKEN=abc123",
		"api_secret=my-secret",
		"credential=internal",
		"GOPRIVATE=github.com/acme/private",
		"GONOSUMDB=corp.example/internal",
		"GONOPROXY=corp.example/internal",
		"GOPROXY=https://user:pass@proxy.example",
	}, "\n")
	sanitized := sanitizeLog(log)
	for _, forbidden := range []string{"abc123", "my-secret", "internal", "github.com/acme/private", "corp.example/internal", "user:pass", "/home/tester", "/workspace/private/module"} {
		if strings.Contains(sanitized, forbidden) {
			t.Fatalf("sanitizeLog() = %q, still contains %q", sanitized, forbidden)
		}
	}
	for _, required := range []string{"<redacted-private-path>", "TOKEN=<redacted>", "api_secret=<redacted>", "credential=<redacted>", "GOPRIVATE=<policy:configured>", "GONOSUMDB=<policy:configured>", "GONOPROXY=<policy:configured>", "GOPROXY=<policy:configured>"} {
		if !strings.Contains(sanitized, required) {
			t.Fatalf("sanitizeLog() = %q, missing %q", sanitized, required)
		}
	}
}

func TestManagerBuildCancelRetryAuditsStates(t *testing.T) {
	builder := &fakeBuilder{
		result: BuildResult{LogSummary: "compile failed"},
		err:    errors.New("compile failed"),
	}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	}, PolicyProfileDev)
	_ = uploadTestSource(t, manager, "build-lifecycle")
	builds, err := manager.ListBuilds(context.Background(), "build-lifecycle")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].Status != BuildStatusQueued {
		t.Fatalf("builds = %+v, want queued build", builds)
	}
	canceled, err := manager.CancelBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("CancelBuild() error = %v", err)
	}
	if canceled.Status != BuildStatusCanceled {
		t.Fatalf("canceled build = %+v, want canceled", canceled)
	}
	retried, err := manager.RetryBuild(context.Background(), "admin", canceled.ID)
	if err != nil {
		t.Fatalf("RetryBuild() error = %v", err)
	}
	if retried.Status != BuildStatusFailed || retried.ID == canceled.ID || retried.SourceID != canceled.SourceID {
		t.Fatalf("retried build = %+v from canceled %+v, want new failed build for same source", retried, canceled)
	}
	if retried.LogSummary == "" {
		t.Fatalf("retried build = %+v, want failed build with log", retried)
	}
	retryFailed, err := manager.RetryBuild(context.Background(), "admin", retried.ID)
	if err != nil {
		t.Fatalf("RetryBuild(failed) error = %v", err)
	}
	if retryFailed.Status != BuildStatusFailed || retryFailed.ID == retried.ID || retryFailed.LogSummary == "" {
		t.Fatalf("retry from failed = %+v, want new failed build with log", retryFailed)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "build-lifecycle", 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	for _, want := range []string{"source_build_queue:succeeded", "source_build_cancel:succeeded", "source_build:failed"} {
		if !operationRecorded(ops, want) {
			t.Fatalf("operations = %+v, missing %s", ops, want)
		}
	}
}

func TestManagerArtifactGCProtectsQueuedBuildSource(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
	}, PolicyProfileDev)
	source := uploadTestSource(t, manager, "gc-source")
	builds, err := manager.ListBuilds(context.Background(), "gc-source")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].Status != BuildStatusQueued {
		t.Fatalf("builds = %+v, want queued build", builds)
	}
	candidates, err := manager.GCCandidates(context.Background())
	if err != nil {
		t.Fatalf("GCCandidates() error = %v", err)
	}
	for _, candidate := range candidates {
		if candidate.Kind == "artifact" && candidate.ID == source.ID && !candidate.Protected {
			t.Fatalf("source artifact gc candidate = %+v, want protected or absent while build is queued", candidate)
		}
	}
	removed, err := manager.RunGC(context.Background(), "admin", false)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	for _, candidate := range removed {
		if candidate.Kind == "artifact" && candidate.ID == source.ID {
			t.Fatalf("removed source artifact while build queued: %+v", candidate)
		}
	}
	if _, err := os.Stat(source.FilePath); err != nil {
		t.Fatalf("source file after gc stat error = %v", err)
	}
}

func TestManagerArtifactGCProtectsRunningBuildSourceAndLog(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: &fakeBuilder{},
	}, PolicyProfileDev)
	source := uploadTestSource(t, manager, "gc-running-source")
	builds, err := manager.ListBuilds(context.Background(), "gc-running-source")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].Status != BuildStatusQueued {
		t.Fatalf("builds = %+v, want queued build", builds)
	}
	if err := manager.repo.MarkBuildRunning(context.Background(), builds[0].ID); err != nil {
		t.Fatalf("MarkBuildRunning() error = %v", err)
	}
	build, err := manager.Build(context.Background(), builds[0].ID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if build.Status != BuildStatusRunning {
		t.Fatalf("build status = %q, want running", build.Status)
	}
	candidates, err := manager.GCCandidates(context.Background())
	if err != nil {
		t.Fatalf("GCCandidates() error = %v", err)
	}
	foundRunningLog := false
	for _, candidate := range candidates {
		if candidate.Kind == "artifact" && candidate.ID == source.ID {
			t.Fatalf("running build source artifact candidate = %+v, want protected source absent from removable candidates", candidate)
		}
		if candidate.Kind == "build_log" && candidate.ID == buildIDString(build.ID) {
			foundRunningLog = true
			if !candidate.Protected || candidate.Reason != "in-flight build" {
				t.Fatalf("running build log candidate = %+v, want protected in-flight build", candidate)
			}
		}
	}
	if !foundRunningLog {
		t.Fatalf("gc candidates = %+v, want running build log candidate", candidates)
	}
	removed, err := manager.RunGC(context.Background(), "admin", false)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	for _, candidate := range removed {
		if candidate.Kind == "artifact" && candidate.ID == source.ID {
			t.Fatalf("removed source artifact while build running: %+v", candidate)
		}
		if candidate.Kind == "build_log" && candidate.ID == buildIDString(build.ID) {
			t.Fatalf("removed build log while build running: %+v", candidate)
		}
	}
	if _, err := os.Stat(source.FilePath); err != nil {
		t.Fatalf("source file after gc stat error = %v", err)
	}
}

func TestManagerArtifactGCClearsCompletedBuildLog(t *testing.T) {
	builder := &fakeBuilder{
		result: BuildResult{LogSummary: "compile failed without secret"},
		err:    errors.New("compile failed"),
	}
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	}, PolicyProfileDev)
	_ = uploadTestSource(t, manager, "gc-build-log")
	builds, err := manager.ListBuilds(context.Background(), "gc-build-log")
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("builds = %+v, want one build", builds)
	}
	build, err := manager.RunBuild(context.Background(), "admin", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusFailed || build.LogSummary == "" {
		t.Fatalf("build = %+v, want failed build with log summary", build)
	}
	removed, err := manager.RunGC(context.Background(), "admin", false)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	found := false
	for _, candidate := range removed {
		if candidate.Kind == "build_log" && candidate.ID == buildIDString(build.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("removed = %+v, want build_log candidate %d", removed, build.ID)
	}
	after, err := manager.Build(context.Background(), build.ID)
	if err != nil {
		t.Fatalf("Build(after gc) error = %v", err)
	}
	if after.LogSummary != "" {
		t.Fatalf("build log after gc = %q, want empty", after.LogSummary)
	}
}

func TestManagerArtifactGCProtectsDesiredAndSnapshotReferences(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	first := uploadTestArtifact(t, manager, "gc-reference")
	second := uploadTestArtifactWithManifestBytes(t, manager, "gc-reference", []byte("second artifact bytes"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "gc-reference", first.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(first) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "gc-reference", second.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(second) error = %v", err)
	}
	if err := manager.repo.UpdateArtifactStatus(context.Background(), first.ID, ArtifactStatusRejected, "rejected fixture"); err != nil {
		t.Fatalf("UpdateArtifactStatus(first) error = %v", err)
	}
	if err := manager.repo.UpdateArtifactStatus(context.Background(), second.ID, ArtifactStatusRejected, "rejected fixture"); err != nil {
		t.Fatalf("UpdateArtifactStatus(second) error = %v", err)
	}
	removed, err := manager.RunGC(context.Background(), "admin", false)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	for _, candidate := range removed {
		if candidate.Kind == "artifact" && (candidate.ID == first.ID || candidate.ID == second.ID) {
			t.Fatalf("removed referenced artifact: %+v", candidate)
		}
	}
	for _, artifact := range []ArtifactRecord{first, second} {
		if _, err := os.Stat(filepath.Dir(artifact.FilePath)); err != nil {
			t.Fatalf("referenced artifact %s path stat error = %v", artifact.ID, err)
		}
	}
}

func TestManagerRejectsSourceArtifactLoad(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	source := uploadTestSource(t, manager, "plugin-a")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", source.ID, DesiredEnabled, `{}`, 10); err == nil || !strings.Contains(err.Error(), "binary artifact") {
		t.Fatalf("SetDesired(source) error = %v, want binary artifact rejection", err)
	}
}

func TestManagerDryRunRejectsBadConfigWithoutGenerationChange(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	plugin, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"ok":true}`, 10)
	if err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	adapter.dryRunErrs = map[string]error{"plugin-a": errors.New("bad config")}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"ok":false}`, 10); err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("SetDesired(bad config) error = %v, want bad config", err)
	}
	after, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if after.DesiredGeneration != plugin.DesiredGeneration || after.ConfigJSON != `{"ok":true}` {
		t.Fatalf("plugin after bad config = %+v, want generation/config unchanged from %+v", after, plugin)
	}
}

func TestManagerDryRunFailureMatrix(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","required":["host"],"properties":{"host":{"type":"string"},"api_secret_ref":{"type":"string"},"token":{"type":"string","sensitive":true}}}`)
		manifest.Secrets = []SecretSpec{{Name: "api_token"}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredDisabled, `{"host":"old"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"host":`); err == nil || result.OK || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("DryRunConfig(invalid JSON) = %+v err=%v, want JSON rejection", result, err)
	}
	if result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"host":7}`); err == nil || result.OK || !strings.Contains(err.Error(), "$.host must be string") {
		t.Fatalf("DryRunConfig(schema mismatch) = %+v err=%v, want schema rejection", result, err)
	}
	if result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"host":"new","api_secret_ref":"api_token"}`); err == nil || result.OK || !strings.Contains(err.Error(), "missing configured secret") {
		t.Fatalf("DryRunConfig(missing secret) = %+v err=%v, want missing secret", result, err)
	}
	if _, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "api_token", "secret", true, false); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	adapter.dryRunErrs = map[string]error{"plugin-a": errors.New("reload rejected for runtime-secret-config")}
	result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"host":"new","api_secret_ref":"api_token","token":"runtime-secret-config"}`)
	if err == nil || result.OK || !strings.Contains(err.Error(), "reload rejected") {
		t.Fatalf("DryRunConfig(reload failure) = %+v err=%v, want reload rejection", result, err)
	}
	if strings.Contains(err.Error(), "runtime-secret-config") || strings.Contains(result.Error, "runtime-secret-config") || !strings.Contains(result.Error, "[REDACTED]") {
		t.Fatalf("DryRunConfig(reload failure) = %+v err=%v, want redacted sensitive config value", result, err)
	}
	adapter.dryRunErrs = nil
	if result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"host":"new","api_secret_ref":"api_token"}`); err != nil || !result.OK {
		t.Fatalf("DryRunConfig(valid) = %+v err=%v, want ok", result, err)
	}
}

func TestManagerDryRunRedactsSensitiveDiff(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredDisabled, `{"token":"old","host":"a"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	result, err := manager.DryRunConfig(context.Background(), "plugin-a", artifact.ID, `{"token":"new","host":"b"}`)
	if err != nil {
		t.Fatalf("DryRunConfig() error = %v", err)
	}
	if strings.Contains(result.RedactedConfigJSON, "new") || strings.Contains(result.RedactedDiffJSON, "old") || strings.Contains(result.RedactedDiffJSON, "new") {
		t.Fatalf("dry-run leaked secret: %+v", result)
	}
	if !strings.Contains(result.RedactedDiffJSON, "[REDACTED]") {
		t.Fatalf("redacted diff = %s, want redaction marker", result.RedactedDiffJSON)
	}
}

func TestManagerSecretVersionsAreSummariesOnly(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Secrets = []SecretSpec{{
			Name:     "api_token",
			Required: true,
			Type:     "api_token",
			Rotation: SecretRotation{Reload: "reload_required"},
		}}
	})
	first, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "api_token", "secret-one", true, false)
	if err != nil {
		t.Fatalf("UpsertSecret(first) error = %v", err)
	}
	second, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "api_token", "secret-two", false, true)
	if err != nil {
		t.Fatalf("UpsertSecret(second) error = %v", err)
	}
	if first.CurrentVersion != 1 || second.CurrentVersion != 2 || second.PreviousVersion != 1 || !second.HotReload || second.ReloadRequired {
		t.Fatalf("secret versions = first %+v second %+v", first, second)
	}
	data, _ := json.Marshal(second)
	if strings.Contains(string(data), "secret-one") || strings.Contains(string(data), "secret-two") {
		t.Fatalf("secret summary leaked value: %s", data)
	}
}

func TestManagerSecretRotationAndFailureDoNotChangeActiveState(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Secrets = []SecretSpec{{
			Name:     "api_token",
			Required: true,
			Type:     "api_token",
			Rotation: SecretRotation{Reload: "hot"},
		}}
	})
	if _, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "api_token", "secret-one", false, true); err != nil {
		t.Fatalf("UpsertSecret(first) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"host":"a"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	before, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin(before) error = %v", err)
	}
	rotated, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "api_token", "secret-two", false, true)
	if err != nil {
		t.Fatalf("UpsertSecret(rotation) error = %v", err)
	}
	if rotated.CurrentVersion != 2 || rotated.PreviousVersion != 1 || !rotated.HotReload || rotated.ReloadRequired {
		t.Fatalf("rotated secret = %+v, want current 2 previous 1 hot reload", rotated)
	}
	afterRotate, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin(after rotation) error = %v", err)
	}
	if afterRotate.ActiveArtifactID != before.ActiveArtifactID ||
		afterRotate.LoadedArtifactID != before.LoadedArtifactID ||
		afterRotate.RuntimeState != before.RuntimeState ||
		afterRotate.AppliedGeneration != before.AppliedGeneration ||
		afterRotate.DesiredGeneration != before.DesiredGeneration {
		t.Fatalf("plugin after secret rotation = %+v, want active state unchanged from %+v", afterRotate, before)
	}
	if _, err := manager.UpsertSecret(context.Background(), "admin", "plugin-a", artifact.ID, "missing_secret", "secret-three", false, true); err == nil {
		t.Fatal("UpsertSecret(undeclared) error = nil")
	}
	afterFail, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin(after failed rotation) error = %v", err)
	}
	if afterFail.ActiveArtifactID != before.ActiveArtifactID ||
		afterFail.LoadedArtifactID != before.LoadedArtifactID ||
		afterFail.RuntimeState != before.RuntimeState ||
		afterFail.AppliedGeneration != before.AppliedGeneration ||
		afterFail.DesiredGeneration != before.DesiredGeneration {
		t.Fatalf("plugin after failed secret rotation = %+v, want active state unchanged from %+v", afterFail, before)
	}
	operations, err := manager.repo.ListOperations(context.Background(), "plugin-a", 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	data, _ := json.Marshal(operations)
	for _, forbidden := range []string{"secret-one", "secret-two", "secret-three"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("operation log leaked %q: %s", forbidden, data)
		}
	}
}

func TestManagerOperationRecordsRedactSecretMetadata(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	if err := manager.repo.RecordOperation(context.Background(), "plugin-a", "artifact-a", "secret_fixture", "failed", "admin", "token=plain-secret", map[string]any{
		"token":    "plain-secret",
		"endpoint": "https://user:pass@example.test/session?token=plain-secret",
		"nested": map[string]any{
			"password": "plain-password",
			"host":     "play.example",
		},
	}); err != nil {
		t.Fatalf("RecordOperation() error = %v", err)
	}
	records, err := manager.repo.ListOperations(context.Background(), "plugin-a", 10)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("operations = %+v, want one record", records)
	}
	payload, _ := json.Marshal(records[0])
	for _, forbidden := range []string{"plain-secret", "plain-password", "user:pass"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("operation record leaked %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(records[0].MetadataJSON, "play.example") {
		t.Fatalf("operation metadata = %s, want non-sensitive context retained", records[0].MetadataJSON)
	}
}

func TestManagerOperationsRecordsHandlerMetricsEventsAndDiagnostics(t *testing.T) {
	var gateway *Gateway
	manager := newManagerForTest(t, &fakeAdapter{
		init: func(g *Gateway) {
			gateway = g
		},
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(req api.UpstreamConnectRequest) (net.Conn, error) {
				_ = gateway.EmitEvent(req.Context, "auth.success", map[string]string{"result": "fixture_accept", "mode": "fixture"})
				_ = gateway.ObserveMetric(req.Context, "auth.attempts", 1, map[string]string{"result": "fixture_accept", "mode": "fixture"})
				gateway.Logger().Info(req.Context, "auth success token=secret-value", map[string]string{"result": "fixture_accept"})
				return newMemoryConn(), nil
			},
		},
	})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Events = []EventSpec{{Name: "auth.success", Fields: []string{"result", "mode"}}}
		manifest.CustomMetrics = []MetricSpec{{Name: "auth.attempts", Type: "counter", Labels: []string{"result", "mode"}}}
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"token":"plain-token-value","host":"play.example"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if _, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"}); err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	var snap OperationsSnapshot
	for time.Now().Before(deadline) {
		var err error
		snap, err = manager.OperationsSnapshot(context.Background(), "plugin-a")
		if err != nil {
			t.Fatalf("OperationsSnapshot() error = %v", err)
		}
		if len(snap.Events) > 0 && len(snap.CustomMetrics) > 0 && len(snap.Traces) > 0 && len(snap.Handlers) > 0 && snap.Handlers[0].Calls > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := manager.repo.RecordOperation(context.Background(), "plugin-a", artifact.ID, "diagnostic_fixture", "failed", "admin", "metadata token=plain-token-value", map[string]any{
		"token":    "plain-token-value",
		"endpoint": "https://user:pass@example.test/session",
		"payload":  "packet-payload-raw",
	}); err != nil {
		t.Fatalf("RecordOperation() error = %v", err)
	}
	if len(snap.Events) == 0 || snap.Events[0].Name != "auth.success" {
		t.Fatalf("events = %+v, want auth.success", snap.Events)
	}
	if len(snap.Handlers) == 0 || snap.Handlers[0].Calls == 0 || snap.Handlers[0].DurationCount == 0 {
		t.Fatalf("handler metrics = %+v, want calls and duration", snap.Handlers)
	}
	if len(snap.CustomMetrics) == 0 || snap.CustomMetrics[0].Name != "auth.attempts" || snap.CustomMetrics[0].Count == 0 || snap.CustomMetrics[0].Labels["result"] != "fixture_accept" {
		t.Fatalf("custom metrics = %+v, want auth.attempts summary", snap.CustomMetrics)
	}
	handlerTraceFound := false
	for _, trace := range snap.Traces {
		if trace.Status == "ok" && strings.Contains(trace.Operation, "plugin.handler") {
			handlerTraceFound = true
			break
		}
	}
	if !handlerTraceFound {
		t.Fatalf("traces = %+v, want successful plugin handler trace", snap.Traces)
	}
	data, summary, err := manager.DiagnosticPackage(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("DiagnosticPackage() error = %v", err)
	}
	if bytes.Contains(data, []byte("secret-value")) {
		t.Fatalf("diagnostic leaked sensitive content: %s", data)
	}
	var diagnostic map[string]any
	if err := json.Unmarshal(data, &diagnostic); err != nil {
		t.Fatalf("diagnostic is not JSON: %v\n%s", err, data)
	}
	operationsSection, ok := diagnostic["operations"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostic operations section = %#v, want object", diagnostic["operations"])
	}
	for _, section := range []string{"handlers", "events", "custom_metrics", "traces"} {
		if _, ok := operationsSection[section]; !ok {
			t.Fatalf("diagnostic operations section missing %q: %#v", section, operationsSection)
		}
	}
	for _, section := range []string{"plugin_state", "dispatch_summary", "recent_errors", "trace_summary", "event_summary", "metric_summary", "runbook"} {
		if _, ok := diagnostic[section]; !ok {
			t.Fatalf("diagnostic missing top-level %q: %#v", section, diagnostic)
		}
	}
	pluginSection, ok := diagnostic["plugin"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostic plugin section = %#v, want object", diagnostic["plugin"])
	}
	configJSON, _ := pluginSection["config_json"].(string)
	if !strings.Contains(configJSON, "[REDACTED]") || strings.Contains(configJSON, "plain-token-value") {
		t.Fatalf("diagnostic config_json = %q, want redacted config", configJSON)
	}
	for _, forbidden := range []string{"plain-token-value", "user:pass@example.test", "packet-payload-raw"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("diagnostic leaked %q: %s", forbidden, data)
		}
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[REDACTED]")) {
		t.Fatalf("diagnostic collapsed to global redaction: %s", data)
	}
	runbook, ok := diagnostic["runbook"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostic runbook section = %#v, want object", diagnostic["runbook"])
	}
	actions, ok := runbook["actions"].([]any)
	if !ok || len(actions) == 0 {
		t.Fatalf("diagnostic runbook actions = %#v, want actions", runbook["actions"])
	}
	if !containsString(summary.Sections, "runbook") {
		t.Fatalf("diagnostic summary sections = %+v, want runbook", summary.Sections)
	}
	for _, section := range []string{"plugin", "plugin_state", "operations", "recent_operations", "dispatch_summary", "recent_errors", "trace_summary", "event_summary", "metric_summary", "runbook"} {
		if !containsString(summary.Sections, section) {
			t.Fatalf("diagnostic summary sections = %+v, want %s", summary.Sections, section)
		}
	}
	diagnosticRecords, err := manager.repo.ListDiagnosticRecords(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListDiagnosticRecords() error = %v", err)
	}
	if len(diagnosticRecords) != 1 {
		t.Fatalf("diagnostic records = %+v, want one package", diagnosticRecords)
	}
	if _, err := os.Stat(diagnosticRecords[0].Path); err != nil {
		t.Fatalf("Stat(diagnostic package) error = %v", err)
	}
	candidates, err := manager.RunOperationsGC(context.Background(), "admin", "plugin-a", true)
	if err != nil {
		t.Fatalf("RunOperationsGC(diagnostic dry-run) error = %v", err)
	}
	protectedDiagnosticFound := false
	for _, candidate := range candidates {
		if candidate.Kind == "diagnostic_package" && candidate.ID == fmt.Sprintf("%d", diagnosticRecords[0].ID) {
			protectedDiagnosticFound = candidate.Protected && candidate.Reason == "diagnostic package retained" && candidate.Path == diagnosticRecords[0].Path
			break
		}
	}
	if !protectedDiagnosticFound {
		t.Fatalf("gc candidates = %+v, want retained diagnostic package protected", candidates)
	}
	expiredAt := time.Now().Add(-DefaultDiagnosticRetention - time.Second).Unix()
	if _, err := manager.repo.db.ExecContext(context.Background(), `UPDATE plugin_diagnostics SET created_at = ? WHERE id = ?`, expiredAt, diagnosticRecords[0].ID); err != nil {
		t.Fatalf("expire diagnostic package error = %v", err)
	}
	removed, err := manager.RunOperationsGC(context.Background(), "admin", "plugin-a", false)
	if err != nil {
		t.Fatalf("RunOperationsGC(diagnostic apply) error = %v", err)
	}
	removedDiagnosticFound := false
	for _, candidate := range removed {
		if candidate.Kind == "diagnostic_package" && candidate.ID == fmt.Sprintf("%d", diagnosticRecords[0].ID) && candidate.Reason == "diagnostic package retention expired" {
			removedDiagnosticFound = true
			break
		}
	}
	if !removedDiagnosticFound {
		t.Fatalf("removed gc candidates = %+v, want expired diagnostic package removed", removed)
	}
	if _, err := os.Stat(diagnosticRecords[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(expired diagnostic package) error = %v, want not exist", err)
	}
	remainingDiagnostics, err := manager.repo.ListDiagnostics(context.Background(), "plugin-a", 20)
	if err != nil {
		t.Fatalf("ListDiagnostics(after gc) error = %v", err)
	}
	if len(remainingDiagnostics) != 0 {
		t.Fatalf("diagnostics after gc = %+v, want empty", remainingDiagnostics)
	}
}

func TestManagerOperationsRejectsUndeclaredAndHighCardinalityEvents(t *testing.T) {
	var gateway *Gateway
	manager := newManagerForTest(t, &fakeAdapter{init: func(g *Gateway) { gateway = g }})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Events = []EventSpec{{Name: "auth.failure", Fields: []string{"result"}}}
		manifest.CustomMetrics = []MetricSpec{{Name: "auth.attempts", Type: "counter", Labels: []string{"result"}}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if err := gateway.EmitEvent(context.Background(), "auth.success", map[string]string{"result": "ok"}); err == nil {
		t.Fatal("EmitEvent(undeclared) error = nil")
	}
	var highCardinalityErr error
	for i := 0; i < 70; i++ {
		highCardinalityErr = gateway.EmitEvent(context.Background(), "auth.failure", map[string]string{"result": fmt.Sprintf("result-%02d", i)})
		if highCardinalityErr != nil {
			break
		}
	}
	if highCardinalityErr == nil {
		t.Fatal("EmitEvent(high-cardinality) error = nil")
	}
	if err := gateway.ObserveMetric(context.Background(), "auth.attempts", 1, map[string]string{"token": "secret"}); err == nil {
		t.Fatal("ObserveMetric(sensitive label) error = nil")
	}
	if err := gateway.ObserveMetric(context.Background(), "auth.attempts", 1, map[string]string{"result": strings.Repeat("x", DefaultLabelValueMaxBytes+1)}); err == nil {
		t.Fatal("ObserveMetric(oversized label) error = nil")
	}
	if err := gateway.ObserveMetric(context.Background(), "auth.undeclared", 1, map[string]string{"result": "ok"}); err == nil {
		t.Fatal("ObserveMetric(undeclared metric) error = nil")
	}
}

func TestManagerOperationsBackgroundTaskDataQuotaExternalAndGC(t *testing.T) {
	var gateway *Gateway
	manager := newManagerForTest(t, &fakeAdapter{init: func(g *Gateway) {
		gateway = g
		_ = g.RegisterBackgroundTask(api.BackgroundTask{
			ID:      "sync",
			Manual:  true,
			Timeout: time.Second,
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		})
	}})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{ID: "sync", Mode: "manual", Manual: true, Timeout: "1s"}}
		manifest.DataStores = []DataStoreSpec{{Name: "cache", SchemaVersion: 1, QuotaBytes: 8, DataClass: "cache"}}
		manifest.FileStores = []FileStoreSpec{{Namespace: "cache", QuotaBytes: 8, DataClass: "cache"}}
		manifest.ExternalDeps = []ExternalSpec{{Name: "session", Endpoint: "http://127.0.0.1:1", Purpose: "auth", Timeout: "20ms", Required: true}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	start := time.Now()
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", manager.operations.plugins["plugin-a"].tasks["sync"].confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("background task trigger blocked too long")
	}
	taskRuntime := manager.operations.plugins["plugin-a"].tasks["sync"]
	waitForPluginManagerTest(t, func() bool {
		return taskRuntime.summary().Running
	})
	if _, err := manager.CancelBackgroundTask(context.Background(), "admin", "plugin-a", "sync"); err != nil {
		t.Fatalf("CancelBackgroundTask() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return !taskRuntime.summary().Running
	})
	if err := gateway.DataStore().Put(context.Background(), api.DataRecord{Key: "one", Value: []byte("12345678"), SchemaVersion: 1}); err != nil {
		t.Fatalf("DataStore.Put(within quota) error = %v", err)
	}
	if err := gateway.DataStore().Put(context.Background(), api.DataRecord{Key: "two", Value: []byte("x")}); err == nil {
		t.Fatal("DataStore.Put(over quota) error = nil")
	}
	if err := gateway.FileStore().Write(context.Background(), "cache", "state.json", []byte("12345678"), "cache", time.Hour); err != nil {
		t.Fatalf("FileStore.Write(within quota) error = %v", err)
	}
	if err := gateway.FileStore().Write(context.Background(), "cache", "state.json", []byte("abcdefgh"), "cache", time.Hour); err != nil {
		t.Fatalf("FileStore.Write(overwrite same size) error = %v", err)
	}
	if err := gateway.FileStore().Write(context.Background(), "cache", "other.json", []byte("x"), "cache", time.Hour); err == nil {
		t.Fatal("FileStore.Write(over quota) error = nil")
	}
	if _, err := gateway.ExternalClient("session").DoHTTP(context.Background(), api.ExternalRequest{Method: "GET", URL: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("ExternalClient.DoHTTP() error = nil, want connection error")
	}
	health, err := manager.HealthCheckExternalDependency(context.Background(), "admin", "plugin-a", "session")
	if err != nil {
		t.Fatalf("HealthCheckExternalDependency() error = %v", err)
	}
	if health.OK || health.Error == "" || health.Summary.Name != "session" || health.Summary.Errors == 0 || health.Summary.LastStatus != "health_failed" {
		t.Fatalf("external health = %+v, want failed session health summary", health)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "plugin-a", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "external_dependency_fail_closed_unhealthy") {
		t.Fatalf("governance decision = %+v, want fail-closed external dependency block", decision)
	}
	snap, err := manager.OperationsSnapshot(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("OperationsSnapshot() error = %v", err)
	}
	if len(snap.ExternalDependencies) == 0 || snap.ExternalDependencies[0].Errors == 0 {
		t.Fatalf("external summaries = %+v, want error count", snap.ExternalDependencies)
	}
	if err := manager.repo.PutPluginData(context.Background(), PluginDataSummary{PluginID: "plugin-a", Key: "expired", SizeBytes: 3, ExpiresAt: time.Now().Add(-time.Second).Unix()}, []byte("old")); err != nil {
		t.Fatalf("PutPluginData(expired) error = %v", err)
	}
	expiredPath := filepath.Join(manager.operations.runtimeRoot(), "plugin-a", "cache", "expired.json")
	orphanPath := filepath.Join(manager.operations.runtimeRoot(), "plugin-a", "cache", "orphan.json")
	protectedPath := filepath.Join(manager.operations.runtimeRoot(), "plugin-a", "cache", "state.json")
	if err := os.MkdirAll(filepath.Dir(expiredPath), 0755); err != nil {
		t.Fatalf("MkdirAll(runtime cache) error = %v", err)
	}
	if err := os.WriteFile(expiredPath, []byte("old"), 0644); err != nil {
		t.Fatalf("WriteFile(expired plugin file) error = %v", err)
	}
	if err := manager.repo.UpsertPluginFile(context.Background(), PluginFileSummary{
		PluginID:  "plugin-a",
		Namespace: "cache",
		Path:      "expired.json",
		DataClass: "cache",
		SizeBytes: 3,
		ExpiresAt: time.Now().Add(-time.Second).Unix(),
	}, expiredPath); err != nil {
		t.Fatalf("UpsertPluginFile(expired) error = %v", err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0644); err != nil {
		t.Fatalf("WriteFile(orphan plugin file) error = %v", err)
	}
	diagnosticPath := filepath.Join(manager.operations.runtimeRoot(), "diagnostics", "plugin-a", "expired.json")
	if err := os.MkdirAll(filepath.Dir(diagnosticPath), 0755); err != nil {
		t.Fatalf("MkdirAll(diagnostic dir) error = %v", err)
	}
	if err := os.WriteFile(diagnosticPath, []byte(`{"plugin":"plugin-a"}`), 0600); err != nil {
		t.Fatalf("WriteFile(expired diagnostic package) error = %v", err)
	}
	if _, err := manager.repo.SaveDiagnostic(context.Background(), "plugin-a", diagnosticPath, 21, []string{"plugin"}); err != nil {
		t.Fatalf("SaveDiagnostic(expired) error = %v", err)
	}
	diagnosticRecords, err := manager.repo.ListDiagnosticRecords(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListDiagnosticRecords() error = %v", err)
	}
	expiredDiagnosticID := ""
	for _, record := range diagnosticRecords {
		if record.Path != diagnosticPath {
			continue
		}
		expiredDiagnosticID = fmt.Sprintf("%d", record.ID)
		expiredAt := time.Now().Add(-DefaultDiagnosticRetention - time.Second).Unix()
		if _, err := manager.repo.db.ExecContext(context.Background(), `UPDATE plugin_diagnostics SET created_at = ? WHERE id = ?`, expiredAt, record.ID); err != nil {
			t.Fatalf("expire diagnostic package error = %v", err)
		}
		break
	}
	if expiredDiagnosticID == "" {
		t.Fatalf("diagnostic records = %+v, want expired package record", diagnosticRecords)
	}
	for i := 0; i < DefaultEventRecentLimit+2; i++ {
		if err := manager.repo.SaveEvent(context.Background(), EventSummary{
			PluginID: "plugin-a",
			Name:     "retention.event",
			Fields:   map[string]string{"result": "ok"},
		}, false, "", "", ""); err != nil {
			t.Fatalf("SaveEvent(%d) error = %v", i, err)
		}
	}
	for i := 0; i < DefaultLogRecentLimit+2; i++ {
		if err := manager.repo.SaveLog(context.Background(), LogSummary{PluginID: "plugin-a", Level: "info", Message: "retention log"}); err != nil {
			t.Fatalf("SaveLog(%d) error = %v", i, err)
		}
	}
	for i := 0; i < DefaultEventRecentLimit+2; i++ {
		if err := manager.repo.SaveTrace(context.Background(), TraceSummary{
			PluginID:   "plugin-a",
			TraceID:    fmt.Sprintf("trace-%d", i),
			Operation:  "retention.trace",
			Status:     "ok",
			DurationMS: 1,
		}, map[string]string{"result": "ok"}); err != nil {
			t.Fatalf("SaveTrace(%d) error = %v", i, err)
		}
	}
	if _, _, err := manager.repo.AcquireTaskLease(context.Background(), "plugin-a", "expired-sync", "shard-a", "node-a", time.Second); err != nil {
		t.Fatalf("AcquireTaskLease(expired-sync) error = %v", err)
	}
	if _, err := manager.repo.db.ExecContext(context.Background(), `
UPDATE plugin_task_leases
SET expires_at = ?, updated_at = ?
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`,
		time.Now().Add(-time.Second).Unix(), time.Now().Add(-time.Second).Unix(), "plugin-a", "expired-sync", "shard-a"); err != nil {
		t.Fatalf("expire task lease error = %v", err)
	}
	candidates, err := manager.RunOperationsGC(context.Background(), "admin", "plugin-a", true)
	if err != nil {
		t.Fatalf("RunOperationsGC(dry-run) error = %v", err)
	}
	found := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.Kind == "retention_rule" && candidate.RetentionRule != "" {
			found["retention_rule:"+candidate.Category] = true
		}
		if candidate.Kind == "plugin_data" && candidate.ID == "expired" && candidate.SizeBytes > 0 && !candidate.Protected && candidate.Reason != "" {
			found["plugin_data"] = true
		}
		if candidate.Kind == "plugin_file" && candidate.ID == "cache/expired.json" && candidate.Path == expiredPath && candidate.SizeBytes > 0 && !candidate.Protected && candidate.Reason != "" {
			found["plugin_file"] = true
		}
		if candidate.Kind == "plugin_file_orphan" && candidate.Path == orphanPath && candidate.SizeBytes > 0 && !candidate.Protected && candidate.Reason != "" {
			found["plugin_file_orphan"] = true
		}
		if candidate.Kind == "diagnostic_package" && candidate.ID == expiredDiagnosticID && candidate.Path == diagnosticPath && candidate.SizeBytes > 0 && !candidate.Protected && candidate.Reason == "diagnostic package retention expired" {
			found["diagnostic_package"] = true
		}
		if candidate.Kind == "event" && candidate.Category == "event" && candidate.SizeBytes > 0 && candidate.Reason != "" {
			found["event"] = true
		}
		if candidate.Kind == "plugin_log" && candidate.Category == "log" && candidate.SizeBytes > 0 && candidate.Reason != "" {
			found["plugin_log"] = true
		}
		if candidate.Kind == "trace" && candidate.Category == "trace" && candidate.SizeBytes > 0 && candidate.Reason != "" {
			found["trace"] = true
		}
		if candidate.Kind == "background_task_lease" && candidate.ID == "expired-sync/shard-a" && candidate.ExpiresAt > 0 && !candidate.Protected {
			found["background_task_lease"] = true
		}
	}
	if !found["plugin_data"] || !found["plugin_file"] || !found["plugin_file_orphan"] {
		t.Fatalf("gc candidates = %+v, want expired data/file and orphan file", candidates)
	}
	if !found["diagnostic_package"] {
		t.Fatalf("gc candidates = %+v, want expired diagnostic package candidate", candidates)
	}
	for _, category := range []string{"diagnostic", "event", "metric", "trace", "background_task", "data", "file"} {
		if !found["retention_rule:"+category] {
			t.Fatalf("gc candidates = %+v, want retention rule for %s", candidates, category)
		}
	}
	for _, kind := range []string{"event", "plugin_log", "trace", "background_task_lease"} {
		if !found[kind] {
			t.Fatalf("gc candidates = %+v, want %s candidate", candidates, kind)
		}
	}
	removed, err := manager.RunOperationsGC(context.Background(), "admin", "plugin-a", false)
	if err != nil {
		t.Fatalf("RunOperationsGC(apply) error = %v", err)
	}
	removedKinds := map[string]bool{}
	for _, candidate := range removed {
		removedKinds[candidate.Kind+":"+candidate.ID] = true
	}
	if !removedKinds["plugin_data:expired"] ||
		!removedKinds["plugin_file:cache/expired.json"] ||
		!removedKinds["plugin_file_orphan:plugin-a/cache/orphan.json"] ||
		!removedKinds["diagnostic_package:"+expiredDiagnosticID] ||
		!removedKinds["background_task_lease:expired-sync/shard-a"] ||
		!removedKinds["event:plugin-a/overflow:1000"] ||
		!removedKinds["plugin_log:plugin-a/overflow:500"] ||
		!removedKinds["trace:plugin-a/overflow:1000"] {
		t.Fatalf("removed gc candidates = %+v, want expired data/file/orphan/lease and event/log/trace overflow removed", removed)
	}
	if _, _, err := manager.repo.GetPluginData(context.Background(), "plugin-a", "expired"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPluginData(expired) error = %v, want sql.ErrNoRows", err)
	}
	files, err := manager.repo.ListPluginFiles(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListPluginFiles() error = %v", err)
	}
	protectedFound := false
	for _, file := range files {
		if file.Namespace == "cache" && file.Path == "expired.json" {
			t.Fatalf("plugin files = %+v, expired file record still present", files)
		}
		if file.Namespace == "cache" && file.Path == "state.json" {
			protectedFound = true
		}
	}
	if !protectedFound {
		t.Fatalf("plugin files = %+v, want protected state.json retained", files)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(expired file) error = %v, want not exist", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(orphan file) error = %v, want not exist", err)
	}
	if _, err := os.Stat(diagnosticPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(expired diagnostic package) error = %v, want not exist", err)
	}
	if _, err := os.Stat(protectedPath); err != nil {
		t.Fatalf("Stat(protected file) error = %v, want retained", err)
	}
	operations, err := manager.repo.ListOperations(context.Background(), "plugin-a", 10)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	auditFound := false
	for _, operation := range operations {
		if operation.Operation == "plugin_operations_gc" && operation.Status == "succeeded" {
			auditFound = true
			break
		}
	}
	if !auditFound {
		t.Fatalf("operations = %+v, want plugin_operations_gc succeeded audit", operations)
	}
}

func TestManagerDisableStopsBackgroundTask(t *testing.T) {
	var started atomic.Int32
	manager := newManagerForTest(t, &fakeAdapter{init: func(g *Gateway) {
		_ = g.RegisterBackgroundTask(api.BackgroundTask{
			ID:      "sync",
			Manual:  true,
			Timeout: 5 * time.Second,
			Run: func(ctx context.Context) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		})
	}})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{
			ID:      "sync",
			Mode:    "manual",
			Manual:  true,
			Timeout: "5s",
		}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	task := manager.operations.plugins["plugin-a"].tasks["sync"]
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", task.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return started.Load() == 1 && task.summary().Running
	})
	if _, err := manager.Disable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return !task.summary().Running
	})
	if summary := task.summary(); !strings.Contains(summary.LastError, "context canceled") {
		t.Fatalf("task summary after disable = %+v, want cancellation error", summary)
	}
}

func TestManagerBackgroundTaskRetriesFailedRun(t *testing.T) {
	var attempts atomic.Int32
	manager := newManagerForTest(t, &fakeAdapter{init: func(g *Gateway) {
		_ = g.RegisterBackgroundTask(api.BackgroundTask{
			ID:      "retry-sync",
			Manual:  true,
			Timeout: time.Second,
			Run: func(context.Context) error {
				if attempts.Add(1) < 3 {
					return errors.New("temporary token=secret")
				}
				return nil
			},
		})
	}})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{ID: "retry-sync", Mode: "manual", Manual: true, Timeout: "1s", Retry: 2}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	task := manager.operations.plugins["plugin-a"].tasks["retry-sync"]
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "retry-sync", task.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return attempts.Load() == 3 && !task.summary().Running
	})
	summary := task.summary()
	if summary.Retry != 2 || summary.LastAttempts != 3 || summary.ConsecutiveFailures != 0 || summary.LastError != "" {
		t.Fatalf("task summary = %+v, want retried success after three attempts", summary)
	}
}

func TestManagerRejectsInvalidExternalDependencyPolicy(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	var manifest Manifest
	if err := json.Unmarshal(testManifestBytesWithCapabilities(t, "bad-external-policy", nil), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.ExternalDeps = []ExternalSpec{{
		Name:       "session",
		Endpoint:   "https://example.test/session",
		Purpose:    "auth",
		Required:   true,
		FailPolicy: "fail_sideways",
	}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	_, err = manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: writeTestMCGP(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes"),
		}),
		FileName: "bad-external-policy.mcgp",
		Actor:    "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "fail_policy") {
		t.Fatalf("UploadArtifact(invalid fail_policy) error = %v, want validation failure", err)
	}
}

func TestManagerExternalVulnerabilityFeedSyncAndScheduler(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	feedPath := filepath.Join(t.TempDir(), "vulnerabilities.json")
	if err := os.WriteFile(feedPath, []byte(`{
		"source": "unit-feed",
		"vulnerabilities": [{
			"vulnerability_id": "CVE-2026-0001",
			"package_name": "example.org/session",
			"version_range": "<1.2.3",
			"severity": "critical",
			"action": "denylist",
			"summary": "test vulnerability"
		}]
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(feed) error = %v", err)
	}
	result, err := manager.SyncExternalVulnerabilityFeed(context.Background(), "admin", ExternalFeedSchedule{URL: feedPath})
	if err != nil {
		t.Fatalf("SyncExternalVulnerabilityFeed() error = %v", err)
	}
	if result.Source != "unit-feed" || result.Imported != 1 {
		t.Fatalf("external vulnerability feed result = %+v, want unit-feed import", result)
	}
	records, err := manager.ListVulnerabilities(context.Background(), "example.org/session")
	if err != nil {
		t.Fatalf("ListVulnerabilities() error = %v", err)
	}
	if len(records) != 1 || records[0].VulnerabilityID != "CVE-2026-0001" || records[0].Severity != VulnerabilitySeverityCritical {
		t.Fatalf("vulnerability records = %+v, want imported critical record", records)
	}

	scheduledPath := filepath.Join(t.TempDir(), "scheduled-vulnerabilities.json")
	if err := os.WriteFile(scheduledPath, []byte(`[{
		"vulnerability_id": "CVE-2026-0002",
		"package_name": "example.org/cache",
		"version_range": "<2.0.0",
		"severity": "high",
		"action": "quarantine"
	}]`), 0o600); err != nil {
		t.Fatalf("WriteFile(scheduled feed) error = %v", err)
	}
	if err := manager.StartExternalFeedSchedulers(context.Background(), nil, []ExternalFeedSchedule{{
		Name:       "scheduled-feed",
		URL:        scheduledPath,
		Source:     "scheduled-unit-feed",
		RunOnStart: true,
		Interval:   time.Hour,
	}}); err != nil {
		t.Fatalf("StartExternalFeedSchedulers() error = %v", err)
	}
	defer manager.StopExternalFeedSchedulers()
	waitForPluginManagerTest(t, func() bool {
		records, err := manager.ListVulnerabilities(context.Background(), "example.org/cache")
		return err == nil && len(records) == 1 && records[0].VulnerabilityID == "CVE-2026-0002"
	})
}

func TestManagerBackgroundTaskSingletonLeaseSkipsSecondNode(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	taskAdapter := func(started chan<- struct{}) *fakeAdapter {
		return &fakeAdapter{init: func(g *Gateway) {
			_ = g.RegisterBackgroundTask(api.BackgroundTask{
				ID:      "sync",
				Manual:  true,
				Timeout: 5 * time.Second,
				Run: func(ctx context.Context) error {
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
		}}
	}
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      taskAdapter(firstStarted),
		NodeID:       "node-a",
	})
	artifact := uploadTestArtifactWithManifest(t, first, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{
			ID:        "sync",
			Mode:      "manual",
			Manual:    true,
			Timeout:   "5s",
			RunPolicy: TaskRunPolicySingleton,
			LeaseTTL:  "5s",
		}}
	})
	if _, err := first.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := first.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(first) error = %v", err)
	}
	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      taskAdapter(secondStarted),
		NodeID:       "node-b",
	})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(second) error = %v", err)
	}

	firstTask := first.operations.plugins["plugin-a"].tasks["sync"]
	secondTask := second.operations.plugins["plugin-a"].tasks["sync"]
	if _, err := first.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", firstTask.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask(first) error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		summary := firstTask.summary()
		return summary.Running && summary.LeaseAcquired && summary.LeaseOwner == "node-a" && len(firstStarted) == 1
	})
	gcCandidates, err := second.RunOperationsGC(context.Background(), "admin", "plugin-a", true)
	if err != nil {
		t.Fatalf("RunOperationsGC(second dry-run) error = %v", err)
	}
	protectedLeaseFound := false
	for _, candidate := range gcCandidates {
		if candidate.Kind == "background_task_lease" && candidate.ID == "sync/global" && candidate.Protected && candidate.Reason == "background task lease retained" {
			protectedLeaseFound = true
			break
		}
	}
	if !protectedLeaseFound {
		t.Fatalf("gc candidates = %+v, want node-a singleton lease protected on node-b dry-run", gcCandidates)
	}
	removed, err := second.RunOperationsGC(context.Background(), "admin", "plugin-a", false)
	if err != nil {
		t.Fatalf("RunOperationsGC(second apply) error = %v", err)
	}
	for _, candidate := range removed {
		if candidate.Kind == "background_task_lease" && candidate.ID == "sync/global" {
			t.Fatalf("removed gc candidates = %+v, active node-a lease must be protected", removed)
		}
	}
	leases, err := first.repo.ListTaskLeases(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListTaskLeases() error = %v", err)
	}
	if len(leases) != 1 || leases[0].OwnerNodeID != "node-a" || leases[0].TaskID != "sync" || leases[0].ShardKey != "global" {
		t.Fatalf("task leases after node-b gc = %+v, want active node-a singleton lease retained", leases)
	}
	if _, err := second.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", secondTask.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask(second) error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		summary := secondTask.summary()
		return !summary.Running && summary.LeaseRequired && summary.LeaseSkipped == 1 && summary.LeaseOwner == "node-a"
	})
	select {
	case <-secondStarted:
		t.Fatal("second node ran singleton task while first node held the lease")
	default:
	}
	close(release)
	waitForPluginManagerTest(t, func() bool {
		return !firstTask.summary().Running
	})
}

func TestManagerBackgroundTaskRenewsSingletonLease(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		NodeID:       "node-a",
		Adapter: &fakeAdapter{init: func(g *Gateway) {
			_ = g.RegisterBackgroundTask(api.BackgroundTask{
				ID:      "sync",
				Manual:  true,
				Timeout: 5 * time.Second,
				Run: func(ctx context.Context) error {
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
		}},
	})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{
			ID:        "sync",
			Mode:      "manual",
			Manual:    true,
			Timeout:   "5s",
			RunPolicy: TaskRunPolicySingleton,
			LeaseTTL:  "2s",
		}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	task := manager.operations.plugins["plugin-a"].tasks["sync"]
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", task.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	var firstExpires int64
	waitForPluginManagerTest(t, func() bool {
		summary := task.summary()
		if summary.Running && summary.LeaseAcquired && summary.LeaseOwner == "node-a" && summary.LeaseExpiresAt > 0 {
			firstExpires = summary.LeaseExpiresAt
			return true
		}
		return false
	})
	waitForPluginManagerTest(t, func() bool {
		summary := task.summary()
		return summary.Running && summary.LeaseAcquired && summary.LeaseExpiresAt > firstExpires
	})
	close(release)
	waitForPluginManagerTest(t, func() bool {
		return !task.summary().Running
	})
}

func TestManagerBackgroundTaskCancelsWhenSingletonLeaseLost(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		NodeID:       "node-a",
		Adapter: &fakeAdapter{init: func(g *Gateway) {
			_ = g.RegisterBackgroundTask(api.BackgroundTask{
				ID:      "sync",
				Manual:  true,
				Timeout: 5 * time.Second,
				Run: func(ctx context.Context) error {
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
		}},
	})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{{
			ID:        "sync",
			Mode:      "manual",
			Manual:    true,
			Timeout:   "5s",
			RunPolicy: TaskRunPolicySingleton,
			LeaseTTL:  "2s",
		}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	task := manager.operations.plugins["plugin-a"].tasks["sync"]
	if _, err := manager.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync", task.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		summary := task.summary()
		return summary.Running && summary.LeaseAcquired && summary.LeaseOwner == "node-a"
	})
	stolenUntil := time.Now().Add(5 * time.Second).Unix()
	if _, err := manager.repo.db.ExecContext(context.Background(), `
UPDATE plugin_task_leases
SET owner_node_id = ?, expires_at = ?, updated_at = ?
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`,
		"node-b", stolenUntil, time.Now().Unix(), "plugin-a", "sync", "global"); err != nil {
		t.Fatalf("steal lease error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		summary := task.summary()
		return !summary.Running && summary.LeaseOwner == "" && strings.Contains(summary.LastError, "node-b")
	})
	summary := task.summary()
	if summary.ConsecutiveFailures != 1 || summary.LastAttempts != 1 {
		t.Fatalf("task summary = %+v, want one failed attempt after lease loss", summary)
	}
}

func TestManagerBackgroundTaskShardedLeaseRunsAcrossNodes(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	taskAdapter := func(taskID string, started chan<- struct{}) *fakeAdapter {
		return &fakeAdapter{init: func(g *Gateway) {
			_ = g.RegisterBackgroundTask(api.BackgroundTask{
				ID:      taskID,
				Manual:  true,
				Timeout: 5 * time.Second,
				Run: func(ctx context.Context) error {
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
		}}
	}
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      taskAdapter("sync-a", firstStarted),
		NodeID:       "node-a",
	})
	artifact := uploadTestArtifactWithManifest(t, first, "plugin-a", func(manifest *Manifest) {
		manifest.BackgroundTasks = []TaskSpec{
			{ID: "sync-a", Mode: "manual", Manual: true, Timeout: "5s", RunPolicy: TaskRunPolicySharded, ShardKey: "shard-a", LeaseTTL: "5s"},
			{ID: "sync-b", Mode: "manual", Manual: true, Timeout: "5s", RunPolicy: TaskRunPolicySharded, ShardKey: "shard-b", LeaseTTL: "5s"},
		}
	})
	if _, err := first.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := first.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(first) error = %v", err)
	}
	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      taskAdapter("sync-b", secondStarted),
		NodeID:       "node-b",
	})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(second) error = %v", err)
	}
	firstTask := first.operations.plugins["plugin-a"].tasks["sync-a"]
	secondTask := second.operations.plugins["plugin-a"].tasks["sync-b"]
	if _, err := first.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync-a", firstTask.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask(first) error = %v", err)
	}
	if _, err := second.TriggerBackgroundTask(context.Background(), "admin", "plugin-a", "sync-b", secondTask.confirmToken); err != nil {
		t.Fatalf("TriggerBackgroundTask(second) error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		firstSummary := firstTask.summary()
		secondSummary := secondTask.summary()
		return firstSummary.Running && firstSummary.LeaseAcquired && firstSummary.LeaseOwner == "node-a" && firstSummary.ShardKey == "shard-a" &&
			secondSummary.Running && secondSummary.LeaseAcquired && secondSummary.LeaseOwner == "node-b" && secondSummary.ShardKey == "shard-b" &&
			len(firstStarted) == 1 && len(secondStarted) == 1
	})
}

func TestManagerPluginNodeRuntimeStateAndPartialRollout(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	service, err := manager.PluginServiceStatus(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceStatus() error = %v", err)
	}
	if len(service.Nodes) != 1 || service.Nodes[0].NodeID == "" || service.Nodes[0].HeartbeatAt == 0 || service.Nodes[0].Stale {
		t.Fatalf("plugin service nodes = %+v, want current node heartbeat", service.Nodes)
	}
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	enabled, err := manager.Enable(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	rollout, err := manager.PluginRolloutStatus(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("PluginRolloutStatus() error = %v", err)
	}
	if !rollout.OK || rollout.PartialFailure || rollout.NodesReady != 1 || len(rollout.NodeRuntimeStates) != 1 {
		t.Fatalf("rollout = %+v, want one ready node", rollout)
	}
	if !rollout.ArtifactDistribution ||
		rollout.ArtifactDistributionMode != "local-content-store" ||
		rollout.ArtifactDistributionStatus != "available" ||
		rollout.ArtifactPackageSHA256 == "" ||
		rollout.CrossNodeApply {
		t.Fatalf("rollout distribution = %+v, want local package available without cross-node apply", rollout)
	}
	if !manager.store.HasDistributionPackage(artifact) {
		t.Fatalf("distribution package missing for artifact %s", artifact.ID)
	}
	state := rollout.NodeRuntimeStates[0]
	if state.PluginID != "plugin-a" ||
		state.ArtifactID != artifact.ID ||
		state.RuntimeState != RuntimeEnabled ||
		state.AppliedGeneration != enabled.DesiredGeneration ||
		!state.Enabled {
		t.Fatalf("node runtime state = %+v, want enabled artifact", state)
	}

	if err := manager.repo.UpsertPluginNode(context.Background(), PluginNodeState{
		NodeID:        "node-b",
		Hostname:      "gateway-b",
		PID:           222,
		ServiceMode:   PluginServiceModeInProcess,
		DataPlaneMode: PluginServiceModeInProcess,
		Status:        PluginNodeStatusOnline,
		StartedAt:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertPluginNode(node-b) error = %v", err)
	}
	if err := manager.repo.UpsertPluginNodeRuntime(context.Background(), PluginNodeRuntimeState{
		NodeID:            "node-b",
		PluginID:          "plugin-a",
		ArtifactID:        "stale-artifact",
		DesiredState:      DesiredEnabled,
		RuntimeState:      RuntimeFailed,
		DesiredGeneration: enabled.DesiredGeneration,
		AppliedGeneration: 0,
		Health:            RuntimeFailed,
		Error:             "load failed",
	}); err != nil {
		t.Fatalf("UpsertPluginNodeRuntime(node-b) error = %v", err)
	}
	rollout, err = manager.PluginRolloutStatus(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("PluginRolloutStatus(after node-b) error = %v", err)
	}
	if rollout.OK || !rollout.PartialFailure || rollout.NodesFailed != 1 || rollout.NodesReady != 1 || rollout.NodesTotal != 2 {
		t.Fatalf("rollout after node-b = %+v, want partial failure", rollout)
	}
}

func TestManagerRollbackConfigSnapshotRunsDryRunBeforeChangingDesired(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"version":1}`, 10); err != nil {
		t.Fatalf("SetDesired(v1) error = %v", err)
	}
	updated, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"version":2}`, 20)
	if err != nil {
		t.Fatalf("SetDesired(v2) error = %v", err)
	}
	snapshots, err := manager.ListConfigSnapshots(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListConfigSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	adapter.dryRunErrs = map[string]error{"plugin-a": errors.New("rollback rejected")}
	if _, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, false); err == nil {
		t.Fatal("RollbackConfigSnapshot() error = nil, want dry-run rejection")
	}
	afterFail, _ := manager.Plugin(context.Background(), "plugin-a")
	if afterFail.DesiredGeneration != updated.DesiredGeneration || afterFail.ConfigJSON != `{"version":2}` {
		t.Fatalf("plugin after failed rollback = %+v, want unchanged %+v", afterFail, updated)
	}
	adapter.dryRunErrs = nil
	rolledBack, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, false)
	if err != nil {
		t.Fatalf("RollbackConfigSnapshot(success) error = %v", err)
	}
	if rolledBack.ConfigJSON != `{"version":1}` || rolledBack.Priority != 20 {
		t.Fatalf("rolled back plugin = %+v, want config v1 and current priority 20", rolledBack)
	}
}

func TestManagerRollbackConfigSnapshotFullDesiredRestoresArtifactState(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	oldArtifact := uploadTestArtifact(t, manager, "plugin-a")
	newArtifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", oldArtifact.ID, DesiredDisabled, `{"version":1}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", newArtifact.ID, DesiredEnabled, `{"version":2}`, 20); err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	snapshots, err := manager.ListConfigSnapshots(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListConfigSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	rolledBack, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, true)
	if err != nil {
		t.Fatalf("RollbackConfigSnapshot(full desired) error = %v", err)
	}
	if rolledBack.DesiredArtifactID != oldArtifact.ID || rolledBack.DesiredState != DesiredDisabled || rolledBack.Priority != 10 || rolledBack.ConfigJSON != `{"version":1}` {
		t.Fatalf("rolled back plugin = %+v, want old artifact/state/priority/config", rolledBack)
	}
}

func TestManagerRollbackConfigSnapshotDiffRedactsSensitiveConfig(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredDisabled, `{"token":"old-secret","host":"old"}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"token":"new-secret","host":"new"}`, 20); err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	snapshots, err := manager.ListConfigSnapshots(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListConfigSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	diff, err := manager.ConfigSnapshotDiff(context.Background(), snapshots[0].ID)
	if err != nil {
		t.Fatalf("ConfigSnapshotDiff() error = %v", err)
	}
	if strings.Contains(diff.RedactedDiffJSON, "old-secret") || strings.Contains(diff.RedactedDiffJSON, "new-secret") {
		t.Fatalf("snapshot diff leaked secret: %+v", diff)
	}
	if !strings.Contains(diff.RedactedDiffJSON, "[REDACTED]") ||
		!strings.Contains(diff.RedactedDiffJSON, "old") ||
		!strings.Contains(diff.RedactedDiffJSON, "new") {
		t.Fatalf("snapshot diff = %s, want redacted secret and visible non-sensitive values", diff.RedactedDiffJSON)
	}
	if _, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, false); err != nil {
		t.Fatalf("RollbackConfigSnapshot(config-only) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"token":"third-secret","host":"third"}`, 30); err != nil {
		t.Fatalf("SetDesired(third) error = %v", err)
	}
	snapshots, err = manager.ListConfigSnapshots(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ListConfigSnapshots(after config-only) error = %v", err)
	}
	if len(snapshots) < 1 {
		t.Fatal("snapshots after config-only rollback = empty")
	}
	diff, err = manager.ConfigSnapshotDiff(context.Background(), snapshots[0].ID)
	if err != nil {
		t.Fatalf("ConfigSnapshotDiff(full candidate) error = %v", err)
	}
	if strings.Contains(diff.RedactedDiffJSON, "third-secret") || strings.Contains(diff.RedactedDiffJSON, "old-secret") {
		t.Fatalf("full rollback snapshot diff leaked secret: %+v", diff)
	}
	if _, err := manager.RollbackConfigSnapshot(context.Background(), "admin", snapshots[0].ID, true); err != nil {
		t.Fatalf("RollbackConfigSnapshot(full desired) error = %v", err)
	}
}

func TestManagerRollbackArtifactDryRunFailureDoesNotChangeDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	oldArtifact := uploadTestArtifactWithManifestBytes(t, manager, "plugin-a", []byte("old plugin bytes"), func(manifest *Manifest) {
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"host":{"type":"number"}}}`)
	})
	newArtifact := uploadTestArtifactWithManifestBytes(t, manager, "plugin-a", []byte("new plugin bytes"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"host":{"type":"string"}}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", oldArtifact.ID, DesiredEnabled, `{"host":1}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(old) error = %v", err)
	}
	before, err := manager.SetDesired(context.Background(), "admin", "plugin-a", newArtifact.ID, DesiredEnabled, `{"host":"new"}`, 20)
	if err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	if _, err := manager.RollbackArtifact(context.Background(), "admin", "plugin-a", oldArtifact.ID); err == nil || !strings.Contains(err.Error(), "$.host must be number") {
		t.Fatalf("RollbackArtifact() error = %v, want dry-run schema rejection", err)
	}
	after, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if after.DesiredArtifactID != before.DesiredArtifactID || after.DesiredGeneration != before.DesiredGeneration || after.ConfigJSON != before.ConfigJSON {
		t.Fatalf("plugin after failed rollback = %+v, want unchanged %+v", after, before)
	}
}

func TestManagerRollbackGateFailureDoesNotChangeDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	oldArtifact := uploadTestArtifactWithManifestBytes(t, manager, "revoke-plugin", []byte("old plugin bytes"), func(manifest *Manifest) {
		manifest.Version = "0.1.0"
	})
	newArtifact := uploadTestArtifactWithManifestBytes(t, manager, "revoke-plugin", []byte("new plugin bytes"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "revoke-plugin", oldArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "revoke-plugin", newArtifact.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	before, err := manager.Plugin(context.Background(), "revoke-plugin")
	if err != nil {
		t.Fatalf("Plugin(before) error = %v", err)
	}
	if _, err := manager.UpsertAdvisory(context.Background(), "admin", AdvisoryRequest{
		AdvisoryID:     "ADV-ROLLBACK-1",
		Status:         AdvisoryStatusRevoked,
		Action:         AdvisoryActionRevoke,
		ArtifactSHA256: oldArtifact.SHA256,
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}
	if _, err := manager.RollbackArtifact(context.Background(), "admin", "revoke-plugin", oldArtifact.ID); err == nil || !strings.Contains(err.Error(), "advisory_revoke") {
		t.Fatalf("RollbackArtifact() error = %v, want advisory_revoke gate block", err)
	}
	after, err := manager.Plugin(context.Background(), "revoke-plugin")
	if err != nil {
		t.Fatalf("Plugin(after) error = %v", err)
	}
	if after.DesiredArtifactID != before.DesiredArtifactID ||
		after.DesiredState != before.DesiredState ||
		after.DesiredGeneration != before.DesiredGeneration ||
		after.ConfigJSON != before.ConfigJSON ||
		after.Priority != before.Priority {
		t.Fatalf("plugin after rollback gate failure = %+v, want unchanged %+v", after, before)
	}
}

func TestManagerEnableDisableAndDispatch(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifact(t, manager, "plugin-a")

	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredDisabled, `{"upstream":"override"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if adapter.loads != 1 {
		t.Fatalf("adapter loads = %d, want 1", adapter.loads)
	}
	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
		Host:     "play.example",
		Upstream: "backend.example:25565",
	})
	if err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream() = %+v, want handled conn", result)
	}

	plugin, err := manager.Disable(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if plugin.RuntimeState != RuntimeDisabled {
		t.Fatalf("disabled runtime state = %q, want %q", plugin.RuntimeState, RuntimeDisabled)
	}
	result, err = manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
		Host:     "play.example",
		Upstream: "backend.example:25565",
	})
	if err != nil {
		t.Fatalf("ConnectUpstream(disabled) error = %v", err)
	}
	if result.Handled {
		t.Fatalf("ConnectUpstream(disabled) = %+v, want pass-through", result)
	}
}

func TestManagerErrPassContinuesToNextHandler(t *testing.T) {
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return nil, api.ErrPass
			},
			"plugin-b": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return newMemoryConn(), nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(a) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-b", artifactB.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(b) error = %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream() = %+v, want second handler conn", result)
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 2 {
		t.Fatalf("dispatch handlers = %d, want 2", len(plan.Handlers))
	}
	if plan.Handlers[0].PluginID != "plugin-a" || plan.Handlers[1].PluginID != "plugin-b" {
		t.Fatalf("dispatch order = %+v, want plugin-a then plugin-b", plan.Handlers)
	}
}

func TestManagerErrBlockedStopsDispatch(t *testing.T) {
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return nil, api.ErrBlocked
			},
			"plugin-b": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return newMemoryConn(), nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(a) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-b", artifactB.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(b) error = %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if !errors.Is(err, api.ErrBlocked) {
		t.Fatalf("ConnectUpstream() error = %v, want ErrBlocked", err)
	}
	if !result.Handled {
		t.Fatalf("ConnectUpstream() = %+v, want handled", result)
	}
	plan := manager.DispatchPlan(context.Background())
	if got := plan.Handlers[0].Blocked; got != 1 {
		t.Fatalf("blocked count = %d, want 1", got)
	}
	if got := plan.Handlers[1].Calls; got != 0 {
		t.Fatalf("second handler calls = %d, want 0", got)
	}
}

func TestManagerPanicDoesNotReplaceExistingDispatch(t *testing.T) {
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return newMemoryConn(), nil
			},
			"plugin-b": func(api.UpstreamConnectRequest) (net.Conn, error) {
				panic("boom")
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(a) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(a) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-b", artifactB.ID, DesiredEnabled, `{}`, 5); err != nil {
		t.Fatalf("SetDesired(b) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-b"); err != nil {
		t.Fatalf("Enable(b) error = %v", err)
	}
	_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err == nil {
		t.Fatal("ConnectUpstream() error = nil, want panic converted to error")
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 2 {
		t.Fatalf("dispatch handlers = %d, want 2", len(plan.Handlers))
	}
	if plan.Handlers[0].PluginID != "plugin-b" || plan.Handlers[0].Panics != 1 {
		t.Fatalf("first handler summary = %+v, want plugin-b panic count", plan.Handlers[0])
	}
}

func TestManagerHandlerTimeoutDoesNotBreakDefaultRouteAfterDisable(t *testing.T) {
	latePeer := make(chan net.Conn, 1)
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"timeout-plugin": func(api.UpstreamConnectRequest) (net.Conn, error) {
				time.Sleep(50 * time.Millisecond)
				left, right := net.Pipe()
				latePeer <- right
				return left, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "timeout-plugin", func(manifest *Manifest) {
		manifest.RuntimeLimits.HandlerTimeoutMS = 10
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "timeout-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "timeout-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if !errors.Is(err, context.DeadlineExceeded) || !result.Handled {
		t.Fatalf("ConnectUpstream(timeout) = %+v err=%v, want handled deadline exceeded", result, err)
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 || plan.Handlers[0].Timeouts != 1 || plan.Handlers[0].DurationCount != 1 {
		t.Fatalf("dispatch after timeout = %+v, want one timeout recorded", plan.Handlers)
	}

	peer := <-latePeer
	defer peer.Close()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late handler conn read error = %v, want EOF after timeout cleanup", err)
	}
	if _, err := manager.Disable(context.Background(), "admin", "timeout-plugin"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	result, err = manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || result.Handled {
		t.Fatalf("ConnectUpstream(after timeout disable) = %+v err=%v, want default route pass-through", result, err)
	}
}

func TestManagerEnableDisablePublishesIsolatedDispatchSnapshots(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(a) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(a) error = %v", err)
	}
	snapshotA := currentDispatchSnapshotForTest(t, manager)
	if got := dispatchSnapshotPluginIDs(snapshotA); !reflect.DeepEqual(got, []string{"plugin-a"}) {
		t.Fatalf("snapshotA plugins = %+v, want plugin-a", got)
	}

	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-b", artifactB.ID, DesiredEnabled, `{}`, 5); err != nil {
		t.Fatalf("SetDesired(b) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-b"); err != nil {
		t.Fatalf("Enable(b) error = %v", err)
	}
	snapshotAB := currentDispatchSnapshotForTest(t, manager)
	if got := dispatchSnapshotPluginIDs(snapshotAB); !reflect.DeepEqual(got, []string{"plugin-b", "plugin-a"}) {
		t.Fatalf("snapshotAB plugins = %+v, want plugin-b then plugin-a", got)
	}
	if got := dispatchSnapshotPluginIDs(snapshotA); !reflect.DeepEqual(got, []string{"plugin-a"}) {
		t.Fatalf("snapshotA after enabling plugin-b = %+v, want unchanged plugin-a", got)
	}

	if _, err := manager.Disable(context.Background(), "admin", "plugin-b"); err != nil {
		t.Fatalf("Disable(b) error = %v", err)
	}
	snapshotAfterDisableB := currentDispatchSnapshotForTest(t, manager)
	if got := dispatchSnapshotPluginIDs(snapshotAfterDisableB); !reflect.DeepEqual(got, []string{"plugin-a"}) {
		t.Fatalf("snapshot after disable b = %+v, want plugin-a", got)
	}
	if got := dispatchSnapshotPluginIDs(snapshotAB); !reflect.DeepEqual(got, []string{"plugin-b", "plugin-a"}) {
		t.Fatalf("snapshotAB after disabling plugin-b = %+v, want unchanged old dispatch table", got)
	}

	if _, err := manager.Disable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Disable(a) error = %v", err)
	}
	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil || result.Handled {
		t.Fatalf("ConnectUpstream(after all disabled) = %+v err=%v, want default route pass-through", result, err)
	}
	if got := dispatchSnapshotPluginIDs(snapshotAfterDisableB); !reflect.DeepEqual(got, []string{"plugin-a"}) {
		t.Fatalf("snapshot after disable b mutated after disable a = %+v, want plugin-a", got)
	}
}

func TestManagerLoadFailureKeepsExistingDispatch(t *testing.T) {
	adapter := &fakeAdapter{
		loadErrs: map[string]error{
			"plugin-b": errors.New("open failed"),
		},
	}
	manager := newManagerForTest(t, adapter)
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(a) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(a) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-b", artifactB.ID, DesiredEnabled, `{}`, 5); err != nil {
		t.Fatalf("SetDesired(b) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-b"); err == nil {
		t.Fatal("Enable(b) error = nil, want load failure")
	}

	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 || plan.Handlers[0].PluginID != "plugin-a" {
		t.Fatalf("dispatch plan after failed enable = %+v, want only plugin-a", plan)
	}
	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream() = %+v, want existing plugin-a conn", result)
	}
}

func TestManagerReconcileRestoresEnabledPlugin(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	firstAdapter := &fakeAdapter{}
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      firstAdapter,
	})
	artifact := uploadTestArtifact(t, first, "plugin-a")
	if _, err := first.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := first.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	secondAdapter := &fakeAdapter{}
	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      secondAdapter,
	})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if secondAdapter.loads != 1 {
		t.Fatalf("reconcile loads = %d, want 1", secondAdapter.loads)
	}
	result, err := second.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if !result.Handled || result.Conn == nil {
		t.Fatalf("ConnectUpstream() = %+v, want restored handler conn", result)
	}
}

func TestAcceptorPanicIsRecovered(t *testing.T) {
	handler := &upstreamHandler{
		pluginID: "acceptor",
		accept: func(api.UpstreamConnectRequest) bool {
			panic("boom")
		},
	}
	accepted, err := handler.accepts(api.UpstreamConnectRequest{})
	if err == nil {
		t.Fatal("accepts() error = nil, want panic error")
	}
	if accepted {
		t.Fatal("accepts() accepted = true, want false")
	}
	if handler.panics.Load() != 1 {
		t.Fatalf("panics = %d, want 1", handler.panics.Load())
	}
}

func TestProtocolProxyTrackDrainAndForceClose(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	pluginGateway, pluginSide := net.Pipe()
	defer pluginSide.Close()
	handlerReturned := make(chan struct{})
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(req api.UpstreamConnectRequest) (net.Conn, error) {
				go func() {
					buf := make([]byte, len(req.InitialData))
					if _, err := io.ReadFull(pluginSide, buf); err != nil {
						t.Errorf("plugin side initial read error = %v", err)
					}
					close(handlerReturned)
					_, _ = pluginSide.Read(make([]byte, 1))
				}()
				return pluginGateway, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithCapabilities(t, manager, "plugin-a", testProtocolProxyCapabilities())
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "plugin-a", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:        "play.example",
			Upstream:    "backend",
			Source:      clientGateway,
			InitialData: []byte("hello"),
		})
		errCh <- err
	}()
	<-handlerReturned
	waitForPluginManagerTest(t, func() bool {
		return manager.DispatchPlan(context.Background()).Handlers[0].ActiveProxy == 1
	})
	plan := manager.DispatchPlan(context.Background())
	if got := plan.Handlers[0].ActiveProxy; got != 1 {
		t.Fatalf("active proxy = %d, want 1", got)
	}
	if got := plan.Handlers[0].DrainingProxy; got != 0 {
		t.Fatalf("draining proxy before disable = %d, want 0", got)
	}
	handler := manager.findHandler("plugin-a", plan.Handlers[0].HandlerID)
	if handler == nil {
		t.Fatal("handler before disable = nil")
	}
	if _, err := manager.Disable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	disabledResult, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
		Host:     "play.example",
		Upstream: "backend",
	})
	if err != nil || disabledResult.Handled {
		t.Fatalf("ConnectUpstream(disabled) = %+v err=%v, want default route pass-through", disabledResult, err)
	}
	if got := manager.DispatchPlan(context.Background()).Handlers; len(got) != 0 {
		t.Fatalf("dispatch handlers after disable = %+v, want no new protocol-proxy dispatch", got)
	}
	active, err := manager.ActiveProxyConnections(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ActiveProxyConnections() error = %v", err)
	}
	if len(active) != 1 || !active[0].Draining || active[0].ForceCloseRequested {
		t.Fatalf("active proxy summary after disable = %+v, want one draining connection without force-close request", active)
	}
	closed, err := manager.ForceCloseDraining(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("ForceCloseDraining() error = %v", err)
	}
	if closed != 1 {
		t.Fatalf("ForceCloseDraining() = %d, want 1", closed)
	}
	active, err = manager.ActiveProxyConnections(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ActiveProxyConnections(after force close) error = %v", err)
	}
	if len(active) == 1 && !active[0].ForceCloseRequested {
		t.Fatalf("active proxy summary after force close = %+v, want force-close request reflected", active)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.activeProxyCountLocked("plugin-a") == 0
	})
	if got := plan.Handlers[0].ProxyForceClosed; got != 0 {
		t.Fatalf("proxy force-closed before refresh = %d, want 0", got)
	}
	if got := plan.Handlers[0].DrainingProxy; got != 0 {
		t.Fatalf("draining proxy before refresh = %d, want 0", got)
	}
	if got := plan.Handlers[0].LastProxyError; got != "" {
		t.Fatalf("last proxy error before refresh = %q, want empty", got)
	}
	if got := handler.proxyForceClosed.Load(); got != 1 {
		t.Fatalf("proxy force-closed count = %d, want 1", got)
	}
	if got := handler.drainingProxy.Load(); got != 0 {
		t.Fatalf("draining proxy count after finish = %d, want 0", got)
	}
	if got := handler.lastProxyError.Load(); got == nil {
		t.Fatal("last proxy error = nil, want force-close copy error summary")
	}
}

func TestActiveProxyConnectionSummaryIncludesLastProxyError(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	blockingPluginGateway, blockingPluginSide := net.Pipe()
	defer blockingPluginSide.Close()

	calls := 0
	manager := newManagerForTest(t, &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				calls++
				if calls == 1 {
					return errorReadConn{Conn: newMemoryConn(), err: errors.New("previous proxy read failed")}, nil
				}
				return blockingPluginGateway, nil
			},
		},
	})
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	firstSource, firstClient := net.Pipe()
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Source: firstSource})
		firstDone <- err
	}()
	_ = firstClient.Close()
	if err := <-firstDone; err != nil {
		t.Fatalf("ConnectUpstream(first) error = %v", err)
	}

	started := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      clientGateway,
			InitialData: []byte("hello"),
		})
		started <- err
	}()
	buf := make([]byte, len("hello"))
	if _, err := io.ReadFull(blockingPluginSide, buf); err != nil {
		t.Fatalf("blocking plugin initial read error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		active, err := manager.ActiveProxyConnections(context.Background(), "plugin-a")
		return err == nil && len(active) == 1
	})
	if _, err := manager.Disable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	active, err := manager.ActiveProxyConnections(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("ActiveProxyConnections() error = %v", err)
	}
	if len(active) != 1 || !active[0].Draining || !strings.Contains(active[0].LastProxyError, "previous proxy read failed") {
		t.Fatalf("active proxy summary = %+v, want draining connection with last proxy error", active)
	}
	_ = clientSide.Close()
	_ = blockingPluginSide.Close()
	if err := <-started; err != nil {
		t.Fatalf("ConnectUpstream(second) error = %v", err)
	}
}

func TestProtocolProxyReplaysInitialAndForwardsClientBytes(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()

	initial := smoke.MinecraftHandshakePacket("play.example")
	next := smoke.MinecraftLoginStartPacket("Steve")
	pluginRead := make(chan []byte, 1)
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				gatewayEnd, pluginEnd := net.Pipe()
				go func() {
					defer pluginEnd.Close()
					handshake, err := smoke.ReadPacketFromConn(pluginEnd)
					if err != nil {
						t.Errorf("plugin handshake read error = %v", err)
						return
					}
					login, err := smoke.ReadPacketFromConn(pluginEnd)
					if err != nil {
						t.Errorf("plugin login read error = %v", err)
						return
					}
					pluginRead <- append(handshake, login...)
				}()
				return gatewayEnd, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      clientGateway,
			InitialData: initial,
		})
		errCh <- err
	}()
	if _, err := clientSide.Write(next); err != nil {
		t.Fatalf("client write error = %v", err)
	}
	got := <-pluginRead
	if !bytes.Equal(got, append(append([]byte(nil), initial...), next...)) {
		t.Fatalf("plugin bytes = %q, want initial+next", got)
	}
	_ = clientSide.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if got, want := plan.Handlers[0].ProxyBytesIn, uint64(len(initial)+len(next)); got != want {
		t.Fatalf("proxy bytes in = %d, want %d", got, want)
	}
}

func TestProtocolProxyMinecraftProtocolVersionMatrix(t *testing.T) {
	for _, protocolVersion := range []int{47, 340, 498, 763, 767} {
		t.Run(fmt.Sprintf("protocol_%d", protocolVersion), func(t *testing.T) {
			clientGateway, clientSide := net.Pipe()
			defer clientSide.Close()

			handshake := smoke.MinecraftHandshakePacketVersion(protocolVersion, "play.example")
			login := smoke.MinecraftLoginStartPacket("Steve")
			payload := smoke.MinecraftPayloadPacket(1, []byte("brand"))
			pluginRead := make(chan []byte, 1)
			adapter := &fakeAdapter{
				handlers: map[string]api.UpstreamConnectHandler{
					"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
						gatewayEnd, pluginEnd := net.Pipe()
						go func() {
							defer pluginEnd.Close()
							gotHandshake, err := smoke.ReadPacketFromConn(pluginEnd)
							if err != nil {
								t.Errorf("plugin handshake read error = %v", err)
								return
							}
							gotLogin, err := smoke.ReadPacketFromConn(pluginEnd)
							if err != nil {
								t.Errorf("plugin login read error = %v", err)
								return
							}
							gotPayload, err := smoke.ReadPacketFromConn(pluginEnd)
							if err != nil {
								t.Errorf("plugin payload read error = %v", err)
								return
							}
							pluginRead <- append(append(append([]byte(nil), gotHandshake...), gotLogin...), gotPayload...)
						}()
						return gatewayEnd, nil
					},
				},
			}
			manager := newManagerForTest(t, adapter)
			enableProtocolProxyTestPlugin(t, manager, "plugin-a")

			errCh := make(chan error, 1)
			go func() {
				_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
					Host:        "play.example",
					Upstream:    "backend",
					Source:      clientGateway,
					InitialData: handshake,
				})
				errCh <- err
			}()
			if err := writeAll(clientSide, append(append([]byte(nil), login...), payload...)); err != nil {
				t.Fatalf("client write error = %v", err)
			}
			got := <-pluginRead
			want := append(append(append([]byte(nil), handshake...), login...), payload...)
			if !bytes.Equal(got, want) {
				t.Fatalf("plugin bytes for protocol %d = %v, want handshake+login+payload", protocolVersion, got)
			}
			_ = clientSide.Close()
			if err := <-errCh; err != nil {
				t.Fatalf("ConnectUpstream() error = %v", err)
			}
			plan := manager.DispatchPlan(context.Background())
			if got, want := plan.Handlers[0].ProxyBytesIn, uint64(len(want)); got != want {
				t.Fatalf("proxy bytes in = %d, want %d", got, want)
			}
		})
	}
}

func TestProtocolProxyForwardsMalformedMinecraftInitialPackets(t *testing.T) {
	tests := []struct {
		name    string
		initial []byte
	}{
		{
			name:    "malformed varint",
			initial: []byte{0xff, 0xff, 0xff, 0xff, 0xff},
		},
		{
			name:    "oversized packet length",
			initial: []byte{0x81, 0x80, 0x80, 0x01},
		},
		{
			name:    "partial packet",
			initial: []byte{0x05, 0x00, 0x04, 'S', 't'},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientGateway, clientSide := newTestTCPConnPair(t)
			defer clientGateway.Close()
			defer clientSide.Close()
			pluginGateway, pluginSide := newTestTCPConnPair(t)
			defer pluginGateway.Close()
			defer pluginSide.Close()
			deadline := time.Now().Add(2 * time.Second)
			_ = clientGateway.SetDeadline(deadline)
			_ = clientSide.SetDeadline(deadline)
			_ = pluginGateway.SetDeadline(deadline)
			_ = pluginSide.SetDeadline(deadline)

			pluginRead := make(chan []byte, 1)
			pluginDone := make(chan error, 1)
			go func() {
				got := make([]byte, len(tt.initial))
				if _, err := io.ReadFull(pluginSide, got); err != nil {
					pluginRead <- nil
					pluginDone <- err
					return
				}
				pluginRead <- got
				if err := pluginSide.CloseWrite(); err != nil {
					pluginDone <- err
					return
				}
				_, err := io.Copy(io.Discard, pluginSide)
				pluginDone <- err
			}()

			manager := newManagerForTest(t, &fakeAdapter{
				handlers: map[string]api.UpstreamConnectHandler{
					"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
						return pluginGateway, nil
					},
				},
			})
			enableProtocolProxyTestPlugin(t, manager, "plugin-a")

			errCh := make(chan error, 1)
			go func() {
				_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
					Host:        "play.example",
					Upstream:    "backend",
					Source:      clientGateway,
					InitialData: tt.initial,
				})
				errCh <- err
			}()

			got := <-pluginRead
			if !bytes.Equal(got, tt.initial) {
				t.Fatalf("plugin initial bytes = %v, want %v", got, tt.initial)
			}
			if err := clientSide.CloseWrite(); err != nil {
				t.Fatalf("client CloseWrite() error = %v", err)
			}
			if err := <-errCh; err != nil {
				t.Fatalf("ConnectUpstream() error = %v", err)
			}
			if err := <-pluginDone; err != nil {
				t.Fatalf("plugin endpoint error = %v", err)
			}
			plan := manager.DispatchPlan(context.Background())
			if got, want := plan.Handlers[0].ProxyBytesIn, uint64(len(tt.initial)); got != want {
				t.Fatalf("proxy bytes in = %d, want %d", got, want)
			}
			if plan.Handlers[0].ProxyStarted != 1 || plan.Handlers[0].ProxyCompleted != 1 || plan.Handlers[0].ActiveProxy != 0 || plan.Handlers[0].ProxyErrors != 0 {
				t.Fatalf("proxy summary = %+v, want one clean completed proxy", plan.Handlers[0])
			}
		})
	}
}

func TestProtocolProxyBackpressureWithMinecraftPackets(t *testing.T) {
	clientGateway, clientSide, err := smoke.NewLoopbackTCPPair()
	if err != nil {
		t.Fatalf("NewLoopbackTCPPair() error = %v", err)
	}
	defer clientGateway.Close()
	defer clientSide.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = clientGateway.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)

	initial := smoke.MinecraftHandshakePacket("play.example")
	login := smoke.MinecraftLoginStartPacket("Steve")
	payload := smoke.MinecraftPayloadPacket(1, bytes.Repeat([]byte("x"), 256*1024))
	pluginRead := make(chan int, 1)
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				gatewayEnd, pluginEnd := net.Pipe()
				go func() {
					defer pluginEnd.Close()
					if _, err := smoke.ReadPacketFromConn(pluginEnd); err != nil {
						t.Errorf("plugin handshake read error = %v", err)
						pluginRead <- 0
						return
					}
					if _, err := smoke.ReadPacketFromConn(pluginEnd); err != nil {
						t.Errorf("plugin login read error = %v", err)
						pluginRead <- 0
						return
					}
					buf := make([]byte, len(payload))
					read := 0
					for read < len(buf) {
						end := read + 4096
						if end > len(buf) {
							end = len(buf)
						}
						n, err := pluginEnd.Read(buf[read:end])
						if n > 0 {
							read += n
							time.Sleep(time.Millisecond)
						}
						if err != nil {
							t.Errorf("plugin payload read error = %v", err)
							pluginRead <- read
							return
						}
					}
					if !bytes.Equal(buf, payload) {
						t.Errorf("plugin payload does not match Minecraft packet")
					}
					pluginRead <- read
				}()
				return gatewayEnd, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      clientGateway,
			InitialData: initial,
		})
		errCh <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		if err := writeAll(clientSide, append(append([]byte(nil), login...), payload...)); err != nil {
			writeErr <- err
			return
		}
		writeErr <- clientSide.CloseWrite()
	}()
	if got := <-pluginRead; got != len(payload) {
		t.Fatalf("plugin payload bytes = %d, want %d", got, len(payload))
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("client write error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if got, want := plan.Handlers[0].ProxyBytesIn, uint64(len(initial)+len(login)+len(payload)); got != want {
		t.Fatalf("proxy bytes in = %d, want %d", got, want)
	}
}

func TestProtocolProxyPluginEndpointEarlyCloseCompletesConnection(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()

	initial := []byte("initial-handshake")
	pluginClosed := make(chan struct{})
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				gatewayEnd, pluginEnd := net.Pipe()
				go func() {
					defer close(pluginClosed)
					buf := make([]byte, len(initial))
					if _, err := io.ReadFull(pluginEnd, buf); err != nil {
						t.Errorf("plugin initial read error = %v", err)
					}
					_ = pluginEnd.Close()
				}()
				return gatewayEnd, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      clientGateway,
			InitialData: initial,
		})
		errCh <- err
	}()
	<-pluginClosed
	_ = clientSide.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if got := plan.Handlers[0].ProxyStarted; got != 1 {
		t.Fatalf("proxy started = %d, want 1", got)
	}
	if got := plan.Handlers[0].ProxyCompleted; got != 1 {
		t.Fatalf("proxy completed = %d, want 1", got)
	}
	if got := plan.Handlers[0].ActiveProxy; got != 0 {
		t.Fatalf("active proxy = %d, want 0", got)
	}
}

func TestProtocolProxyClientEarlyCloseCompletesConnection(t *testing.T) {
	clientGateway, clientSide := newTestTCPConnPair(t)
	defer clientGateway.Close()
	defer clientSide.Close()

	initial := []byte("initial-handshake")
	pluginSawClientClose := make(chan struct{})
	pluginGateway, pluginEnd := newTestTCPConnPair(t)
	defer pluginGateway.Close()
	defer pluginEnd.Close()
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				go func() {
					defer close(pluginSawClientClose)
					defer pluginEnd.Close()
					buf := make([]byte, len(initial))
					if _, err := io.ReadFull(pluginEnd, buf); err != nil {
						t.Errorf("plugin initial read error = %v", err)
						return
					}
					_, _ = pluginEnd.Read(make([]byte, 1))
				}()
				return pluginGateway, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      clientGateway,
			InitialData: initial,
		})
		errCh <- err
	}()
	_ = clientSide.Close()
	<-pluginSawClientClose
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if got := plan.Handlers[0].ProxyStarted; got != 1 {
		t.Fatalf("proxy started = %d, want 1", got)
	}
	if got := plan.Handlers[0].ProxyCompleted; got != 1 {
		t.Fatalf("proxy completed = %d, want 1", got)
	}
	if got := plan.Handlers[0].ActiveProxy; got != 0 {
		t.Fatalf("active proxy = %d, want 0", got)
	}
}

func TestProtocolProxyCopyPanicReportsErrorAndHalfCloses(t *testing.T) {
	src := &panicReadCloser{}
	dst := &trackingHalfCloseWriter{}
	done := make(chan proxyCopyResult, 1)

	copyProtocolProxy(dst, src, true, done)

	result := <-done
	if !result.toPlugin {
		t.Fatal("copy result toPlugin = false, want true")
	}
	if result.bytes != 0 {
		t.Fatalf("copy result bytes = %d, want 0", result.bytes)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "protocol-proxy copy panic: boom") {
		t.Fatalf("copy result error = %v, want recovered panic", result.err)
	}
	if !src.closeReadCalled {
		t.Fatal("source CloseRead was not called after panic")
	}
	if !dst.closeWriteCalled {
		t.Fatal("destination CloseWrite was not called after panic")
	}
}

func TestProtocolProxySummaryRecordsError(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return errorReadConn{Conn: newMemoryConn(), err: errors.New("plugin endpoint read failed")}, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	enableProtocolProxyTestPlugin(t, manager, "plugin-a")

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source: clientGateway,
		})
		errCh <- err
	}()
	_ = clientSide.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if plan.Handlers[0].ProxyErrors != 1 || !strings.Contains(plan.Handlers[0].LastProxyError, "plugin endpoint read failed") {
		t.Fatalf("proxy summary = %+v, want one proxy error with last error", plan.Handlers[0])
	}
}

func TestProtocolProxyInitialWriteTimeoutClosesUnreadableConn(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				return writer, nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithCapabilities(t, manager, "plugin-a", testProtocolProxyCapabilities())
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"unused":true}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "plugin-a", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	sourceGateway, sourceClient := net.Pipe()
	defer sourceGateway.Close()
	defer sourceClient.Close()

	done := make(chan error, 1)
	go func() {
		initial := bytes.Repeat([]byte("x"), 2*1024*1024)
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Source:      sourceGateway,
			InitialData: initial,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ConnectUpstream() error = nil, want initial replay failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectUpstream() did not return after initial write deadline")
	}
}

func TestRouteResolverDecisionsCacheAndFallback(t *testing.T) {
	calls := 0
	adapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookRouteResolve,
				func(api.RouteResolveRequest) bool { return true },
				func(req api.RouteResolveRequest) (api.RouteDecision, error) {
					calls++
					switch req.Host {
					case "override.example":
						return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "10.0.0.10:25565", Reason: "test override", CacheTTL: time.Minute}, nil
					case "reject.example":
						return api.RouteDecision{Action: api.RouteDecisionReject, Reason: "test reject", CacheTTL: time.Minute}, nil
					case "provider-fallback.example":
						return api.RouteDecision{Action: api.RouteDecisionFallback, Upstream: "provider-fallback:25565", Reason: "provider fallback", CacheTTL: time.Minute}, nil
					case "pass.example":
						return api.RouteDecision{Action: api.RouteDecisionPass}, nil
					case "cached.example":
						if calls == 1 {
							return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "10.0.0.20:25565", Reason: "cached", CacheTTL: time.Minute}, nil
						}
						return api.RouteDecision{}, errors.New("source unavailable")
					default:
						return api.RouteDecision{}, errors.New("source unavailable")
					}
				})
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "route-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "provider", Key: ExtensionRouteResolve}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["route.resolve/v1"],"route":{"cache_ttl_ms":60000}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "route-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "route-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	override, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "override.example"}, nil)
	if err != nil || override.Decision.Action != api.RouteDecisionOverride || override.Decision.Upstream == "" {
		t.Fatalf("override decision = %+v err=%v", override, err)
	}
	reject, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "reject.example"}, nil)
	if err != nil || reject.Decision.Action != api.RouteDecisionReject {
		t.Fatalf("reject decision = %+v err=%v", reject, err)
	}
	providerFallback, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "provider-fallback.example"}, nil)
	if err != nil || providerFallback.Decision.Action != api.RouteDecisionFallback || providerFallback.Decision.Upstream != "provider-fallback:25565" {
		t.Fatalf("provider fallback decision = %+v err=%v", providerFallback, err)
	}
	pass, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "pass.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
	if err != nil || pass.Source != "sqlite_fallback" || pass.Decision.Upstream != "sqlite:25565" {
		t.Fatalf("pass fallback = %+v err=%v", pass, err)
	}
	calls = 0
	first, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "cached.example"}, nil)
	if err != nil || first.Source != "provider" {
		t.Fatalf("first cached decision = %+v err=%v", first, err)
	}
	second, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "cached.example"}, nil)
	if err != nil || second.Source != "cache" || second.Decision.Upstream != "10.0.0.20:25565" {
		t.Fatalf("cache fallback decision = %+v err=%v", second, err)
	}
	sqlite, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "down.example", FallbackUpstream: "sqlite-down:25565", FallbackHit: true}, nil)
	if err != nil || sqlite.Source != "sqlite_fallback" || sqlite.Decision.Upstream != "sqlite-down:25565" {
		t.Fatalf("sqlite fallback = %+v err=%v", sqlite, err)
	}
}

func TestStatusPingPerHostAndDisableFallsBack(t *testing.T) {
	adapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookStatusPing,
				func(api.StatusPingRequest) bool { return true },
				func(req api.StatusPingRequest) (api.StatusPingResponse, error) {
					return api.StatusPingResponse{MOTD: "motd for " + req.Host, VersionText: "v1", MaxPlayers: 20}, nil
				})
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "status-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionStatusPing}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["status.ping/v1"],"status":{"hosts":["a.example","b.example"]}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "status-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "status-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	status, err := manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "a.example"})
	if err != nil || !status.Handled || status.Response.MOTD != "motd for a.example" {
		t.Fatalf("StatusPing() = %+v err=%v", status, err)
	}
	if _, err := manager.Disable(context.Background(), "admin", "status-plugin"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	status, err = manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "a.example"})
	if err != nil || status.Handled {
		t.Fatalf("StatusPing(disabled) = %+v err=%v, want default fallback", status, err)
	}
}

func TestEventSubscriberFailureDoesNotAffectEmitter(t *testing.T) {
	var sinkDown atomic.Bool
	var deliveries atomic.Int32
	sinkDown.Store(true)
	adapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookEventSubscriber,
				func(api.EventDeliveryRequest) bool { return true },
				func(api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
					deliveries.Add(1)
					if sinkDown.Load() {
						return api.EventDeliveryResult{Retry: true, Reason: "sink down"}, errors.New("sink down")
					}
					return api.EventDeliveryResult{OK: true}, nil
				})
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "subscriber-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "event", Key: ExtensionEventSubscriber}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["event.subscriber/v1"],"event_subscriber":{"mode":"at_least_once","max_retry":1}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "subscriber-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "subscriber-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	emitterManifest := Manifest{Events: []EventSpec{{Name: "audit.test", Fields: []string{"ok"}}}}
	if err := manager.operations.ForPlugin("emitter", "artifact", emitterManifest).EmitEvent(context.Background(), "audit.test", map[string]string{"ok": "true"}); err != nil {
		t.Fatalf("EmitEvent() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.operations.SubscriberDeadLetters() > 0
	})
	sinkDown.Store(false)
	if replayed := manager.ReplaySubscriberDeadLetters(context.Background(), "admin"); replayed == 0 {
		t.Fatal("ReplaySubscriberDeadLetters() = 0, want replayed dead letter")
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.operations.SubscriberDeadLetters() == 0 && deliveries.Load() >= 2
	})

	sinkDown.Store(true)
	if err := manager.operations.ForPlugin("emitter", "artifact", emitterManifest).EmitEvent(context.Background(), "audit.test", map[string]string{"ok": "true"}); err != nil {
		t.Fatalf("EmitEvent(second) error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.operations.SubscriberDeadLetters() > 0
	})
	if dropped := manager.DropSubscriberDeadLetters(context.Background(), "admin"); dropped == 0 {
		t.Fatal("DropSubscriberDeadLetters() = 0, want dropped dead letter")
	}
	if deadLetters := manager.operations.SubscriberDeadLetters(); deadLetters != 0 {
		t.Fatalf("SubscriberDeadLetters() = %d, want 0 after drop", deadLetters)
	}
}

func TestEventSubscriberDeadLetterCanReplayAcrossNodes(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	var replayed atomic.Int32
	failingAdapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookEventSubscriber,
				func(api.EventDeliveryRequest) bool { return true },
				func(api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
					return api.EventDeliveryResult{Retry: true, Reason: "node-a sink down"}, errors.New("node-a sink down")
				})
		},
	}
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      failingAdapter,
		NodeID:       "node-a",
	})
	artifact := uploadTestArtifactWithManifest(t, first, "subscriber-cross-node", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "event", Key: ExtensionEventSubscriber}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["event.subscriber/v1"],"event_subscriber":{"mode":"at_least_once","max_retry":1}}`)
	})
	if _, err := first.SetDesired(context.Background(), "admin", "subscriber-cross-node", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := first.Enable(context.Background(), "admin", "subscriber-cross-node"); err != nil {
		t.Fatalf("Enable(first) error = %v", err)
	}
	emitterManifest := Manifest{Events: []EventSpec{{Name: "audit.cross_node", Fields: []string{"ok"}}}}
	if err := first.operations.ForPlugin("emitter", "artifact", emitterManifest).EmitEvent(context.Background(), "audit.cross_node", map[string]string{"ok": "true"}); err != nil {
		t.Fatalf("EmitEvent(first) error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return first.SubscriberDeadLetters(context.Background()) == 1
	})
	var deadLetterNode string
	if err := db.QueryRowContext(context.Background(), `SELECT node_id FROM plugin_subscriber_dead_letters WHERE subscriber_plugin_id = ? AND status = 'pending'`, "subscriber-cross-node").Scan(&deadLetterNode); err != nil {
		t.Fatalf("pending dead letter node query error = %v", err)
	}
	if deadLetterNode != "node-a" {
		t.Fatalf("dead letter node = %q, want node-a", deadLetterNode)
	}

	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter: &fakeAdapter{initOnly: true, initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookEventSubscriber,
				func(api.EventDeliveryRequest) bool { return true },
				func(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
					if req.Name == "audit.cross_node" {
						replayed.Add(1)
					}
					return api.EventDeliveryResult{OK: true}, nil
				})
		}},
		NodeID: "node-b",
	})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(second) error = %v", err)
	}
	if count := second.ReplaySubscriberDeadLetters(context.Background(), "node-b-admin"); count != 1 {
		t.Fatalf("ReplaySubscriberDeadLetters(second) = %d, want 1", count)
	}
	waitForPluginManagerTest(t, func() bool {
		return second.SubscriberDeadLetters(context.Background()) == 0 && replayed.Load() == 1
	})
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM plugin_subscriber_dead_letters WHERE subscriber_plugin_id = ?`, "subscriber-cross-node").Scan(&status); err != nil {
		t.Fatalf("dead letter status query error = %v", err)
	}
	if status != "replayed" {
		t.Fatalf("dead letter status = %q, want replayed", status)
	}
	ops, err := second.repo.ListOperations(context.Background(), "", 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	if !operationRecorded(ops, "event_subscriber_replay:succeeded") {
		t.Fatalf("operations = %+v, want cross-node replay audit", ops)
	}
}

func TestProviderRegistryIncludesUnavailableAdminAuthProvider(t *testing.T) {
	adapter := &fakeAdapter{
		initOnly: true,
		initHook: func(gateway *Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookAdminAuthProvider,
				func(api.ProviderRegistration) bool { return true },
				func() (api.ProviderRegistration, error) {
					return api.ProviderRegistration{Type: "admin.auth.provider/v1", Name: "oidc", Fallback: true}, errors.New("oidc down")
				})
		},
	}
	manager := newManagerForTest(t, adapter)
	artifact := uploadTestArtifactWithManifest(t, manager, "admin-auth-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "provider", Key: ExtensionAdminAuthProvider}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["admin.auth.provider/v1"],"providers":[{"type":"admin.auth.provider/v1","name":"oidc","fallback":true}]}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "admin-auth-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "admin-auth-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Providers) != 1 || plan.Providers[0].Status != "unavailable" || !plan.Providers[0].Fallback {
		t.Fatalf("providers = %+v, want unavailable fallback admin auth provider", plan.Providers)
	}
}

func TestManagerConnectionFilterOrderingAndFailOpen(t *testing.T) {
	var calls []string
	adapter := &fakeAdapter{initOnly: true, initHook: func(gateway *Gateway) error {
		switch gateway.PluginID {
		case "fail-open-filter":
			return api.RegisterHookHandler(gateway, api.HookConnectionFilter,
				func(api.ConnectionFilterRequest) bool { return true },
				func(api.ConnectionFilterRequest) (api.FilterDecision, error) {
					calls = append(calls, "fail-open")
					return api.FilterDecision{}, errors.New("temporary filter error")
				})
		case "deny-filter":
			return api.RegisterHookHandler(gateway, api.HookConnectionFilter,
				func(api.ConnectionFilterRequest) bool { return true },
				func(api.ConnectionFilterRequest) (api.FilterDecision, error) {
					calls = append(calls, "deny")
					return api.FilterDecision{Reject: true, Reason: "blocked by middleware"}, nil
				})
		default:
			return nil
		}
	}}
	manager := newManagerForTest(t, adapter)
	failOpenArtifact := uploadTestArtifactWithManifest(t, manager, "fail-open-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"],"middleware":{"fail_policy":"fail_open"}}`)
	})
	denyArtifact := uploadTestArtifactWithManifest(t, manager, "deny-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"],"middleware":{"fail_policy":"fail_open"}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "fail-open-filter", failOpenArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(fail-open) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "deny-filter", denyArtifact.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(deny) error = %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	result, err := manager.FilterConnection(context.Background(), api.ConnectionFilterRequest{SourceAddr: "127.0.0.1:12345"})
	if err != nil || result.Allowed || result.PluginID != "deny-filter" || result.Reason != "blocked by middleware" {
		t.Fatalf("FilterConnection() = %+v err=%v, want fail-open continue then deny", result, err)
	}
	if got := strings.Join(calls, ","); got != "fail-open,deny" {
		t.Fatalf("connection filter calls = %q, want priority order", got)
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Middleware) != 2 ||
		plan.Middleware[0].PluginID != "fail-open-filter" ||
		plan.Middleware[1].PluginID != "deny-filter" ||
		plan.Middleware[0].Errors != 1 ||
		plan.Middleware[1].Blocked != 1 {
		t.Fatalf("middleware dispatch summary = %+v, want ordered fail-open error then deny block", plan.Middleware)
	}
}

func TestManagerConnectionFilterFailClosedStopsDispatch(t *testing.T) {
	var calls []string
	adapter := &fakeAdapter{initOnly: true, initHook: func(gateway *Gateway) error {
		switch gateway.PluginID {
		case "fail-closed-filter":
			return api.RegisterHookHandler(gateway, api.HookConnectionFilter,
				func(api.ConnectionFilterRequest) bool { return true },
				func(api.ConnectionFilterRequest) (api.FilterDecision, error) {
					calls = append(calls, "fail-closed")
					return api.FilterDecision{}, errors.New("hard filter error")
				})
		case "later-filter":
			return api.RegisterHookHandler(gateway, api.HookConnectionFilter,
				func(api.ConnectionFilterRequest) bool { return true },
				func(api.ConnectionFilterRequest) (api.FilterDecision, error) {
					calls = append(calls, "later")
					return api.FilterDecision{Allow: true}, nil
				})
		default:
			return nil
		}
	}}
	manager := newManagerForTest(t, adapter)
	failClosedArtifact := uploadTestArtifactWithManifest(t, manager, "fail-closed-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"],"middleware":{"fail_policy":"fail_closed"}}`)
	})
	laterArtifact := uploadTestArtifactWithManifest(t, manager, "later-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"],"middleware":{"fail_policy":"fail_open"}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "fail-closed-filter", failClosedArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(fail-closed) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "later-filter", laterArtifact.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(later) error = %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	result, err := manager.FilterConnection(context.Background(), api.ConnectionFilterRequest{SourceAddr: "127.0.0.1:12345"})
	if err == nil || !strings.Contains(err.Error(), "hard filter error") || result.Allowed || result.PluginID != "fail-closed-filter" {
		t.Fatalf("FilterConnection() = %+v err=%v, want fail-closed stop", result, err)
	}
	if got := strings.Join(calls, ","); got != "fail-closed" {
		t.Fatalf("connection filter calls = %q, want fail-closed to stop later filter", got)
	}
}

func TestManagerHandshakeFilterOrderingAndRewrite(t *testing.T) {
	var calls []string
	adapter := &fakeAdapter{initOnly: true, initHook: func(gateway *Gateway) error {
		switch gateway.PluginID {
		case "rewrite-filter":
			return api.RegisterHookHandler(gateway, api.HookHandshakeFilter,
				func(api.HandshakeFilterRequest) bool { return true },
				func(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
					calls = append(calls, "rewrite:"+req.ServerHost)
					return api.HandshakeFilterDecision{
						FilterDecision: api.FilterDecision{Allow: true},
						RewriteHost:    "rewritten.example",
					}, nil
				})
		case "deny-rewritten-filter":
			return api.RegisterHookHandler(gateway, api.HookHandshakeFilter,
				func(api.HandshakeFilterRequest) bool { return true },
				func(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
					calls = append(calls, "deny:"+req.ServerHost)
					if req.ServerHost == "rewritten.example" {
						return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Reject: true, Reason: "rewritten denied"}}, nil
					}
					return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Allow: true}}, nil
				})
		default:
			return nil
		}
	}}
	manager := newManagerForTest(t, adapter)
	rewriteArtifact := uploadTestArtifactWithManifest(t, manager, "rewrite-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionHandshakeFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["handshake.filter/v1"],"middleware":{"fail_policy":"fail_open"}}`)
	})
	denyArtifact := uploadTestArtifactWithManifest(t, manager, "deny-rewritten-filter", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "middleware", Key: ExtensionHandshakeFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["handshake.filter/v1"],"middleware":{"fail_policy":"fail_open"}}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "rewrite-filter", rewriteArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(rewrite) error = %v", err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "deny-rewritten-filter", denyArtifact.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(deny) error = %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	result, err := manager.FilterHandshake(context.Background(), api.HandshakeFilterRequest{ServerHost: "original.example"})
	if err != nil || result.Allowed || result.PluginID != "deny-rewritten-filter" || result.Reason != "rewritten denied" {
		t.Fatalf("FilterHandshake() = %+v err=%v, want rewrite then deny", result, err)
	}
	if got := strings.Join(calls, ","); got != "rewrite:original.example,deny:rewritten.example" {
		t.Fatalf("handshake filter calls = %q, want rewrite to feed later filter", got)
	}
}

func TestOfficialRulePolicyBadCIDRDoesNotBreakDefaultRoute(t *testing.T) {
	manager := newManagerForTest(t, nil)
	artifact, err := manager.Artifact(context.Background(), "builtin-official-rule-policy-0.1.0")
	if err != nil {
		t.Fatalf("official artifact missing: %v", err)
	}
	config := `{"source_deny_cidr":["not-a-cidr"],"upstream_rewrite":{"play.example":"rewrite:25565"},"maintenance":{"enabled":true,"hosts":["play.example"],"motd":"Maint","favicon":"data:image/png;base64,fixture","online_players":2,"max_players":50,"version":"1.20.4","window":"02:00-03:00 UTC"}}`
	if _, err := manager.SetDesired(context.Background(), "admin", "official.rule-policy", artifact.ID, DesiredEnabled, config, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "official.rule-policy"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	filter, err := manager.FilterConnection(context.Background(), api.ConnectionFilterRequest{SourceAddr: "127.0.0.1:12345"})
	if err != nil || !filter.Allowed {
		t.Fatalf("FilterConnection() = %+v err=%v, want allowed despite invalid CIDR", filter, err)
	}
	route, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "unknown.example", FallbackUpstream: "default:25565", FallbackHit: true}, nil)
	if err != nil || route.Source != "sqlite_fallback" || route.Decision.Upstream != "default:25565" {
		t.Fatalf("default route fallback = %+v err=%v", route, err)
	}
	rewrite, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "default:25565", FallbackHit: true}, nil)
	if err != nil || rewrite.Decision.Action != api.RouteDecisionOverride || rewrite.Decision.Upstream != "rewrite:25565" {
		t.Fatalf("upstream rewrite = %+v err=%v", rewrite, err)
	}
	status, err := manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "play.example", ProtocolVersion: 765})
	if err != nil || !status.Handled || status.Response.Favicon == "" || status.Response.OnlinePlayers != 2 || status.Response.MaxPlayers != 50 || status.Response.VersionText != "1.20.4" {
		t.Fatalf("status ping = %+v err=%v, want configured maintenance status fields", status, err)
	}
}

func enableProtocolProxyTestPlugin(t *testing.T, manager *Manager, pluginID string) ArtifactRecord {
	t.Helper()
	artifact := uploadTestArtifactWithCapabilities(t, manager, pluginID, testProtocolProxyCapabilities())
	if _, err := manager.SetDesired(context.Background(), "admin", pluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, pluginID, artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", pluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	return artifact
}

func approveGovernanceForTest(t *testing.T, manager *Manager, pluginID, artifactID string) {
	t.Helper()
	if _, err := manager.CreateReview(context.Background(), "admin", pluginID, GovernanceReviewRequest{
		ArtifactID: artifactID,
		Profile:    PolicyProfileProd,
		Decision:   ReviewDecisionApproved,
		Notes:      "test approval",
	}); err != nil {
		t.Fatalf("CreateReview(%s) error = %v", pluginID, err)
	}
}

func newManagerForTest(t *testing.T, adapter RuntimeAdapter) *Manager {
	t.Helper()
	return newManagerForTestWithBuilders(t, adapter, nil)
}

func testProtocolProxyCapabilities() json.RawMessage {
	return json.RawMessage(`{
		"upstream_connect":{"mode":"protocol-proxy"},
		"scope":{"type":"host","values":["play.example"]},
		"rollout":{"mode":"canary"},
		"minecraft":{
			"protocol_versions":{"tested":[767]},
			"forwarding":{"supported":["none"],"default":"none"}
		}
	}`)
}

func newManagerForTestWithBuilders(t *testing.T, adapter RuntimeAdapter, builders map[string]SourceBuilder) *Manager {
	t.Helper()
	return newManagerForTestWithBuildersProfile(t, adapter, builders, PolicyProfileProd)
}

func newManagerForTestWithBuildersProfile(t *testing.T, adapter RuntimeAdapter, builders map[string]SourceBuilder, profile string) *Manager {
	t.Helper()
	db := openPluginManagerTestDB(t)
	return New(Options{
		DB:            db,
		ArtifactRoot:  t.TempDir(),
		Adapter:       adapter,
		Builders:      builders,
		PolicyProfile: profile,
	})
}

func newSandboxServiceModeManagerForTest(t *testing.T, policy SandboxPolicy) *Manager {
	t.Helper()
	manager := New(Options{
		DB:                 openPluginManagerTestDB(t),
		ArtifactRoot:       t.TempDir(),
		FutureRuntimeGates: FutureRuntimeGates{SandboxProcess: true},
		SandboxPolicy:      policy,
		SandboxSelfCheck:   func(SandboxPolicy) error { return nil },
	})
	if _, err := manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeSandboxProcess); err != nil {
		t.Fatalf("SetPluginServiceDesired(sandbox) error = %v", err)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode(sandbox) error = %v", err)
	}
	if manager.serviceMode != PluginServiceModeSandboxProcess {
		t.Fatalf("serviceMode = %q, want %q", manager.serviceMode, PluginServiceModeSandboxProcess)
	}
	return manager
}

func openPluginManagerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := admindb.Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return db
}

func uploadTestArtifact(t *testing.T, manager *Manager, pluginID string) ArtifactRecord {
	t.Helper()
	return uploadTestArtifactWithCapabilities(t, manager, pluginID, nil)
}

func uploadTestArtifactWithCapabilities(t *testing.T, manager *Manager, pluginID string, capabilities json.RawMessage) ArtifactRecord {
	t.Helper()
	return uploadTestArtifactWithManifest(t, manager, pluginID, func(manifest *Manifest) {
		manifest.Capabilities = capabilities
	})
}

func uploadTestArtifactWithManifest(t *testing.T, manager *Manager, pluginID string, mutate func(*Manifest)) ArtifactRecord {
	t.Helper()
	return uploadTestArtifactWithManifestBytes(t, manager, pluginID, []byte("fake plugin bytes "+pluginID), mutate)
}

func uploadTestArtifactWithManifestBytes(t *testing.T, manager *Manager, pluginID string, runtimeBytes []byte, mutate func(*Manifest)) ArtifactRecord {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(testManifestBytesWithCapabilities(t, pluginID, nil), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	if mutate != nil {
		mutate(&manifest)
	}
	if manifest.Runtime.Type == RuntimeSandbox {
		if manifest.Runtime.Entry == "" {
			manifest.Runtime.Entry = RuntimeEntry
		}
		if manifest.Runtime.Protocol == "" {
			manifest.Runtime.Protocol = SandboxProcessProtocolV1
		}
		if manifest.Runtime.OS == "" {
			manifest.Runtime.OS = runtime.GOOS
		}
		if manifest.Runtime.Arch == "" {
			manifest.Runtime.Arch = runtime.GOARCH
		}
		if manifest.Runtime.ABIVersion == "" {
			manifest.Runtime.ABIVersion = SandboxProcessABIVersionV1
		}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	entry := manifest.Runtime.Entry
	if entry == "" {
		entry = RuntimeEntry
	}
	modes := map[string]os.FileMode{}
	if manifest.Runtime.Type == RuntimeSandbox {
		modes[entry] = 0755
	}
	entries := map[string][]byte{
		"manifest.json": manifestBytes,
		entry:           runtimeBytes,
	}
	if manifest.Runtime.Type == RuntimeSandbox {
		entries["conformance.json"] = sandboxConformanceFixtureBytesForTest(t)
	}
	packagePath := writeTestMCGPWithModes(t, entries, modes)
	artifact, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   pluginID + ".mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact(%s) error = %v", pluginID, err)
	}
	return artifact
}

func sandboxConformanceFixtureBytesForTest(t *testing.T) []byte {
	t.Helper()
	var fixtures []map[string]any
	fixtures = append(fixtures, map[string]any{"name": "contract", "status": "pass"})
	for _, coverage := range SandboxConformanceRequiredCoverage() {
		fixtures = append(fixtures, map[string]any{
			"name":         coverage,
			"status":       "pass",
			"extension":    RuntimeSandbox,
			"coverage":     coverage,
			"mode":         "executable",
			"validated_by": SandboxConformanceValidatedByCLI,
			"evidence": SandboxConformanceEvidence{
				Coverage:    coverage,
				Executed:    true,
				ValidatedBy: SandboxConformanceValidatedByCLI,
				Checks:      []string{"unit.fixture"},
			},
		})
	}
	data, err := json.Marshal(map[string]any{
		"fixtures": fixtures,
	})
	if err != nil {
		t.Fatalf("Marshal sandbox conformance fixture error = %v", err)
	}
	return data
}

func uploadTestArtifactWithProvenance(t *testing.T, manager *Manager, pluginID string, provenance map[string]any) ArtifactRecord {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(testManifestBytesWithCapabilities(t, pluginID, nil), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	provenanceBytes, err := json.Marshal(provenance)
	if err != nil {
		t.Fatalf("Marshal provenance error = %v", err)
	}
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json":   manifestBytes,
		RuntimeEntry:      []byte("fake plugin bytes " + pluginID),
		"provenance.json": provenanceBytes,
	})
	artifact, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   pluginID + ".mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact(%s) error = %v", pluginID, err)
	}
	return artifact
}

func uploadTestSource(t *testing.T, manager *Manager, pluginID string) ArtifactRecord {
	t.Helper()
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testSourceManifestBytes(t, pluginID),
		"go.mod":        []byte("module example.com/" + pluginID + "\n\ngo 1.24.0\n"),
		"main.go":       []byte("package main\n"),
	})
	source, err := manager.UploadSource(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   pluginID + "-source.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadSource(%s) error = %v", pluginID, err)
	}
	return source
}

func uploadBuildableTestSource(t *testing.T, manager *Manager, pluginID string) ArtifactRecord {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "Buildable Source",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeSource,
		Runtime: RuntimeManifest{
			Type:        RuntimeGoPlugin,
			EntrySymbol: "Plugin",
		},
		Build: BuildManifest{
			Type:           BuildTypeGo,
			Entry:          ".",
			GoVersion:      runtime.Version(),
			Tags:           []string{},
			VendorRequired: false,
			Output:         RuntimeEntry,
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
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	mainSource := `package main

import (
	"net"

	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginImpl struct{ api.AbstractPlugin }

func Plugin() api.Plugin { return &pluginImpl{} }

func (p *pluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(api.UpstreamConnectRequest) bool { return true },
		func(api.UpstreamConnectRequest) (net.Conn, error) {
			left, right := net.Pipe()
			_ = right.Close()
			return left, nil
		},
	)
}
`
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"go.mod":        []byte("module example.com/" + pluginID + "\n\ngo 1.25.0\n\nrequire github.com/tursom/mc-gateway v0.0.0\n\nreplace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"),
		"main.go":       []byte(mainSource),
	})
	source, err := manager.UploadSource(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   pluginID + "-source.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadSource(%s) error = %v", pluginID, err)
	}
	return source
}

func waitForPluginManagerTest(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for plugin manager condition")
		case <-ticker.C:
			if done() {
				return
			}
		}
	}
}

func currentDispatchSnapshotForTest(t *testing.T, manager *Manager) []*upstreamHandler {
	t.Helper()
	value := manager.snapshot.Load()
	handlers, ok := value.([]*upstreamHandler)
	if !ok {
		t.Fatalf("dispatch snapshot = %#v, want upstream handlers", value)
	}
	return handlers
}

func dispatchSnapshotPluginIDs(handlers []*upstreamHandler) []string {
	ids := make([]string, 0, len(handlers))
	for _, handler := range handlers {
		ids = append(ids, handler.pluginID)
	}
	return ids
}

type fakeAdapter struct {
	loads      int
	handlers   map[string]api.UpstreamConnectHandler
	initOnly   bool
	initHook   func(*Gateway) error
	init       func(*Gateway)
	loadErr    error
	loadErrs   map[string]error
	dryRunErr  error
	dryRunErrs map[string]error
}

func (a *fakeAdapter) Load(_ context.Context, artifact ArtifactRecord, _ PluginRecord, gateway *Gateway) (api.Plugin, error) {
	a.loads++
	if a.loadErr != nil {
		return nil, a.loadErr
	}
	if a.loadErrs != nil && a.loadErrs[artifact.PluginID] != nil {
		return nil, a.loadErrs[artifact.PluginID]
	}
	handler := api.UpstreamConnectHandler(func(api.UpstreamConnectRequest) (net.Conn, error) {
		return newMemoryConn(), nil
	})
	if a.handlers != nil && a.handlers[artifact.PluginID] != nil {
		handler = a.handlers[artifact.PluginID]
	}
	if !a.initOnly {
		if err := api.RegisterHookHandler(
			gateway,
			api.HookUpstreamConnect,
			func(api.UpstreamConnectRequest) bool { return true },
			handler,
		); err != nil {
			return nil, err
		}
	}
	if a.initHook != nil {
		if err := a.initHook(gateway); err != nil {
			return nil, err
		}
	}
	if a.init != nil {
		a.init(gateway)
	}
	return &fakePlugin{}, nil
}

func (a *fakeAdapter) DryRunConfig(_ context.Context, artifact ArtifactRecord, _ PluginRecord) error {
	if a.dryRunErr != nil {
		return a.dryRunErr
	}
	if a.dryRunErrs != nil && a.dryRunErrs[artifact.PluginID] != nil {
		return a.dryRunErrs[artifact.PluginID]
	}
	return nil
}

type fakePlugin struct {
	api.AbstractPlugin
}

type panicReadCloser struct {
	closeReadCalled bool
}

func (p *panicReadCloser) Read([]byte) (int, error) {
	panic("boom")
}

func (p *panicReadCloser) CloseRead() error {
	p.closeReadCalled = true
	return nil
}

type trackingHalfCloseWriter struct {
	closeWriteCalled bool
}

func (w *trackingHalfCloseWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func (w *trackingHalfCloseWriter) CloseWrite() error {
	w.closeWriteCalled = true
	return nil
}

type errorReadConn struct {
	net.Conn
	err error
}

func (c errorReadConn) Read([]byte) (int, error) {
	return 0, c.err
}

type fakeBuilder struct {
	result BuildResult
	err    error
	calls  int
}

func (b *fakeBuilder) Build(context.Context, ArtifactRecord, BuildRequest, BuildRecord) (BuildResult, error) {
	b.calls++
	return b.result, b.err
}

func newMemoryConn() net.Conn {
	left, right := net.Pipe()
	_ = right.Close()
	return left
}

func operationRecorded(records []OperationRecord, want string) bool {
	for _, record := range records {
		if record.Operation+":"+record.Status == want {
			return true
		}
	}
	return false
}

func newTestTCPConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenTCP() error = %v", err)
	}
	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		_ = listener.Close()
		t.Fatalf("DialTCP() error = %v", err)
	}
	var server *net.TCPConn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		_ = listener.Close()
		_ = client.Close()
		t.Fatalf("AcceptTCP() error = %v", err)
	case <-time.After(time.Second):
		_ = listener.Close()
		_ = client.Close()
		t.Fatal("AcceptTCP() timed out")
	}
	if err := listener.Close(); err != nil {
		_ = client.Close()
		_ = server.Close()
		t.Fatalf("Close(listener) error = %v", err)
	}
	return server, client
}
