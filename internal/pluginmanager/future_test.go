// internal/pluginmanager/future_test.go 包含用于约束 future 行为的测试。

package pluginmanager

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestPluginServiceModeReservedApplyKeepsDataPlaneInProcess(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	state, err := manager.PluginServiceState(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceState() error = %v", err)
	}
	if state.DesiredMode != PluginServiceModeInProcess ||
		state.ActiveMode != PluginServiceModeInProcess ||
		state.DataPlaneMode != PluginServiceModeInProcess ||
		!state.ImplementedAdapter ||
		state.RestartRequired {
		t.Fatalf("default service state = %+v, want in-process without restart", state)
	}
	state, err = manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeGoPluginProcess)
	if err != nil {
		t.Fatalf("SetPluginServiceDesired() error = %v", err)
	}
	if state.DesiredMode != PluginServiceModeGoPluginProcess ||
		state.ActiveMode != PluginServiceModeInProcess ||
		state.DataPlaneMode != PluginServiceModeInProcess ||
		!state.RestartRequired ||
		state.DesiredMaturity != FeatureMaturityPartial ||
		state.UnsupportedReason != "" {
		t.Fatalf("service state after desired switch = %+v, want pending process desired mode", state)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode() error = %v", err)
	}
	state, _ = manager.PluginServiceState(context.Background())
	if state.DesiredMode != PluginServiceModeGoPluginProcess ||
		state.ActiveMode != PluginServiceModeInProcess ||
		state.DataPlaneMode != PluginServiceModeInProcess ||
		!state.RestartRequired ||
		!strings.Contains(state.LastError, "managed runtime adapter") {
		t.Fatalf("service state after apply = %+v, want custom adapter fallback to in-process data plane", state)
	}
}

func TestPluginServiceStatusIncludesModeMaturity(t *testing.T) {
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
	if status.Service.DesiredMode != PluginServiceModeGoPluginProcess ||
		status.Service.ActiveMode != PluginServiceModeInProcess ||
		status.Service.DataPlaneMode != PluginServiceModeInProcess {
		t.Fatalf("service status = %+v, want desired future mode with in-process data plane", status.Service)
	}
	if len(status.Hosts) != 0 {
		t.Fatalf("hosts = %+v, want no process host summaries for reserved mode", status.Hosts)
	}
	if len(status.Nodes) != 1 ||
		status.Nodes[0].NodeID == "" ||
		status.Nodes[0].Status != PluginNodeStatusOnline ||
		status.Nodes[0].DataPlaneMode != PluginServiceModeInProcess ||
		status.Nodes[0].Stale {
		t.Fatalf("nodes = %+v, want one fresh in-process node", status.Nodes)
	}
	processMode := findServiceModeFeature(status.Modes, PluginServiceModeGoPluginProcess)
	if !processMode.Implemented || !processMode.DataPlane || processMode.Maturity != FeatureMaturityPartial ||
		!strings.Contains(processMode.UnsupportedReason, "protocol-proxy drain-only") ||
		!strings.Contains(processMode.UnsupportedReason, "per-node crash isolation") ||
		strings.Contains(processMode.UnsupportedReason, "cross-node crash policy coordination are not implemented") {
		t.Fatalf("go-plugin-process mode = %+v, want partial process data plane", processMode)
	}
	processAdapter := findRuntimeAdapterStatus(status.RuntimeAdapters, PluginServiceModeGoPluginProcess, RuntimeGoPlugin)
	if !processAdapter.Implemented || processAdapter.Maturity != FeatureMaturityPartial || !processAdapter.DataPlane || !processAdapter.Lifecycle ||
		!strings.Contains(processAdapter.UnsupportedReason, "fd-live") ||
		strings.Contains(processAdapter.UnsupportedReason, "cross-node crash policy coordination are not implemented") {
		t.Fatalf("go-plugin-process adapter = %+v, want partial process adapter", processAdapter)
	}
	wasmSandboxAdapter := findRuntimeAdapterStatus(status.RuntimeAdapters, PluginServiceModeSandboxProcess, RuntimeWASM)
	if wasmSandboxAdapter.Implemented || wasmSandboxAdapter.Maturity != FeatureMaturityReserved || wasmSandboxAdapter.DataPlane || !wasmSandboxAdapter.RequiresRestart || !strings.Contains(wasmSandboxAdapter.UnsupportedReason, "WASM data-plane") {
		t.Fatalf("wasm sandbox adapter = %+v, want reserved non-data-plane adapter", wasmSandboxAdapter)
	}
}

func TestRuntimeAdapterLifecycleFactory(t *testing.T) {
	factory := RuntimeAdapterFactory{}
	adapter, status := factory.AdapterFor(PluginServiceModeInProcess, RuntimeBuiltin)
	if !status.Implemented || !status.DataPlane || !status.Lifecycle || status.Adapter != "go-plugin-in-process" {
		t.Fatalf("adapter status = %+v, want implemented in-process lifecycle", status)
	}
	lifecycle, ok := adapter.(RuntimeAdapterLifecycle)
	if !ok {
		t.Fatalf("adapter %T does not implement RuntimeAdapterLifecycle", adapter)
	}
	artifact := ArtifactRecord{
		ID:           "builtin-official-rule-policy-0.1.0",
		PluginID:     "official.rule-policy",
		ArtifactType: ArtifactTypeBinary,
		RuntimeType:  RuntimeBuiltin,
		MetadataJSON: `{"schema_version":"mc-gateway.plugin/v1","id":"official.rule-policy","name":"Official Rule Policy","version":"0.1.0","artifact_type":"binary","runtime":{"type":"builtin"},"api_version":"plugin-api/v1","extension_points":[{"type":"provider","key":"route.resolve/v1"}],"capabilities":{}}`,
	}
	pluginRecord := PluginRecord{ID: artifact.PluginID, ConfigJSON: `{}`}
	prepared, err := lifecycle.Prepare(context.Background(), artifact, pluginRecord)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	instance, err := lifecycle.Start(context.Background(), prepared, artifact, pluginRecord, NewGateway(artifact.PluginID, nil, nil, nil))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if instance.Plugin == nil || instance.PluginID != artifact.PluginID || instance.ArtifactID != artifact.ID {
		t.Fatalf("runtime instance = %+v, want plugin instance with ids", instance)
	}
	if health := lifecycle.HealthCheck(context.Background(), instance); !health.OK || health.Status != RuntimeEnabled {
		t.Fatalf("HealthCheck() = %+v, want enabled", health)
	}
	if err := lifecycle.ReloadConfig(context.Background(), instance, `{}`); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	diag := lifecycle.Diagnostics(context.Background(), instance)
	if diag.PluginID != artifact.PluginID || diag.ArtifactID != artifact.ID || diag.State != RuntimeEnabled {
		t.Fatalf("Diagnostics() = %+v, want enabled plugin diagnostics", diag)
	}
	if err := lifecycle.Drain(context.Background(), instance); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if err := instance.Plugin.Destroy(); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if err := lifecycle.Stop(context.Background(), instance); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestRuntimeAdapterFactoryProcessModeSupportsProcessDataPlane(t *testing.T) {
	factory := RuntimeAdapterFactory{}
	adapter, status := factory.AdapterFor(PluginServiceModeGoPluginProcess, RuntimeGoPlugin)
	if !status.Implemented || status.Maturity != FeatureMaturityPartial || !status.DataPlane || !status.Lifecycle || status.Adapter != "go-plugin-process-host" || !strings.Contains(status.UnsupportedReason, "fd-live") {
		t.Fatalf("process adapter status = %+v, want partial process host adapter", status)
	}
	lifecycle, ok := adapter.(RuntimeAdapterLifecycle)
	if !ok {
		t.Fatalf("adapter %T does not implement lifecycle", adapter)
	}
	if err := lifecycle.ValidateArtifact(context.Background(), ArtifactRecord{
		RuntimeType:             RuntimeGoPlugin,
		ArtifactType:            ArtifactTypeBinary,
		CapabilitiesSummaryJSON: `{"upstream_connect":{"mode":"protocol-proxy"}}`,
	}); err != nil {
		t.Fatalf("ValidateArtifact(process protocol-proxy adapter) error = %v", err)
	}

	adapter, status = factory.AdapterFor(PluginServiceModeSandboxProcess, RuntimeWASM)
	if status.Implemented || status.Maturity != FeatureMaturityReserved || status.DataPlane || status.Adapter != "wazero" || !strings.Contains(status.UnsupportedReason, "WASM data-plane") {
		t.Fatalf("wasm sandbox adapter status = %+v, want reserved wazero adapter status", status)
	}
	if _, ok := adapter.(RuntimeAdapterLifecycle); !ok {
		t.Fatalf("adapter %T does not implement lifecycle", adapter)
	}
}

func TestPluginHostSummariesRefreshLoadedProcessCrash(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.markHostStarted("plugin-a", "artifact-a", nil)

	process := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: "artifact-a",
		PID:        4242,
		StartedAt:  1000,
	}
	process.crashLoop = true
	process.crashCount = 1
	process.lastError = "exit status 2"
	process.exitedAt = 1010
	process.lastCrashAt = 1010

	manager.mu.Lock()
	manager.loaded["plugin-a"] = &loadedPlugin{
		artifact: ArtifactRecord{ID: "artifact-a"},
		runtime:  RuntimeInstance{HostProcess: process},
	}
	manager.mu.Unlock()

	summaries := manager.PluginHostSummaries()
	if len(summaries) != 1 {
		t.Fatalf("PluginHostSummaries() = %+v, want one host", summaries)
	}
	summary := summaries[0]
	if summary.PluginID != "plugin-a" ||
		summary.ArtifactID != "artifact-a" ||
		summary.PID != 4242 ||
		summary.State != RuntimeFailed ||
		!summary.CrashLoop ||
		summary.CrashCount != 1 ||
		summary.LastError != "exit status 2" ||
		summary.LastCrashAt != 1010 ||
		summary.BackoffUntil != 1010+int64(defaultPluginHostCrashBackoff/time.Second) ||
		!summary.Isolated {
		t.Fatalf("host summary = %+v, want refreshed crashed process state", summary)
	}
}

func TestPluginHostCrashBackoffPreventsDeadHostReuse(t *testing.T) {
	adapter := &fakeAdapter{}
	manager := newManagerForTest(t, adapter)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	if _, err := manager.SetPluginServiceCrashPolicy(context.Background(), "admin", PluginHostCrashPolicy{BackoffSeconds: 5}); err != nil {
		t.Fatalf("SetPluginServiceCrashPolicy() error = %v", err)
	}
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	pluginRecord, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10)
	if err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	crashAt := time.Now().Unix()
	process := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: artifact.ID,
		PID:        4242,
		StartedAt:  crashAt - 1,
	}
	process.crashLoop = true
	process.crashCount = 1
	process.lastError = "exit status 2"
	process.exitedAt = crashAt
	process.lastCrashAt = crashAt

	manager.mu.Lock()
	manager.loaded["plugin-a"] = &loadedPlugin{
		record:   pluginRecord,
		artifact: artifact,
		runtime:  RuntimeInstance{HostProcess: process},
	}
	_, err = manager.loadLocked(context.Background(), pluginRecord)
	_, stillLoaded := manager.loaded["plugin-a"]
	manager.mu.Unlock()

	if err == nil || !strings.Contains(err.Error(), "crash loop backoff") {
		t.Fatalf("loadLocked(crashed host) error = %v, want crash loop backoff", err)
	}
	if stillLoaded {
		t.Fatalf("crashed host remained cached in manager.loaded")
	}
	if adapter.loads != 0 {
		t.Fatalf("adapter loads = %d, want no restart while backoff is active", adapter.loads)
	}
	summary := manager.hostSummary("plugin-a")
	if !summary.CrashLoop || summary.BackoffUntil != crashAt+5 || summary.LastError != "exit status 2" {
		t.Fatalf("host summary = %+v, want configured crash-loop backoff", summary)
	}
}

