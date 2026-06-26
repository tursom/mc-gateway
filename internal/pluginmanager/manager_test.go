package pluginmanager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/plugin/api"
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

func TestManagerUploadSourceQueuesBuild(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
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
	manager := newManagerForTest(t, &fakeAdapter{})
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
	manager := newManagerForTestWithBuilders(t, adapter, map[string]SourceBuilder{
		BuilderTypeLocalProcess: builder,
	})
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
	artifact := uploadTestArtifactWithCapabilities(t, manager, "plugin-a", json.RawMessage(`{"upstream_connect":{"mode":"protocol-proxy"}}`))
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
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
	if _, err := manager.Disable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	closed, err := manager.ForceCloseDraining(context.Background(), "admin", "plugin-a")
	if err != nil {
		t.Fatalf("ForceCloseDraining() error = %v", err)
	}
	if closed != 1 {
		t.Fatalf("ForceCloseDraining() = %d, want 1", closed)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.activeProxyCountLocked("plugin-a") == 0
	})
}

func TestProtocolProxyReplaysInitialAndForwardsClientBytes(t *testing.T) {
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()

	initial := []byte("initial-handshake")
	next := []byte("login-start")
	pluginRead := make(chan []byte, 1)
	adapter := &fakeAdapter{
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(api.UpstreamConnectRequest) (net.Conn, error) {
				gatewayEnd, pluginEnd := net.Pipe()
				go func() {
					defer pluginEnd.Close()
					buf := make([]byte, len(initial)+len(next))
					if _, err := io.ReadFull(pluginEnd, buf); err != nil {
						t.Errorf("plugin read error = %v", err)
						return
					}
					pluginRead <- buf
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
	if got := plan.Handlers[0].ProxyBytesIn; got != uint64(len(next)) {
		t.Fatalf("proxy bytes in = %d, want %d", got, len(next))
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
	artifact := uploadTestArtifactWithCapabilities(t, manager, "plugin-a", json.RawMessage(`{"upstream_connect":{"mode":"protocol-proxy"}}`))
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{"unused":true}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
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

func enableProtocolProxyTestPlugin(t *testing.T, manager *Manager, pluginID string) ArtifactRecord {
	t.Helper()
	artifact := uploadTestArtifactWithCapabilities(t, manager, pluginID, json.RawMessage(`{"upstream_connect":{"mode":"protocol-proxy"}}`))
	if _, err := manager.SetDesired(context.Background(), "admin", pluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", pluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	return artifact
}

func newManagerForTest(t *testing.T, adapter RuntimeAdapter) *Manager {
	t.Helper()
	return newManagerForTestWithBuilders(t, adapter, nil)
}

func newManagerForTestWithBuilders(t *testing.T, adapter RuntimeAdapter, builders map[string]SourceBuilder) *Manager {
	t.Helper()
	db := openPluginManagerTestDB(t)
	return New(Options{
		DB:           db,
		ArtifactRoot: t.TempDir(),
		Adapter:      adapter,
		Builders:     builders,
	})
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
	var manifest Manifest
	if err := json.Unmarshal(testManifestBytesWithCapabilities(t, pluginID, nil), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	if mutate != nil {
		mutate(&manifest)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("fake plugin bytes " + pluginID),
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
			Type:           RuntimeGoPlugin,
			EntrySymbol:    "Plugin",
			MetadataSymbol: "MCGatewayPluginMetadata",
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
	"encoding/json"
	"net"
	"runtime"

	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginImpl struct{ api.AbstractPlugin }

func Plugin() api.Plugin { return &pluginImpl{} }

func MCGatewayPluginMetadata() string { return manifestJSON }

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

var manifestJSON = compactJSON(map[string]any{
	"schema_version": "mc-gateway.plugin/v1",
	"id": "` + pluginID + `",
	"name": "Buildable Source",
	"version": "0.1.0",
	"artifact_type": "binary",
	"runtime": map[string]any{
		"type": "go-plugin",
		"entry": "plugin.so",
		"entry_symbol": "Plugin",
		"metadata_symbol": "MCGatewayPluginMetadata",
	},
	"api_version": "plugin-api/v1",
	"sdk_module": "github.com/tursom/mc-gateway/plugin/api",
	"sdk_module_version": "v0.1.0",
	"go_version": runtime.Version(),
	"go_os": runtime.GOOS,
	"go_arch": runtime.GOARCH,
	"extension_points": []map[string]any{{"type": "hook", "key": "upstream.connect/v1"}},
	"capabilities": map[string]any{"extension_points": []string{"upstream.connect/v1"}},
})

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
`
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"go.mod":        []byte("module example.com/" + pluginID + "\n\ngo 1.24.0\n\nrequire github.com/tursom/mc-gateway v0.0.0\n\nreplace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"),
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

type fakeAdapter struct {
	loads      int
	handlers   map[string]api.UpstreamConnectHandler
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
	if err := api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(api.UpstreamConnectRequest) bool { return true },
		handler,
	); err != nil {
		return nil, err
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

type fakeBuilder struct {
	result BuildResult
	err    error
}

func (b *fakeBuilder) Build(context.Context, ArtifactRecord, BuildRequest, BuildRecord) (BuildResult, error) {
	return b.result, b.err
}

func newMemoryConn() net.Conn {
	left, right := net.Pipe()
	_ = right.Close()
	return left
}