func TestPluginHostCrashAutoIsolatesLoadedPlugin(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if handlers := manager.DispatchPlan(context.Background()).Handlers; len(handlers) != 1 {
		t.Fatalf("dispatch handlers before crash = %+v, want one handler", handlers)
	}

	crashAt := time.Now().Unix()
	process := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: artifact.ID,
		PID:        4242,
		StartedAt:  crashAt - 1,
	}
	process.crashLoop = true
	process.crashCount = 1
	process.lastError = "exit status 2"
	process.exitedAt = crashAt
	process.lastCrashAt = crashAt

	manager.mu.Lock()
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.loaded["plugin-a"].runtime.HostProcess = process
	manager.mu.Unlock()

	summary := findHostStatus(manager.PluginHostSummaries(), "plugin-a")
	if !summary.Isolated || summary.State != RuntimeFailed || summary.BackoffUntil <= time.Now().Unix() {
		t.Fatalf("host summary after crash = %+v, want isolated failed host with backoff", summary)
	}
	if handlers := manager.DispatchPlan(context.Background()).Handlers; len(handlers) != 0 {
		t.Fatalf("dispatch handlers after crash = %+v, want isolated plugin removed", handlers)
	}
	manager.mu.Lock()
	_, stillLoaded := manager.loaded["plugin-a"]
	manager.mu.Unlock()
	if stillLoaded {
		t.Fatalf("isolated crashed plugin remained cached in manager.loaded")
	}
	plugin, err := manager.Plugin(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if plugin.RuntimeState != RuntimeFailed || !strings.Contains(plugin.LastError, "exit status 2") {
		t.Fatalf("plugin after crash isolation = %+v, want failed runtime with crash error", plugin)
	}
	state, err := manager.PluginServiceState(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceState() error = %v", err)
	}
	if !strings.Contains(state.LastError, "exit status 2") {
		t.Fatalf("plugin service state after crash isolation = %+v, want persisted crash error", state)
	}
	if _, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"}); err != nil {
		t.Fatalf("ConnectUpstream(after isolation) error = %v, want pass-through no handler", err)
	}
}

func TestPluginHostCrashPolicyIsolatesAfterWindowThreshold(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	if _, err := manager.SetPluginServiceCrashPolicy(context.Background(), "admin", PluginHostCrashPolicy{BackoffSeconds: 5, MaxCrashes: 2, WindowSeconds: 60}); err != nil {
		t.Fatalf("SetPluginServiceCrashPolicy() error = %v", err)
	}
	artifact := uploadTestArtifact(t, manager, "plugin-a")
	pluginRecord, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10)
	if err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	manager.serviceMode = PluginServiceModeGoPluginProcess

	firstCrashAt := time.Now().Unix()
	firstProcess := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: artifact.ID,
		PID:        4242,
		StartedAt:  firstCrashAt - 1,
	}
	firstProcess.crashLoop = true
	firstProcess.crashCount = 1
	firstProcess.lastError = "first crash"
	firstProcess.exitedAt = firstCrashAt
	firstProcess.lastCrashAt = firstCrashAt
	manager.mu.Lock()
	manager.loaded["plugin-a"] = &loadedPlugin{
		record:   pluginRecord,
		artifact: artifact,
		runtime:  RuntimeInstance{HostProcess: firstProcess},
	}
	manager.mu.Unlock()

	firstSummary := findHostStatus(manager.PluginHostSummaries(), "plugin-a")
	if firstSummary.CrashCount != 1 || firstSummary.CrashLoop || firstSummary.Isolated || firstSummary.BackoffUntil != firstCrashAt+5 {
		t.Fatalf("first crash summary = %+v, want backoff without isolation", firstSummary)
	}
	manager.mu.Lock()
	_, stillLoaded := manager.loaded["plugin-a"]
	manager.mu.Unlock()
	if stillLoaded {
		t.Fatalf("first crashed host remained cached in manager.loaded")
	}

	secondCrashAt := firstCrashAt + 10
	secondProcess := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: artifact.ID,
		PID:        4243,
		StartedAt:  secondCrashAt - 1,
	}
	secondProcess.crashLoop = true
	secondProcess.crashCount = 1
	secondProcess.lastError = "second crash"
	secondProcess.exitedAt = secondCrashAt
	secondProcess.lastCrashAt = secondCrashAt
	manager.mu.Lock()
	manager.loaded["plugin-a"] = &loadedPlugin{
		record:   pluginRecord,
		artifact: artifact,
		runtime:  RuntimeInstance{HostProcess: secondProcess},
	}
	manager.mu.Unlock()

	secondSummary := findHostStatus(manager.PluginHostSummaries(), "plugin-a")
	if secondSummary.CrashCount != 2 || !secondSummary.CrashLoop || !secondSummary.Isolated || secondSummary.BackoffUntil != secondCrashAt+5 {
		t.Fatalf("second crash summary = %+v, want threshold isolation", secondSummary)
	}
}

func TestPluginHostCrashPolicyKeepsPartialRolloutNodeTruth(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	first := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      &fakeAdapter{},
		NodeID:       "node-a",
	})
	second := New(Options{
		DB:           db,
		ArtifactRoot: root,
		Adapter:      &fakeAdapter{},
		NodeID:       "node-b",
	})
	if _, err := first.SetPluginServiceCrashPolicy(context.Background(), "admin", PluginHostCrashPolicy{BackoffSeconds: 9, MaxCrashes: 1, WindowSeconds: 120}); err != nil {
		t.Fatalf("SetPluginServiceCrashPolicy() error = %v", err)
	}
	artifact := uploadTestArtifact(t, first, "plugin-a")
	if _, err := first.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := first.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable(first) error = %v", err)
	}
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(second) error = %v", err)
	}

	second.mu.Lock()
	second.recordPluginNodeRuntimeStateLocked(context.Background(), "plugin-a")
	second.mu.Unlock()
	if err := first.SimulatePluginHostCrash(context.Background(), "plugin-a", "node-a host crash-loop"); err != nil {
		t.Fatalf("SimulatePluginHostCrash() error = %v", err)
	}
	second.mu.Lock()
	second.recordPluginNodeRuntimeStateLocked(context.Background(), "plugin-a")
	second.mu.Unlock()

	rollout, err := first.PluginRolloutStatus(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("PluginRolloutStatus() error = %v", err)
	}
	if rollout.OK || !rollout.PartialFailure || rollout.NodesTotal != 2 || rollout.NodesReady != 1 || rollout.NodesFailed != 1 {
		t.Fatalf("rollout = %+v, want one ready node and one failed node", rollout)
	}
	var readyNode, failedNode PluginNodeRuntimeState
	for _, state := range rollout.NodeRuntimeStates {
		switch state.NodeID {
		case "node-a":
			failedNode = state
		case "node-b":
			readyNode = state
		}
	}
	if readyNode.NodeID != "node-b" ||
		readyNode.ArtifactID != artifact.ID ||
		readyNode.RuntimeState != RuntimeEnabled ||
		!readyNode.Enabled ||
		readyNode.Error != "" {
		t.Fatalf("ready node state = %+v, want node-b still enabled despite node-a crash", readyNode)
	}
	if failedNode.NodeID != "node-a" ||
		failedNode.RuntimeState != RuntimeFailed ||
		!strings.Contains(failedNode.Error, "node-a host crash-loop") {
		t.Fatalf("failed node state = %+v, want node-a crash-loop failure", failedNode)
	}
	service, err := first.PluginServiceState(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceState() error = %v", err)
	}
	if service.CrashPolicy.BackoffSeconds != 9 ||
		service.CrashPolicy.MaxCrashes != 1 ||
		service.CrashPolicy.WindowSeconds != 120 ||
		!strings.Contains(service.LastError, "node-a host crash-loop") {
		t.Fatalf("service state = %+v, want shared crash policy and last error", service)
	}
}

func TestRepositoryPluginNodeStateMarksStaleHeartbeats(t *testing.T) {
	db := openPluginManagerTestDB(t)
	now := time.Unix(1000, 0)
	repo := NewRepositoryWithClock(db, func() time.Time { return now })
	if err := repo.UpsertPluginNode(context.Background(), PluginNodeState{
		NodeID:        "node-a",
		Hostname:      "gateway-a",
		PID:           123,
		ServiceMode:   PluginServiceModeInProcess,
		DataPlaneMode: PluginServiceModeInProcess,
		Status:        PluginNodeStatusOnline,
		StartedAt:     now.Add(-time.Hour).Unix(),
		HeartbeatAt:   now.Add(-10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("UpsertPluginNode() error = %v", err)
	}
	nodes, err := repo.ListPluginNodes(context.Background(), DefaultPluginNodeStaleAfter)
	if err != nil {
		t.Fatalf("ListPluginNodes() error = %v", err)
	}
	if len(nodes) != 1 || !nodes[0].Stale || nodes[0].Status != PluginNodeStatusStale {
		t.Fatalf("nodes = %+v, want stale node", nodes)
	}
}

func TestSandboxRequiredCapabilityBlocksEnable(t *testing.T) {
	manager := newManagerForTest(t, nil)
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
	status, err := manager.PluginServiceStatus(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceStatus() error = %v", err)
	}
	if status.Service.DataPlaneMode != PluginServiceModeInProcess ||
		status.Service.DesiredMaturity != FeatureMaturityReserved ||
		!strings.Contains(status.Service.LastError, "reserved") {
		t.Fatalf("service status after sandbox apply = %+v, want reserved sandbox fallback status", status.Service)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "sandbox-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err == nil || !strings.Contains(err.Error(), "sandbox-process runtime is disabled") {
		t.Fatalf("SetDesired(sandbox desired mode) error = %v, want service mode block", err)
	}
}

func TestSandboxPolicyDiagnosticsAndSecretHandle(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-secret", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.Secrets = []SecretSpec{{Name: "api_token"}}
	})
	if _, err := manager.UpsertSecret(context.Background(), "admin", "sandbox-secret", artifact.ID, "api_token", "plain-secret-value", false, false); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	resp, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: "sandbox-secret", Handle: "api_token"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret() error = %v", err)
	}
	if !resp.OK || resp.Version != 1 {
		t.Fatalf("ResolveSandboxSecret() = %+v, want version-only handle response", resp)
	}
	missing, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: "sandbox-secret", Handle: "missing"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(missing) error = %v", err)
	}
	if missing.OK || !strings.Contains(missing.Error, "not authorized") {
		t.Fatalf("ResolveSandboxSecret(missing) = %+v, want unauthorized handle", missing)
	}
	process := &SandboxProcess{
		PluginID:   "sandbox-secret",
		ArtifactID: artifact.ID,
		PID:        1234,
		StartedAt:  time.Now().Unix(),
		Policy: SandboxPolicy{
			Env:           map[string]string{"SAFE": "1", "API_SECRET": "must-not-enter-env"},
			SecretHandles: []string{"api_token"},
			CPUSeconds:    1,
			MemoryBytes:   8 * 1024 * 1024,
		},
	}
	diag := process.Diagnostics()
	if !diag.ControlRPC || !diag.FilesystemEnforced || !diag.NetworkEnforced || !diag.EnvEnforced || !diag.CPUMemoryEnforced || !diag.SecretRPC {
		t.Fatalf("Diagnostics() = %+v, want all sandbox enforcement flags", diag)
	}
	env := sandboxEnv(process.Policy)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "must-not-enter-env") || strings.Contains(joined, "API_SECRET") || !strings.Contains(joined, "SAFE=1") || !strings.Contains(joined, "MC_GATEWAY_SANDBOX_CONTROL=unix:///run/control.sock") {
		t.Fatalf("sandboxEnv() = %q, want allowlist env without secret material", joined)
	}
	payload, err := json.Marshal(SandboxSecretRequest{Handle: "api_token"})
	if err != nil {
		t.Fatalf("Marshal(secret request) error = %v", err)
	}
	control := process.HandleControlRequest(context.Background(), SandboxControlRequest{
		Command:  sandboxControlCommandSecret,
		Protocol: sandboxProcessProtocol,
		Payload:  payload,
	}, manager)
	if !control.OK || control.Secret == nil || !control.Secret.OK || control.Secret.Version != 1 || control.Secret.Error != "" {
		t.Fatalf("HandleControlRequest(secret) = %+v, want version-only authorized handle", control)
	}
	diagControl := process.HandleControlRequest(context.Background(), SandboxControlRequest{Command: sandboxControlCommandDiagnostics}, manager)
	if !diagControl.OK || diagControl.Diagnostics == nil || !diagControl.Diagnostics.ControlRPC || diagControl.Diagnostics.ControlSocket != "unix:///run/control.sock" {
		t.Fatalf("HandleControlRequest(diagnostics) = %+v, want control RPC diagnostics", diagControl)
	}
}

func TestSandboxFilesystemStagingRejectsEscapes(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}
	root, err := prepareSandboxRoot(executable, SandboxPolicy{FilesystemRoots: []string{"/data/cache"}})
	if err != nil {
		t.Fatalf("prepareSandboxRoot() error = %v", err)
	}
	defer os.RemoveAll(root)
	if _, err := os.Stat(filepath.Join(root, "plugin")); err != nil {
		t.Fatalf("sandbox plugin executable not staged: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "data", "cache")); err != nil || !info.IsDir() {
		t.Fatalf("sandbox filesystem root not staged: info=%+v err=%v", info, err)
	}
	if _, err := prepareSandboxRoot(executable, SandboxPolicy{FilesystemRoots: []string{"../host"}}); err == nil {
		t.Fatal("prepareSandboxRoot(escape) error = nil, want escape rejected")
	}
}

func TestSandboxPolicyBlocksUnenforceableControls(t *testing.T) {
	tests := []struct {
		name   string
		policy SandboxPolicy
		want   string
	}{
		{
			name:   "network enabled",
			policy: SandboxPolicy{NetworkEnabled: true, CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024},
			want:   "network access cannot be enabled",
		},
		{
			name:   "secret env",
			policy: SandboxPolicy{Env: map[string]string{"API_TOKEN": "raw"}, CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024},
			want:   "secret material",
		},
		{
			name:   "missing cpu",
			policy: SandboxPolicy{MemoryBytes: 8 * 1024 * 1024},
			want:   "cpu limit must be positive",
		},
		{
			name:   "missing memory",
			policy: SandboxPolicy{CPUSeconds: 1},
			want:   "memory limit must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSandboxPolicyEnforceable(tt.policy)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateSandboxPolicyEnforceable() error = %v, want %q", err, tt.want)
			}
		})
	}
	if err := validateSandboxPolicyEnforceable(normalizeSandboxPolicy(SandboxPolicy{})); err != nil {
		t.Fatalf("validateSandboxPolicyEnforceable(default policy) error = %v", err)
	}
}

func TestWASMRequiredCapabilityBlocksEnable(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	manager.serviceMode = PluginServiceModeSandboxProcess
	manager.futureGates = FutureRuntimeGates{SandboxProcess: true, WASM: true}
	artifact := uploadTestArtifactWithManifest(t, manager, "wasm-capability-plugin", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}}
		manifest.Capabilities = json.RawMessage(`{"runtime":{"required_capabilities":["network.egress","secret.env"]}}`)
	})
	if result, err := manager.DryRunConfig(context.Background(), "wasm-capability-plugin", artifact.ID, `{}`); err == nil || result.OK || !strings.Contains(err.Error(), "wasm runtime cannot enforce required capabilities") {
		t.Fatalf("DryRunConfig(wasm required capabilities) = %+v err=%v, want enforcement block", result, err)
	}
	preflight := manager.preflightChecks(context.Background(), PluginRecord{ID: "wasm-capability-plugin", ConfigJSON: `{}`}, artifact, mustManifestFromArtifact(t, artifact), PolicyProfileProd, GovernanceActionEnable, `{}`)
	codes := preflightCheckCodes(preflight.Checks)
	if preflight.OK || !codes["capability_enforcement_unavailable"] || codes["wasm_runtime_disabled"] {
		t.Fatalf("preflight = %+v, want wasm required capability enforcement block without service-mode block", preflight)
	}
	foundDetails := false
	for _, check := range preflight.Checks {
		if check.Code != "capability_enforcement_unavailable" {
			continue
		}
		capabilities, _ := check.Details["capabilities"].([]string)
		foundDetails = check.Details["runtime_type"] == RuntimeWASM &&
			len(capabilities) == 2 &&
			capabilities[0] == "network.egress" &&
			capabilities[1] == "secret.env"
	}
	if !foundDetails {
		t.Fatalf("preflight = %+v, want wasm runtime and required capability details", preflight)
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

func TestWASMValidationRejectsUnsupportedExtensionPoint(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "wasm-upstream", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeWASM
		manifest.Runtime.Entry = RuntimeWASMEntry
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionUpstreamConnect}}
	})
	err := manager.RunWASMValidation(context.Background(), "wasm-upstream", artifact.ID, "ok")
	if err == nil || !strings.Contains(err.Error(), "wasm extension point") {
		t.Fatalf("RunWASMValidation(unsupported extension) error = %v, want extension point block", err)
	}
}

func TestIngressServiceSchemaGateDisabledBlocksEnable(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	artifact := uploadTestArtifactWithManifest(t, manager, "ingress-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":25566}}`)
	})
	var summary CapabilitySummary
	if err := json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary); err != nil {
		t.Fatalf("Unmarshal capabilities summary error = %v", err)
	}
	if summary.Ingress == nil || summary.Ingress.Protocol != "tcp" || summary.Ingress.Bind != "127.0.0.1" || summary.Ingress.Port != 25566 {
		t.Fatalf("capabilities summary ingress = %+v, want stored tcp ingress declaration", summary.Ingress)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", "ingress-plugin", artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	preflight, err := manager.RunPreflight(context.Background(), "admin", "ingress-plugin", PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	preflightCodes := preflightCheckCodes(preflight.Checks)
	if preflight.OK || !preflightCodes["ingress_service_schema_valid"] || !preflightCodes["ingress_service_disabled"] || preflightCodes["ingress_service_invalid"] {
		t.Fatalf("preflight = %+v, want schema-valid ingress declaration plus ingress_service_disabled block", preflight)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "ingress-plugin", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "ingress_service_disabled") {
		t.Fatalf("decision = %+v, want ingress_service_disabled block", decision)
	}
	if _, err := manager.Enable(context.Background(), "admin", "ingress-plugin"); err == nil || !strings.Contains(err.Error(), "ingress_service_disabled") {
		t.Fatalf("Enable() error = %v, want ingress_service_disabled", err)
	}
}

func TestIngressServiceInvalidCapabilityBlocksPreflight(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	artifact := uploadTestArtifactWithManifest(t, manager, "bad-ingress-plugin", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Secrets = []SecretSpec{{Name: "tls-cert"}}
		manifest.Capabilities = json.RawMessage(`{
			"extension_points":["ingress.service/v1"],
			"ingress":{
				"protocol":"smtp",
				"bind":"localhost",
				"port":70000,
				"tls":{"enabled":true,"cert_secret":"tls-cert","key_secret":"missing-key"},
				"health":{"path":"ready","interval":"not-a-duration"}
			}
		}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "bad-ingress-plugin", artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	preflight, err := manager.RunPreflight(context.Background(), "admin", "bad-ingress-plugin", PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileProd,
		Action:     GovernanceActionEnable,
		ConfigJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}
	codes := preflightCheckCodes(preflight.Checks)
	if preflight.OK || !codes["ingress_service_invalid"] || !codes["ingress_service_disabled"] || codes["ingress_service_schema_valid"] {
		t.Fatalf("preflight = %+v, want invalid ingress declaration plus disabled block", preflight)
	}
	foundErrors := false
	for _, check := range preflight.Checks {
		if check.Code != "ingress_service_invalid" {
			continue
		}
		errorsValue, ok := check.Details["errors"].([]string)
		if ok && len(errorsValue) >= 4 {
			foundErrors = true
		}
	}
	if !foundErrors {
		t.Fatalf("preflight = %+v, want detailed ingress schema errors", preflight)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "bad-ingress-plugin", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "ingress_service_invalid") || !hasIssueCode(decision.Issues, "ingress_service_disabled") {
		t.Fatalf("decision = %+v, want ingress schema and disabled blocks", decision)
	}
}

func TestIngressServicePortConflictBlocksGovernance(t *testing.T) {
	ctx := context.Background()
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	ownerArtifact := uploadTestArtifactWithManifest(t, manager, "ingress-owner", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"0.0.0.0","port":25566}}`)
	})
	if _, err := manager.SetDesired(ctx, "admin", "ingress-owner", ownerArtifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(owner) error = %v", err)
	}
	if _, err := manager.repo.db.ExecContext(ctx, `
UPDATE plugins
SET active_artifact_id = ?, loaded_artifact_id = ?, desired_state = ?, runtime_state = ?, applied_generation = desired_generation
WHERE id = ?`, ownerArtifact.ID, ownerArtifact.ID, DesiredEnabled, RuntimeEnabled, "ingress-owner"); err != nil {
		t.Fatalf("seed enabled owner error = %v", err)
	}

	targetArtifact := uploadTestArtifactWithManifest(t, manager, "ingress-target", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":25566}}`)
	})
	if _, err := manager.SetDesired(ctx, "admin", "ingress-target", targetArtifact.ID, DesiredDisabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(target) error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(ctx, "ingress-target", targetArtifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "ingress_port_conflict") || !hasIssueCode(decision.Issues, "ingress_service_disabled") {
		t.Fatalf("decision = %+v, want ingress port conflict plus disabled block", decision)
	}
}

func TestIngressServiceReservedListenerConflictBlocksGovernance(t *testing.T) {
	ctx := context.Background()
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	manager.ingressReservedListeners = []IngressReservedListener{{
		Name:    "tcp_admin",
		Network: "tcp",
		Bind:    "0.0.0.0",
		Port:    25575,
		Enabled: true,
	}}
	artifact := uploadTestArtifactWithManifest(t, manager, "reserved-ingress", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":25575}}`)
	})
	if _, err := manager.SetDesired(ctx, "admin", "reserved-ingress", artifact.ID, DesiredDisabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	decision, err := manager.EvaluateGovernance(ctx, "reserved-ingress", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "ingress_reserved_listener_conflict") || !hasIssueCode(decision.Issues, "ingress_service_disabled") {
		t.Fatalf("decision = %+v, want reserved listener conflict plus disabled block", decision)
	}
}

func TestIngressListenerLifecycleRejectsReservedListenerConflict(t *testing.T) {
	ctx := context.Background()
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	manager.futureGates = FutureRuntimeGates{Ingress: true}
	port := reserveFreeTCPPortForTest(t)
	manager.RefreshIngressReservedListeners([]IngressReservedListener{{
		Name:    "tcp_admin",
		Network: "tcp",
		Bind:    "0.0.0.0",
		Port:    port,
		Enabled: true,
	}})
	artifact := uploadTestArtifactWithManifest(t, manager, "reserved-runtime-ingress", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Capabilities = json.RawMessage(fmt.Sprintf(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":%d}}`, port))
	})
	if _, err := manager.StartIngressListener(ctx, "reserved-runtime-ingress", artifact.ID); err == nil || !strings.Contains(err.Error(), "reserved gateway listener") {
		t.Fatalf("StartIngressListener(reserved conflict) error = %v, want reserved listener block", err)
	}
}

func TestIngressListenerLifecycleWithGate(t *testing.T) {
	ctx := context.Background()
	manager := newManagerForTest(t, &fakeAdapter{initOnly: true})
	manager.futureGates = FutureRuntimeGates{Ingress: true}
	manager.RefreshIngressReservedListeners([]IngressReservedListener{{
		Name:    "tcp_admin",
		Network: "tcp",
		Bind:    "127.0.0.1",
		Port:    1,
		Enabled: true,
	}})
	if got := manager.ingressLifecycle.ReservedListeners(); len(got) != 1 || got[0].Name != "tcp_admin" {
		t.Fatalf("ReservedListeners() = %+v, want refreshed reservation", got)
	}
	freePort := reserveFreeTCPPortForTest(t)
	artifact := uploadTestArtifactWithManifest(t, manager, "ingress-runtime", func(manifest *Manifest) {
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "service", Key: ExtensionIngressService}}
		manifest.Secrets = []SecretSpec{{Name: "tls-cert"}, {Name: "tls-key"}}
		manifest.Capabilities = json.RawMessage(fmt.Sprintf(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":%d,"tls":{"enabled":true,"cert_secret":"tls-cert","key_secret":"tls-key"},"health":{"path":"/ready","interval":"1s"}}}`, freePort))
	})
	listener, err := manager.StartIngressListener(ctx, "ingress-runtime", artifact.ID)
	if err != nil {
		t.Fatalf("StartIngressListener() error = %v", err)
	}
	if listener.State != RuntimeEnabled || listener.Health != "healthy" || listener.Port != freePort || listener.TLS["cert_secret"] == "tls-cert" || listener.TLS["key_secret"] == "tls-key" {
		t.Fatalf("listener = %+v, want enabled listener with redacted TLS refs", listener)
	}
	addr := net.JoinHostPort(listener.Bind, fmt.Sprint(listener.Port))
	draining := manager.DrainIngressListener("ingress-runtime")
	if draining.State != RuntimeDraining || draining.DrainingAt == 0 {
		t.Fatalf("DrainIngressListener() = %+v, want draining", draining)
	}
	disabled := manager.DisableIngressListener("ingress-runtime")
	if disabled.State != RuntimeDisabled || disabled.DisabledAt == 0 {
		t.Fatalf("DisableIngressListener() = %+v, want disabled", disabled)
	}
	if conn, err := net.DialTimeout(listener.Network, addr, 20*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatalf("Dial(%s) succeeded after disable, want listener closed", addr)
	}
}

func reserveFreeTCPPortForTest(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(tcp :0) error = %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close(temp listener) error = %v", err)
	}
	return port
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
	var admission GovernanceDecision
	if err := json.Unmarshal([]byte(record.AdmissionJSON), &admission); err != nil {
		t.Fatalf("Unmarshal(admission_json) error = %v", err)
	}
	if admission.Action != GovernanceActionPromotion || admission.Profile != PolicyProfileProd || admission.PolicyHash == "" || admission.CreatedAt == 0 {
		t.Fatalf("admission = %+v, want promotion preview with policy snapshot", admission)
	}
	if _, err := manager.Plugin(context.Background(), "repo-plugin"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-plugin) error = %v, want not found because import must not auto-enable/create desired state", err)
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
		NodeID:       "node-b",
		PluginID:     "repo-plugin",
		ArtifactID:   "missing",
		DesiredState: DesiredDisabled,
		RuntimeState: RuntimeFailed,
		Health:       RuntimeFailed,
		Error:        "artifact distribution retry pending",
	}); err != nil {
		t.Fatalf("UpsertPluginNodeRuntime(node-b) error = %v", err)
	}
	result, err := manager.ApplyRepositoryImport(context.Background(), "admin", record.ID, `{"host":"target-blue"}`, "", 25, false)
	if err != nil {
		t.Fatalf("ApplyRepositoryImport() error = %v", err)
	}
	if !result.OK || result.Status != PromotionStatusReady || result.Applied == nil {
		t.Fatalf("ApplyRepositoryImport() = %+v, want successful desired-state apply", result)
	}
	if result.Rollout == nil || !result.Rollout.ArtifactDistribution || !result.Rollout.CrossNodeApply || !result.Rollout.PartialFailure || result.Rollout.NodesFailed != 1 {
		t.Fatalf("repository rollout = %+v, want automatic artifact distribution with cross-node partial failure", result.Rollout)
	}
	plugin, err := manager.Plugin(context.Background(), "repo-plugin")
	if err != nil {
		t.Fatalf("Plugin(repo-plugin after apply) error = %v", err)
	}
	if plugin.DesiredArtifactID != artifact.ID || plugin.DesiredState != DesiredDisabled || plugin.ActiveArtifactID != "" || plugin.ConfigJSON != `{"host":"target-blue"}` || plugin.Priority != 25 {
		t.Fatalf("plugin after repository apply = %+v, want disabled desired state without active traffic", plugin)
	}
	resultJSON, _ := json.Marshal(result)
	if strings.Contains(string(resultJSON), "target-blue") {
		t.Fatalf("repository import apply result leaked config value:\n%s", resultJSON)
	}
}

func testManifestBytesWithVersion(t *testing.T, pluginID, version string) []byte {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(testManifestBytesWithCapabilities(t, pluginID, nil), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.Version = version
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	return data
}

func TestRepositoryIndexSyncCachesAndDegradesToLastGoodIndex(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	repoDir := t.TempDir()
	artifactPath := filepath.Join(repoDir, "repo-sync-plugin.mcgp")
	tmpPackage := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytesWithVersion(t, "repo-sync-plugin", "0.2.0"),
		RuntimeEntry:    []byte("repo sync plugin bytes"),
	})
	data, err := os.ReadFile(tmpPackage)
	if err != nil {
		t.Fatalf("ReadFile(package) error = %v", err)
	}
	if err := os.WriteFile(artifactPath, data, 0644); err != nil {
		t.Fatalf("WriteFile(repository artifact) error = %v", err)
	}
	indexPath := filepath.Join(repoDir, "index.json")
	indexBytes, _ := json.Marshal(map[string]any{
		"name": "sync-cache",
		"artifacts": []map[string]any{{
			"id":            "repo-sync-plugin-0.2.0",
			"plugin_id":     "repo-sync-plugin",
			"version":       "0.2.0",
			"artifact_path": artifactPath,
		}},
	})
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}
	_, first, err := manager.SyncRepositoryIndex(context.Background(), "admin", RepositoryImportRequest{
		RepositoryType: RepositoryTypeInternal,
		IndexPath:      indexPath,
	})
	if err != nil {
		t.Fatalf("SyncRepositoryIndex(first) error = %v", err)
	}
	if first.Status != RepositorySyncStatusSucceeded || first.CacheKey == "" || first.CandidateCount != 1 {
		t.Fatalf("first sync = %+v, want successful cached index", first)
	}
	if err := os.WriteFile(indexPath, []byte(`{`), 0644); err != nil {
		t.Fatalf("WriteFile(bad index) error = %v", err)
	}
	report, err := manager.RepositoryUpdateAvailability(context.Background(), RepositoryImportRequest{
		RepositoryType: RepositoryTypeInternal,
		IndexPath:      indexPath,
	})
	if err != nil {
		t.Fatalf("RepositoryUpdateAvailability(degraded) error = %v", err)
	}
	if report.Sync.Status != RepositorySyncStatusDegraded || report.RepositoryName != "sync-cache" || len(report.Candidates) != 1 {
		t.Fatalf("update report = %+v, want degraded cache report with candidates", report)
	}
	if _, err := manager.Plugin(context.Background(), "repo-sync-plugin"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-sync-plugin) error = %v, want availability check to avoid desired/active mutation", err)
	}
	syncs, err := manager.repo.ListRepositoryIndexSyncs(context.Background(), RepositoryTypeInternal, indexPath, 10)
	if err != nil {
		t.Fatalf("ListRepositoryIndexSyncs() error = %v", err)
	}
	if len(syncs) < 2 || syncs[0].Status != RepositorySyncStatusDegraded || syncs[1].Status != RepositorySyncStatusSucceeded {
		t.Fatalf("sync records = %+v, want degraded audit after successful cache", syncs)
	}
}

func TestRepositoryUpdateAvailabilityReportsNewerVersionWithoutChangingDesiredState(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	current := uploadTestArtifact(t, manager, "repo-update-plugin")
	if _, err := manager.SetDesired(context.Background(), "admin", "repo-update-plugin", current.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(current) error = %v", err)
	}

	repoDir := t.TempDir()
	availablePackage := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytesWithVersion(t, "repo-update-plugin", "0.2.0"),
		"plugin.so":     []byte("repo update plugin bytes"),
	})
	availableData, err := os.ReadFile(availablePackage)
	if err != nil {
		t.Fatalf("ReadFile(available package) error = %v", err)
	}
	artifactPath := filepath.Join(repoDir, "repo-update-plugin-0.2.0.mcgp")
	if err := os.WriteFile(artifactPath, availableData, 0644); err != nil {
		t.Fatalf("WriteFile(available package) error = %v", err)
	}
	availableSHA, err := fileSHA256(artifactPath)
	if err != nil {
		t.Fatalf("fileSHA256(available) error = %v", err)
	}
	indexPath := filepath.Join(repoDir, "index.json")
	indexBytes, _ := json.Marshal(map[string]any{
		"name": "updates-test",
		"artifacts": []map[string]any{{
			"id":            "repo-update-plugin-0.2.0",
			"plugin_id":     "repo-update-plugin",
			"version":       "0.2.0",
			"artifact_path": artifactPath,
			"sha256":        availableSHA,
		}},
	})
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}

	report, err := manager.RepositoryUpdateAvailability(context.Background(), RepositoryImportRequest{
		RepositoryType: RepositoryTypeFile,
		IndexPath:      indexPath,
		PluginID:       "repo-update-plugin",
	})
	if err != nil {
		t.Fatalf("RepositoryUpdateAvailability() error = %v", err)
	}
	if report.RepositoryName != "updates-test" || len(report.Candidates) != 1 || len(report.Updates) != 1 {
		t.Fatalf("update report = %+v, want one update candidate", report)
	}
	update := report.Updates[0]
	if !update.UpdateAvailable ||
		update.Reason != "newer_version" ||
		update.AvailableVersion != "0.2.0" ||
		update.CurrentVersion != "0.1.0" ||
		update.CurrentArtifactID != current.ID ||
		update.VersionComparison <= 0 ||
		!update.VersionComparisonStable {
		t.Fatalf("update candidate = %+v, want newer version against current desired artifact", update)
	}
	plugin, err := manager.Plugin(context.Background(), "repo-update-plugin")
	if err != nil {
		t.Fatalf("Plugin(repo-update-plugin) error = %v", err)
	}
	if plugin.DesiredArtifactID != current.ID || plugin.ActiveArtifactID != "" || plugin.DesiredState != DesiredDisabled {
		t.Fatalf("plugin after update check = %+v, want unchanged desired state", plugin)
	}
}

func TestRepositoryImportAdmissionEvaluatesGovernanceWithoutDesiredState(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	if _, err := manager.UpsertAdvisory(context.Background(), "security", AdvisoryRequest{
		AdvisoryID: "ADV-REPO-BLOCK",
		PluginID:   "repo-blocked",
		Status:     AdvisoryStatusRevoked,
		Action:     AdvisoryActionRevoke,
		Mitigation: "blocked before repository import apply",
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}

	repoDir := t.TempDir()
	artifactPath := filepath.Join(repoDir, "repo-blocked.mcgp")
	manifestBytes := testManifestBytesWithCapabilities(t, "repo-blocked", nil)
	tmpPackage := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("repo blocked plugin bytes"),
	})
	data, err := os.ReadFile(tmpPackage)
	if err != nil {
		t.Fatalf("ReadFile(package) error = %v", err)
	}
	if err := os.WriteFile(artifactPath, data, 0644); err != nil {
		t.Fatalf("WriteFile(repository artifact) error = %v", err)
	}
	indexPath := filepath.Join(repoDir, "index.json")
	indexBytes, _ := json.Marshal(map[string]any{
		"name": "blocked-test",
		"artifacts": []map[string]any{{
			"id":            "repo-blocked-0.1.0",
			"plugin_id":     "repo-blocked",
			"version":       "0.1.0",
			"artifact_path": artifactPath,
		}},
	})
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}
	record, _, err := manager.ImportRepositoryArtifact(context.Background(), "admin", RepositoryImportRequest{
		RepositoryType: RepositoryTypeFile,
		IndexPath:      indexPath,
		ArtifactID:     "repo-blocked-0.1.0",
	})
	if err != nil {
		t.Fatalf("ImportRepositoryArtifact() error = %v", err)
	}
	var admission GovernanceDecision
	if err := json.Unmarshal([]byte(record.AdmissionJSON), &admission); err != nil {
		t.Fatalf("Unmarshal(admission_json) error = %v", err)
	}
	if admission.OK || admission.Action != GovernanceActionPromotion {
		t.Fatalf("admission = %+v, want blocked promotion preview", admission)
	}
	found := false
	for _, issue := range admission.Issues {
		if issue.Code == "advisory_revoke" && issue.Severity == GateSeverityBlocking {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("admission issues = %+v, want advisory_revoke block", admission.Issues)
	}
	if _, err := manager.Plugin(context.Background(), "repo-blocked"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-blocked) error = %v, want repository import to stay local-only", err)
	}
	result, err := manager.ApplyRepositoryImport(context.Background(), "admin", record.ID, `{}`, "", 0, false)
	if err != nil {
		t.Fatalf("ApplyRepositoryImport(blocked) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "governance_gate") {
		t.Fatalf("blocked repository apply result = %+v, want governance gate block", result)
	}
	if _, err := manager.Plugin(context.Background(), "repo-blocked"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-blocked after blocked apply) error = %v, want not found", err)
	}
}

func TestRepositoryURLImportCreatesLocalArtifactWithoutEnable(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	manifestBytes := testManifestBytesWithCapabilities(t, "repo-url-plugin", nil)
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("repo url plugin bytes"),
	})
	packageSHA, err := fileSHA256(packagePath)
	if err != nil {
		t.Fatalf("fileSHA256() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "url-test",
				"artifacts": []map[string]any{{
					"id":            "repo-url-plugin-0.1.0",
					"plugin_id":     "repo-url-plugin",
					"version":       "0.1.0",
					"artifact_path": "repo-url-plugin.mcgp",
					"sha256":        packageSHA,
				}},
			})
		case "/repo-url-plugin.mcgp":
			http.ServeFile(w, r, packagePath)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	record, artifact, err := manager.ImportRepositoryArtifact(context.Background(), "admin", RepositoryImportRequest{
		RepositoryType: RepositoryTypeURL,
		IndexPath:      server.URL + "/index.json",
		ArtifactID:     "repo-url-plugin-0.1.0",
	})
	if err != nil {
		t.Fatalf("ImportRepositoryArtifact(url) error = %v", err)
	}
	if record.RepositoryType != RepositoryTypeURL || record.RepositoryName != "url-test" || record.ArtifactID != artifact.ID {
		t.Fatalf("url import record = %+v artifact = %+v", record, artifact)
	}
	if artifact.PluginID != "repo-url-plugin" || artifact.PackageSHA256 != packageSHA {
		t.Fatalf("url artifact = %+v, want imported package sha", artifact)
	}
	if _, err := manager.Plugin(context.Background(), "repo-url-plugin"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(repo-url-plugin) error = %v, want not found because import must not auto-enable/create desired state", err)
	}
}

func TestPromotionBundleBlocksMissingSecretMapping(t *testing.T) {
	bundle := PromotionBundle{
		SchemaVersion: SchemaVersion,
		APIVersion:    APIVersion,
		Profile:       PolicyProfileProd,
		Plugins: []PromotionPlugin{{
			PluginID:       "secret-plugin",
			Version:        "0.1.0",
			ArtifactType:   ArtifactTypeBinary,
			RuntimeType:    RuntimeGoPlugin,
			APIVersion:     APIVersion,
			ArtifactSHA256: "artifact-sha",
			DesiredState:   DesiredDisabled,
			SecretRefs:     []string{"api_token"},
		}},
	}
	report := EvaluatePromotionBundle(bundle)
	if report.OK || report.Status != PromotionStatusBlocked {
		t.Fatalf("promotion report = %+v, want blocked report", report)
	}
	foundWarning := false
	foundBlocking := false
	for _, check := range report.Checks {
		if check.Code == "secret_mapping_required" && check.Severity == GateSeverityWarning {
			foundWarning = true
		}
		if check.Code == "secret_mapping_missing" && check.Severity == GateSeverityBlocking {
			foundBlocking = true
		}
	}
	if !foundWarning || !foundBlocking {
		t.Fatalf("promotion checks = %+v, want secret mapping warning and blocking missing mapping", report.Checks)
	}
	bundle.Plugins[0].SecretMapping = map[string]string{"api_token": "prod/api-token"}
	report = EvaluatePromotionBundle(bundle)
	if !report.OK || report.Status != PromotionStatusReady {
		t.Fatalf("mapped promotion report = %+v, want ready after explicit target secret mapping", report)
	}
}

func TestPromotionEnvironmentOverrideAppliesDesiredOnly(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-override")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-override", artifact.ID, `{"host":"blue","limits":{"rate":10}}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	bundle.Plugins[0].Overrides = map[string]any{"host": "green", "limits": map[string]any{"burst": 20}}
	overriddenConfig := `{"host":"green","limits":{"rate":10,"burst":20}}`
	bundle.Plugins[0].ConfigHash = stableHashJSONRaw(defaultJSONObject(overriddenConfig))
	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-override": `{"host":"blue","limits":{"rate":10}}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(override) error = %v", err)
	}
	if !result.OK || len(result.Applied) != 1 {
		t.Fatalf("override apply = %+v, want desired apply", result)
	}
	plugin, err := manager.Plugin(context.Background(), "promotion-override")
	if err != nil {
		t.Fatalf("Plugin(after override apply) error = %v", err)
	}
	if stableHashJSONRaw(defaultJSONObject(plugin.ConfigJSON)) != stableHashJSONRaw(defaultJSONObject(overriddenConfig)) || plugin.DesiredState != DesiredDisabled || plugin.ActiveArtifactID != "" {
		t.Fatalf("plugin after override apply = %+v, want overridden disabled desired without active traffic", plugin)
	}
}

func TestPromotionDriftExplainsDesiredVsActual(t *testing.T) {
	current := PromotionBundle{
		SchemaVersion: SchemaVersion,
		APIVersion:    APIVersion,
		Profile:       PolicyProfileProd,
		Plugins: []PromotionPlugin{{
			PluginID:       "drift-plugin",
			Version:        "0.1.0",
			RuntimeType:    RuntimeGoPlugin,
			APIVersion:     APIVersion,
			ArtifactSHA256: "actual",
			DesiredState:   DesiredDisabled,
			ConfigHash:     "actual-config",
		}},
	}
	baseline := current
	baseline.Plugins = append([]PromotionPlugin(nil), current.Plugins...)
	baseline.Plugins[0].ArtifactSHA256 = "desired"
	baseline.Plugins[0].DesiredState = DesiredEnabled
	baseline.Plugins[0].ConfigHash = "desired-config"
	report := PromotionDrift(current, baseline)
	if report.OK || report.Status != PromotionStatusDrift || len(report.Diff) == 0 {
		t.Fatalf("drift report = %+v, want drift", report)
	}
	for _, diff := range report.Diff {
		if diff.Reason == "" {
			t.Fatalf("drift diff = %+v, want desired vs actual reason", diff)
		}
	}
}

func TestPromotionApplyRequiresConfigHashAndUpdatesDesiredOnly(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-apply")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-apply", artifact.ID, `{"host":"blue"}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-apply": `{"host":"red"}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(mismatch) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "config_hash_mismatch") {
		t.Fatalf("apply mismatch result = %+v, want config_hash_mismatch block", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-apply"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after failed apply) error = %v, want not found", err)
	}

	result, err = manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-apply": `{"host":"blue"}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle() error = %v", err)
	}
	if !result.OK || result.Status != PromotionStatusReady || len(result.Applied) != 1 {
		t.Fatalf("apply result = %+v, want one applied desired state", result)
	}
	plugin, err := manager.Plugin(context.Background(), "promotion-apply")
	if err != nil {
		t.Fatalf("Plugin(after apply) error = %v", err)
	}
	if plugin.DesiredArtifactID != artifact.ID || plugin.DesiredState != DesiredDisabled || plugin.ActiveArtifactID != "" || plugin.ConfigJSON != `{"host":"blue"}` {
		t.Fatalf("plugin after apply = %+v, want disabled desired state without active traffic", plugin)
	}
	if result.Applied[0].PluginID != "promotion-apply" || result.Applied[0].DesiredState != DesiredDisabled || result.Applied[0].ArtifactID != artifact.ID {
		t.Fatalf("applied summary = %+v, want redacted desired metadata", result.Applied[0])
	}
}

func TestPromotionApplyWarningGovernanceDoesNotAdvanceDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "promotion-warning", func(manifest *Manifest) {
		manifest.RuntimeLimits.HandlerTimeoutMS = int(DefaultHandlerTimeout.Milliseconds()) + 1
	})
	plugin, err := manager.SetDesired(context.Background(), "admin", "promotion-warning", artifact.ID, DesiredDisabled, `{}`, 10)
	if err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.CreateWarningOverride(context.Background(), "admin", "promotion-warning", WarningOverrideRequest{
		ArtifactID: artifact.ID,
		Profile:    PolicyProfileDev,
		Action:     GovernanceActionPromotion,
		Reason:     "documented runtime-limit review",
		TTLSeconds: int64(time.Hour / time.Second),
	}); err != nil {
		t.Fatalf("CreateWarningOverride() error = %v", err)
	}
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-warning", artifact.ID, `{}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}

	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-warning": `{}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(warning) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "governance_gate") || !hasPromotionGovernanceIssueCode(result.Checks, "runtime_limits_warning") {
		t.Fatalf("apply warning result = %+v, want governance warning block", result)
	}
	afterApply, err := manager.Plugin(context.Background(), "promotion-warning")
	if err != nil {
		t.Fatalf("Plugin(after warning apply) error = %v", err)
	}
	if afterApply.DesiredGeneration != plugin.DesiredGeneration || afterApply.DesiredState != DesiredDisabled || afterApply.ActiveArtifactID != "" {
		t.Fatalf("plugin after warning apply = %+v, want unchanged disabled desired state", afterApply)
	}

	drill, err := manager.RunPromotionDRDrill(context.Background(), bundle, map[string]string{
		"promotion-warning": `{}`,
	})
	if err != nil {
		t.Fatalf("RunPromotionDRDrill(warning) error = %v", err)
	}
	if drill.OK || !hasPromotionCheckCode(drill.Checks, "governance_gate") || !hasPromotionGovernanceIssueCode(drill.Checks, "runtime_limits_warning") {
		t.Fatalf("DR drill warning result = %+v, want governance warning block", drill)
	}
	afterDrill, err := manager.Plugin(context.Background(), "promotion-warning")
	if err != nil {
		t.Fatalf("Plugin(after warning drill) error = %v", err)
	}
	if afterDrill.DesiredGeneration != plugin.DesiredGeneration || afterDrill.DesiredState != DesiredDisabled || afterDrill.ActiveArtifactID != "" {
		t.Fatalf("plugin after warning drill = %+v, want unchanged disabled desired state", afterDrill)
	}
}

func TestPromotionApplyUnsupportedPolicyProfileDoesNotCreateDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-policy-unsupported")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-policy-unsupported", artifact.ID, `{}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	bundle.Profile = "official"

	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-policy-unsupported": `{}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(unsupported profile) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "policy_profile_unsupported") {
		t.Fatalf("apply unsupported profile result = %+v, want policy_profile_unsupported block", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-policy-unsupported"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after unsupported profile apply) error = %v, want not found", err)
	}

	drill, err := manager.RunPromotionDRDrill(context.Background(), bundle, map[string]string{
		"promotion-policy-unsupported": `{}`,
	})
	if err != nil {
		t.Fatalf("RunPromotionDRDrill(unsupported profile) error = %v", err)
	}
	if drill.OK || !hasPromotionCheckCode(drill.Checks, "policy_profile_unsupported") {
		t.Fatalf("DR drill unsupported profile result = %+v, want policy_profile_unsupported block", drill)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-policy-unsupported"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after unsupported profile drill) error = %v, want not found", err)
	}
}

func TestPromotionApplyUnsupportedRuntimeDoesNotCreateDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-runtime-unsupported")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-runtime-unsupported", artifact.ID, `{}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	bundle.Plugins[0].RuntimeType = "nodejs"
	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-runtime-unsupported": `{}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(unsupported runtime) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "runtime_unsupported") {
		t.Fatalf("apply unsupported runtime result = %+v, want runtime_unsupported block", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-runtime-unsupported"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after unsupported runtime apply) error = %v, want not found", err)
	}
}

func TestPromotionDRDrillChecksTargetArtifactConfigAndDoesNotApply(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-drill")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileDev, "promotion-drill", artifact.ID, `{"host":"blue"}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	result, err := manager.RunPromotionDRDrill(context.Background(), bundle, map[string]string{
		"promotion-drill": `{"host":"red"}`,
	})
	if err != nil {
		t.Fatalf("RunPromotionDRDrill(mismatch) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "config_hash_mismatch") {
		t.Fatalf("DR drill mismatch result = %+v, want config_hash_mismatch block", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-drill"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after blocked drill) error = %v, want not found", err)
	}

	result, err = manager.RunPromotionDRDrill(context.Background(), bundle, map[string]string{
		"promotion-drill": `{"host":"blue"}`,
	})
	if err != nil {
		t.Fatalf("RunPromotionDRDrill() error = %v", err)
	}
	if !result.OK || result.Status != PromotionStatusReady {
		t.Fatalf("DR drill result = %+v, want ready", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-drill"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after successful drill) error = %v, want not found because drill must not apply desired state", err)
	}
}

func TestPromotionApplyGovernanceBlockDoesNotCreateDesired(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "promotion-revoked")
	bundle, err := manager.ExportPromotionBundle(context.Background(), "test", PolicyProfileProd, "promotion-revoked", artifact.ID, `{}`)
	if err != nil {
		t.Fatalf("ExportPromotionBundle() error = %v", err)
	}
	if _, err := manager.UpsertAdvisory(context.Background(), "secops", AdvisoryRequest{
		AdvisoryID:     "ADV-PROMO-1",
		Status:         AdvisoryStatusRevoked,
		Action:         AdvisoryActionRevoke,
		ArtifactSHA256: artifact.SHA256,
	}); err != nil {
		t.Fatalf("UpsertAdvisory() error = %v", err)
	}
	result, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		"promotion-revoked": `{}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle(revoked) error = %v", err)
	}
	if result.OK || !hasPromotionCheckCode(result.Checks, "governance_gate") {
		t.Fatalf("apply revoked result = %+v, want governance gate block", result)
	}
	if _, err := manager.Plugin(context.Background(), "promotion-revoked"); err != ErrPluginNotFound {
		t.Fatalf("Plugin(after blocked apply) error = %v, want not found", err)
	}
}

func TestTrustRootRotationVerifyRevokeAuditsLifecycle(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "signed-plugin")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	data, err := os.ReadFile(manager.store.DistributionPackagePath(artifact))
	if err != nil {
		t.Fatalf("ReadFile(distribution package) error = %v", err)
	}
	signature := ed25519.Sign(privateKey, data)
	root, err := manager.RotateTrustRoot(context.Background(), "secops", TrustRootRequest{
		RootID:    "official",
		KeyID:     "ci-key",
		PublicKey: base64.StdEncoding.EncodeToString(publicKey),
		Policy:    map[string]any{"profile": PolicyProfileProd},
	})
	if err != nil {
		t.Fatalf("RotateTrustRoot() error = %v", err)
	}
	if root.Status != TrustRootStatusTrusted || root.PublicKeySHA256 == "" || root.PolicyJSON == "{}" {
		t.Fatalf("trust root = %+v, want trusted distributed root with policy", root)
	}
	verified, err := manager.VerifyArtifactSignature(context.Background(), "secops", SignatureVerificationRequest{
		ArtifactID: artifact.ID,
		RootID:     "official",
		KeyID:      "ci-key",
		Signature:  base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		t.Fatalf("VerifyArtifactSignature() error = %v", err)
	}
	if !verified.Verified || !verified.SignatureValid || !verified.Trusted || verified.TrustStatus != TrustRootStatusTrusted {
		t.Fatalf("verified = %+v, want trusted valid signature", verified)
	}
	revoked, err := manager.RevokeTrustRoot(context.Background(), "secops", TrustRootRevokeRequest{
		RootID: "official",
		KeyID:  "ci-key",
		Reason: "compromised",
	})
	if err != nil {
		t.Fatalf("RevokeTrustRoot() error = %v", err)
	}
	if revoked.Status != TrustRootStatusRevoked || revoked.RevocationReason != "compromised" {
		t.Fatalf("revoked = %+v, want revoked key", revoked)
	}
	verified, err = manager.VerifyArtifactSignature(context.Background(), "secops", SignatureVerificationRequest{
		ArtifactID: artifact.ID,
		RootID:     "official",
		KeyID:      "ci-key",
		Signature:  base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		t.Fatalf("VerifyArtifactSignature(revoked) error = %v", err)
	}
	if verified.Verified || !verified.SignatureValid || verified.Trusted || !verified.Revoked || verified.RevocationReason != "compromised" {
		t.Fatalf("verified revoked = %+v, want valid signature blocked by revoked trust root", verified)
	}
	if _, err := manager.RotateTrustRoot(context.Background(), "secops", TrustRootRequest{
		RootID:    "official",
		KeyID:     "ci-key",
		PublicKey: base64.StdEncoding.EncodeToString(publicKey),
	}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("RotateTrustRoot(revoked key id) error = %v, want revoked key id blocked", err)
	}
	ops, err := manager.repo.ListOperations(context.Background(), "", 20)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	if !operationRecorded(ops, "trust_root_distribution:succeeded") ||
		!operationRecorded(ops, "trust_root_revoke:succeeded") ||
		!operationRecorded(ops, "signature_verify:failed") {
		t.Fatalf("operations = %+v, want trust distribution/revoke/signature audit", ops)
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

func TestSupplyChainAssessmentAllowsTrustedExternalCIArtifact(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "external-ci-trusted", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"dependencies":[{"name":"example.com/external-ci-trusted","version":"v0.1.0"}]}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-trusted", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() before assessment error = %v", err)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "external-ci-trusted", artifact.ID, map[string]any{
		"signature": map[string]any{"verified": true},
		"sbom":      map[string]any{"required": true, "scan_ok": true},
		"external_ci": map[string]any{
			"required":        true,
			"trusted":         true,
			"source_sha256":   "src-sha",
			"artifact_sha256": artifact.SHA256,
			"package_sha256":  artifact.PackageSHA256,
			"run_id":          "github-actions/run-123",
			"builder_id":      "github-actions/mc-gateway-plugin-build",
			"attestation":     "slsa-v1",
			"sbom":            "sbom.spdx.json",
			"release_provenance": map[string]any{
				"gateway_release": "v0.1.0",
				"builder":         "ghcr.io/tursom/mc-gateway-plugin-builder",
			},
		},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain(external ci trusted) error = %v", err)
	}
	if assessment.Status != SupplyChainStatusAllowed || len(assessment.Issues) != 0 {
		t.Fatalf("assessment = %+v, want allowed external CI provenance", assessment)
	}
	externalCI := jsonMapFromAny(assessment.Metadata["external_ci"])
	if externalCI["provenance_complete"] != true || externalCI["artifact_sha256_matches"] != true || externalCI["package_sha256_matches"] != true {
		t.Fatalf("external CI metadata = %+v, want provenance/hash match markers", externalCI)
	}
	if _, err := manager.Enable(context.Background(), "admin", "external-ci-trusted"); err != nil {
		t.Fatalf("Enable() with trusted external CI provenance error = %v", err)
	}
}

func TestExternalCIProvenanceBlocksRepositoryAndPromotionApply(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileProd)
	artifact := uploadTestArtifactWithProvenance(t, manager, "external-ci-apply-blocked", map[string]any{
		"external_ci": map[string]any{
			"artifact_sha256": "wrong-artifact-sha",
			"run_id":          "github-actions/run-789",
		},
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-apply-blocked", artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	repoRecord, err := manager.repo.SaveRepositoryImport(context.Background(), RepositoryImportRecord{
		RepositoryType: RepositoryTypeFile,
		RepositoryName: "local",
		CandidateID:    "external-ci-apply-blocked",
		PluginID:       artifact.PluginID,
		Version:        artifact.Version,
		ArtifactID:     artifact.ID,
		PackageSHA256:  artifact.PackageSHA256,
		TrustPolicy:    "external-ci",
		ImportedBy:     "admin",
	})
	if err != nil {
		t.Fatalf("SaveRepositoryImport() error = %v", err)
	}
	repoResult, err := manager.ApplyRepositoryImport(context.Background(), "admin", repoRecord.ID, `{}`, "", 0, false)
	if err != nil {
		t.Fatalf("ApplyRepositoryImport() error = %v", err)
	}
	if repoResult.OK ||
		!hasPromotionCheckCode(repoResult.Checks, "governance_gate") ||
		!hasPromotionGovernanceIssueCode(repoResult.Checks, "external_ci_artifact_hash_mismatch") ||
		!hasPromotionGovernanceIssueCode(repoResult.Checks, "external_ci_provenance_incomplete") {
		t.Fatalf("repository apply result = %+v, want external CI governance gate block", repoResult)
	}
	bundle := NewPromotionBundle("test", PolicyProfileProd, artifact, mustManifestFromArtifact(t, artifact), `{}`)
	promotionResult, err := manager.ApplyPromotionBundle(context.Background(), "admin", bundle, map[string]string{
		artifact.PluginID: `{}`,
	}, false)
	if err != nil {
		t.Fatalf("ApplyPromotionBundle() error = %v", err)
	}
	if promotionResult.OK ||
		!hasPromotionCheckCode(promotionResult.Checks, "governance_gate") ||
		!hasPromotionGovernanceIssueCode(promotionResult.Checks, "external_ci_artifact_hash_mismatch") ||
		!hasPromotionGovernanceIssueCode(promotionResult.Checks, "external_ci_provenance_incomplete") {
		t.Fatalf("promotion apply result = %+v, want external CI governance gate block", promotionResult)
	}
}

func TestExternalCIProvenanceMetadataBlocksEnableWithoutRequiredFlag(t *testing.T) {
	manager := newManagerForTestWithBuildersProfile(t, &fakeAdapter{}, nil, PolicyProfileProd)
	artifact := uploadTestArtifactWithProvenance(t, manager, "external-ci-enable-blocked", map[string]any{
		"external_ci": map[string]any{
			"source_sha256":   "src-sha",
			"artifact_sha256": "wrong-artifact-sha",
			"run_id":          "github-actions/run-enable",
			"builder_id":      "github-actions/mc-gateway-plugin-build",
		},
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-enable-blocked", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	_, err := manager.Enable(context.Background(), "admin", "external-ci-enable-blocked")
	if err == nil || !strings.Contains(err.Error(), "external_ci_") {
		t.Fatalf("Enable() error = %v, want external CI governance block", err)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "external-ci-enable-blocked", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if !governanceIssueCodes(decision.Issues)["external_ci_provenance_incomplete"] {
		t.Fatalf("decision = %+v, want external_ci_provenance_incomplete", decision)
	}
}

func TestExternalCIProvenanceRequiresTopLevelSignatureAndSBOM(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "external-ci-metadata-required")
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-metadata-required", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() before assessment error = %v", err)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "external-ci-metadata-required", artifact.ID, map[string]any{
		"external_ci": map[string]any{
			"required":           true,
			"trusted":            true,
			"signature_verified": true,
			"source_sha256":      "src-sha",
			"artifact_sha256":    artifact.SHA256,
			"package_sha256":     artifact.PackageSHA256,
			"run_id":             "github-actions/run-metadata",
			"builder_id":         "github-actions/mc-gateway-plugin-build",
			"attestation":        "slsa-v1",
			"sbom":               "sbom.spdx.json",
			"release_provenance": map[string]any{"gateway_release": "v0.1.0"},
		},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain() error = %v", err)
	}
	if assessment.Status != SupplyChainStatusBlocked ||
		!hasIssueCode(assessment.Issues, "external_ci_provenance_incomplete") ||
		!hasIssueCode(assessment.Issues, "external_ci_signature_unverified") {
		t.Fatalf("assessment = %+v, want missing top-level signature/SBOM to block external CI provenance", assessment)
	}
	externalCI := jsonMapFromAny(assessment.Metadata["external_ci"])
	missing := stringSlice(externalCI["missing_fields"])
	if externalCI["provenance_complete"] != false ||
		!containsString(missing, "signature") ||
		!containsString(missing, "sbom_metadata") {
		t.Fatalf("external CI metadata = %+v, want signature and sbom_metadata missing fields", externalCI)
	}
}

func TestSupplyChainAssessmentBlocksUntrustedExternalCIArtifact(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifact(t, manager, "external-ci-blocked")
	if _, err := manager.SetDesired(context.Background(), "admin", "external-ci-blocked", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() before assessment error = %v", err)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "external-ci-blocked", artifact.ID, map[string]any{
		"external_ci": map[string]any{
			"required":        true,
			"artifact_sha256": "wrong-artifact-sha",
			"run_id":          "github-actions/run-456",
		},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain(external ci blocked) error = %v", err)
	}
	if assessment.Status != SupplyChainStatusBlocked ||
		!hasIssueCode(assessment.Issues, "external_ci_provenance_incomplete") ||
		!hasIssueCode(assessment.Issues, "external_ci_artifact_hash_mismatch") ||
		!hasIssueCode(assessment.Issues, "external_ci_signature_unverified") ||
		!hasIssueCode(assessment.Issues, "external_ci_untrusted") {
		t.Fatalf("assessment = %+v, want blocking external CI provenance issues", assessment)
	}
	decision, err := manager.EvaluateGovernance(context.Background(), "external-ci-blocked", artifact.ID, GovernanceActionEnable, PolicyProfileProd, `{}`)
	if err != nil {
		t.Fatalf("EvaluateGovernance() error = %v", err)
	}
	if decision.OK || !hasIssueCode(decision.Issues, "external_ci_artifact_hash_mismatch") {
		t.Fatalf("decision = %+v, want external CI assessment to block governance", decision)
	}
}

func TestSupplyChainAssessmentEvaluatesLicensePolicy(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "license-plugin", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"license":"GPL-3.0-only"}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "license-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() before license assessment error = %v", err)
	}
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "license-plugin", artifact.ID, map[string]any{
		"license_policy": map[string]any{
			"allowed":       []any{"MIT"},
			"denied":        []any{"GPL-3.0-only"},
			"allow_unknown": false,
		},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain(license policy) error = %v", err)
	}
	if assessment.Status != SupplyChainStatusBlocked {
		t.Fatalf("assessment status = %s, want blocked", assessment.Status)
	}
	if !containsString(stringSlice(assessment.License["denylist_matches"]), "GPL-3.0-ONLY") ||
		!containsString(stringSlice(assessment.License["allowlist_missing"]), "GPL-3.0-ONLY") {
		t.Fatalf("assessment license = %+v, want derived denylist and allowlist matches", assessment.License)
	}
	if !hasIssueCode(assessment.Issues, "license_denylist") || !hasIssueCode(assessment.Issues, "license_allowlist_missing") {
		t.Fatalf("assessment issues = %+v, want derived license policy issues", assessment.Issues)
	}
	if _, err := manager.Enable(context.Background(), "admin", "license-plugin"); err == nil {
		t.Fatal("Enable() error = nil, want license policy to block enable")
	}
}

func TestSupplyChainAssessmentLicenseReviewWarning(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "review-license-plugin", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"license":"MPL-2.0"}`)
	})
	assessment, err := manager.AssessSupplyChain(context.Background(), "admin", "review-license-plugin", artifact.ID, map[string]any{
		"license_policy": map[string]any{
			"allowed":         []any{"MPL-2.0"},
			"review_required": []any{"MPL-2.0"},
		},
	})
	if err != nil {
		t.Fatalf("AssessSupplyChain(review policy) error = %v", err)
	}
	if assessment.Status != SupplyChainStatusWarning {
		t.Fatalf("assessment status = %s, want warning", assessment.Status)
	}
	if !containsString(stringSlice(assessment.License["review_required_matches"]), "MPL-2.0") {
		t.Fatalf("assessment license = %+v, want review required match", assessment.License)
	}
	if !hasIssueCode(assessment.Issues, "license_review_required") {
		t.Fatalf("assessment issues = %+v, want review required warning", assessment.Issues)
	}
}

func TestAdvisoryFeedSyncRescansAndQuarantines(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "feed-plugin", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"dependencies":[{"name":"example.com/bad","version":"v1.2.3"}]}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "feed-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "feed-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	result, err := manager.SyncAdvisoryFeed(context.Background(), "admin", AdvisoryFeedRequest{
		Source: "unit-feed",
		Advisories: []AdvisoryRequest{{
			AdvisoryID:        "MCG-FEED-0001",
			Status:            AdvisoryStatusActive,
			Action:            AdvisoryActionQuarantine,
			DependencyName:    "example.com/bad",
			DependencyRange:   "<2.0.0",
			RecommendedAction: "upgrade",
			FixedVersion:      "v2.0.0",
		}},
	})
	if err != nil {
		t.Fatalf("SyncAdvisoryFeed() error = %v", err)
	}
	if result.Source != "unit-feed" || result.Imported != 1 || result.Rescan.Scanned == 0 || result.Rescan.Blocking != 1 || result.Rescan.QuarantineRuns != 1 || result.Rescan.OK {
		t.Fatalf("feed result = %+v, want blocking quarantine rescan", result)
	}
	if len(result.Rescan.Matches) != 1 || result.Rescan.Matches[0].ArtifactID != artifact.ID || !result.Rescan.Matches[0].Active {
		t.Fatalf("feed rescan matches = %+v, want active artifact match", result.Rescan.Matches)
	}
	plugin, err := manager.Plugin(context.Background(), "feed-plugin")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if plugin.RuntimeState != RuntimeDraining {
		t.Fatalf("plugin runtime state = %s, want draining after advisory quarantine", plugin.RuntimeState)
	}
	report, err := manager.RescanAdvisories(context.Background(), "admin", "feed-plugin", artifact.ID)
	if err != nil {
		t.Fatalf("RescanAdvisories() error = %v", err)
	}
	if report.Scanned != 1 || report.Blocking != 1 || len(report.Matches) != 1 || report.OK {
		t.Fatalf("rescan report = %+v, want blocking match for selected artifact", report)
	}
}

func TestSBOMParseFeedsVulnerabilityScan(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytesWithCapabilities(t, "sbom-imported-plugin", nil),
		RuntimeEntry:    []byte("sbom imported plugin bytes"),
		"sbom.json": []byte(`{
			"schema_version":"mc-gateway.sbom/v1",
			"dependencies":[{"name":"example.com/vulnerable","version":"v1.2.3","license":"MIT"}]
		}`),
	})
	artifact, err := manager.UploadArtifact(context.Background(), ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "sbom-imported-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact(sbom package) error = %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		t.Fatalf("Unmarshal(metadata manifest) error = %v", err)
	}
	if len(sbomDependencies(manifest)) != 1 || sbomDependencies(manifest)[0].Name != "example.com/vulnerable" {
		t.Fatalf("metadata supply chain = %s, want parsed SBOM dependency", artifact.MetadataJSON)
	}
	result, err := manager.ImportVulnerabilityDB(context.Background(), "security", VulnerabilityDBRequest{
		Source: "imported-sbom-test",
		Vulnerabilities: []VulnerabilityRequest{{
			VulnerabilityID: "CVE-2026-SBOM",
			PackageName:     "example.com/vulnerable",
			VersionRange:    "<= v1.2.3",
			Severity:        VulnerabilitySeverityCritical,
			Action:          AdvisoryActionDenylist,
		}},
	})
	if err != nil {
		t.Fatalf("ImportVulnerabilityDB(sbom) error = %v", err)
	}
	if result.Scan.Scanned == 0 || result.Scan.Blocking != 1 || result.Scan.OK {
		t.Fatalf("vulnerability scan = %+v, want blocking match from parsed SBOM", result.Scan)
	}
}

func TestVulnerabilityDatabaseImportScansAndBlocksGovernance(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	artifact := uploadTestArtifactWithManifest(t, manager, "vuln-plugin", func(manifest *Manifest) {
		manifest.SupplyChain = json.RawMessage(`{"dependencies":[{"name":"example.com/bad","version":"v1.2.3"}]}`)
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "vuln-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "vuln-plugin"); err != nil {
		t.Fatalf("Enable() before vulnerability import error = %v", err)
	}
	result, err := manager.ImportVulnerabilityDB(context.Background(), "admin", VulnerabilityDBRequest{
		Source: "unit-vuln-db",
		Vulnerabilities: []VulnerabilityRequest{{
			VulnerabilityID: "CVE-2026-0001",
			PackageName:     "example.com/bad",
			VersionRange:    "<2.0.0",
			Severity:        VulnerabilitySeverityCritical,
			Action:          AdvisoryActionQuarantine,
			FixedVersion:    "v2.0.0",
			Summary:         "test vulnerable package",
			References:      []string{"https://example.test/CVE-2026-0001"},
		}},
	})
	if err != nil {
		t.Fatalf("ImportVulnerabilityDB() error = %v", err)
	}
	if result.Source != "unit-vuln-db" || result.Imported != 1 || result.Scan.Scanned == 0 || result.Scan.Blocking != 1 || result.Scan.QuarantineRuns != 1 || result.Scan.OK {
		t.Fatalf("vulnerability import result = %+v, want blocking quarantine scan", result)
	}
	if len(result.Scan.Matches) != 1 ||
		result.Scan.Matches[0].ArtifactID != artifact.ID ||
		result.Scan.Matches[0].PackageName != "example.com/bad" ||
		result.Scan.Matches[0].PackageVersion != "v1.2.3" ||
		!result.Scan.Matches[0].Active {
		t.Fatalf("vulnerability matches = %+v, want active dependency match", result.Scan.Matches)
	}
	plugin, err := manager.Plugin(context.Background(), "vuln-plugin")
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if plugin.RuntimeState != RuntimeDraining {
		t.Fatalf("plugin runtime state = %s, want draining after vulnerability quarantine", plugin.RuntimeState)
	}
	report, err := manager.ScanVulnerabilities(context.Background(), "admin", "vuln-plugin", artifact.ID)
	if err != nil {
		t.Fatalf("ScanVulnerabilities() error = %v", err)
	}
	if report.Scanned != 1 || report.Blocking != 1 || len(report.Matches) != 1 || report.OK {
		t.Fatalf("targeted vulnerability scan = %+v, want blocking match", report)
	}
	records, err := manager.ListVulnerabilities(context.Background(), "example.com/bad")
	if err != nil {
		t.Fatalf("ListVulnerabilities() error = %v", err)
	}
	if len(records) != 1 || records[0].VulnerabilityID != "CVE-2026-0001" || len(records[0].References) != 1 {
		t.Fatalf("vulnerability records = %+v, want imported record with references", records)
	}
	if _, err := manager.Enable(context.Background(), "admin", "vuln-plugin"); err == nil || !strings.Contains(err.Error(), "vulnerability_quarantine") {
		t.Fatalf("Enable() after vulnerability import error = %v, want vulnerability governance block", err)
	}
}

func TestInstrumentationIsNotRuntimePlugin(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	record, err := manager.SaveInstrumentation(context.Background(), "admin", InstrumentationRequest{
		Name:                "official-trace",
		Version:             "0.1.0",
		Profile:             "official",
		GeneratedDiffHash:   "abc123",
		GatewayBinarySHA256: "gatewayabc123",
		CIArtifactSHA256:    "ciabc123",
		Provenance: map[string]any{
			"policy_hash": "policyabc123",
			"governance": map[string]any{
				"ok":          true,
				"action":      GovernanceActionPromotion,
				"profile":     PolicyProfileProd,
				"policy_hash": "policyabc123",
			},
		},
		Conformance: map[string]any{"ok": true},
		Benchmark:   map[string]any{"ok": true},
		Smoke:       map[string]any{"ok": true},
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

func TestInstrumentationAvailableRequiresReleaseEvidence(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	base := InstrumentationRequest{
		Name:                "official-trace",
		Version:             "0.1.0",
		Profile:             "official",
		GeneratedDiffHash:   "abc123",
		GatewayBinarySHA256: "gatewayabc123",
		CIArtifactSHA256:    "ciabc123",
		Provenance: map[string]any{
			"policy_hash": "policyabc123",
			"governance": map[string]any{
				"ok":          true,
				"action":      GovernanceActionPromotion,
				"profile":     PolicyProfileProd,
				"policy_hash": "policyabc123",
			},
		},
		Conformance: map[string]any{"ok": true},
		Benchmark:   map[string]any{"ok": true},
		Smoke:       map[string]any{"ok": true},
	}
	for _, tc := range []struct {
		name string
		req  InstrumentationRequest
		want string
	}{
		{
			name: "missing generated diff",
			req: func() InstrumentationRequest {
				req := base
				req.GeneratedDiffHash = ""
				return req
			}(),
			want: "generated_diff_hash",
		},
		{
			name: "missing gateway binary binding",
			req: func() InstrumentationRequest {
				req := base
				req.GatewayBinarySHA256 = ""
				return req
			}(),
			want: "gateway_binary_sha256",
		},
		{
			name: "missing ci artifact binding",
			req: func() InstrumentationRequest {
				req := base
				req.CIArtifactSHA256 = ""
				return req
			}(),
			want: "ci_artifact_sha256",
		},
		{
			name: "failed conformance",
			req: func() InstrumentationRequest {
				req := base
				req.Conformance = map[string]any{"ok": false}
				return req
			}(),
			want: "conformance",
		},
		{
			name: "failed benchmark",
			req: func() InstrumentationRequest {
				req := base
				req.Benchmark = map[string]any{"ok": false}
				return req
			}(),
			want: "benchmark",
		},
		{
			name: "failed smoke",
			req: func() InstrumentationRequest {
				req := base
				req.Smoke = map[string]any{"ok": false}
				return req
			}(),
			want: "smoke",
		},
	} {
		_, err := manager.SaveInstrumentation(context.Background(), "admin", tc.req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: SaveInstrumentation() error = %v, want %q gate", tc.name, err, tc.want)
		}
	}
	record, err := manager.SaveInstrumentation(context.Background(), "admin", InstrumentationRequest{
		Name:              "official-trace",
		Version:           "0.1.0",
		Profile:           "official",
		GeneratedDiffHash: "abc123",
		Status:            InstrumentationStatusBlocked,
		Conformance:       map[string]any{"ok": false},
		Benchmark:         map[string]any{"ok": false},
		Smoke:             map[string]any{"ok": false},
	})
	if err != nil {
		t.Fatalf("SaveInstrumentation(blocked failed conformance) error = %v", err)
	}
	if record.Status != InstrumentationStatusBlocked {
		t.Fatalf("instrumentation status = %q, want blocked", record.Status)
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

func findServiceModeFeature(modes []PluginServiceModeFeature, mode string) PluginServiceModeFeature {
	for _, feature := range modes {
		if feature.Mode == mode {
			return feature
		}
	}
	return PluginServiceModeFeature{}
}

func findRuntimeAdapterStatus(statuses []RuntimeAdapterFactoryStatus, mode, runtimeType string) RuntimeAdapterFactoryStatus {
	for _, status := range statuses {
		if status.ServiceMode == mode && status.RuntimeType == runtimeType {
			return status
		}
	}
	return RuntimeAdapterFactoryStatus{}
}

func mustManifestFromArtifact(t *testing.T, artifact ArtifactRecord) Manifest {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		t.Fatalf("Unmarshal artifact manifest error = %v", err)
	}
	return manifest
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasIssueCode(issues []GovernanceIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func hasPromotionCheckCode(checks []PromotionCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
			return true
		}
	}
	return false
}

func hasPromotionGovernanceIssueCode(checks []PromotionCheck, code string) bool {
	for _, check := range checks {
		if check.Code != "governance_gate" || check.Details == nil {
			continue
		}
		decision, ok := check.Details["decision"].(GovernanceDecision)
		if !ok {
			continue
		}
		if hasIssueCode(decision.Issues, code) {
			return true
		}
	}
	return false
}
