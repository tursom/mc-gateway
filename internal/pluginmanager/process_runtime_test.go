package pluginmanager

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestGoPluginProcessProtocolProxyBridgeDrainsAndStopsHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host stream bridge uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pluginPath := buildProcessProtocolProxyPlugin(t)
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin) error = %v", err)
	}

	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true

	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-proxy", pluginBytes, func(manifest *Manifest) {
		manifest.Capabilities = testProtocolProxyCapabilities()
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "process-proxy", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "process-proxy", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "process-proxy"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	initial := []byte("initial")
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:        "play.example",
			Upstream:    "backend",
			Source:      clientGateway,
			InitialData: initial,
			SourceAddr:  "client.example:25565",
		})
		errCh <- err
	}()

	writeDone := make(chan error, 1)
	go func() {
		_, err := clientSide.Write([]byte("next"))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("client write error = %v", err)
		}
	case err := <-errCh:
		t.Fatalf("ConnectUpstream() returned before client write: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client write timed out waiting for process protocol-proxy reader")
	}
	response := make([]byte, len("ok:client.example:25565"))
	if _, err := io.ReadFull(clientSide, response); err != nil {
		t.Fatalf("client read response error = %v", err)
	}
	if !bytes.Equal(response, []byte("ok:client.example:25565")) {
		t.Fatalf("plugin response = %q, want source-address response", response)
	}
	waitForProcessRuntimeTest(t, func() bool {
		return manager.activeProxyCountLocked("process-proxy") == 1
	})
	handler := manager.findHandler("process-proxy", "upstream.connect/v1")
	if handler == nil {
		t.Fatal("process-proxy handler not found")
	}

	disabled, err := manager.Disable(context.Background(), "admin", "process-proxy")
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if disabled.RuntimeState != RuntimeDraining {
		t.Fatalf("disabled runtime state = %q, want draining while process proxy is active", disabled.RuntimeState)
	}
	summary := manager.hostSummary("process-proxy")
	if summary.State != RuntimeDraining || summary.ExitedAt != 0 {
		t.Fatalf("host summary after disable = %+v, want active draining host", summary)
	}
	pass, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"})
	if err != nil {
		t.Fatalf("ConnectUpstream(after disable) error = %v", err)
	}
	if pass.Handled {
		t.Fatalf("ConnectUpstream(after disable) = %+v, want pass-through", pass)
	}

	_ = clientSide.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if got, want := handler.proxyBytesIn.Load(), uint64(len(initial)+len("next")); got != want {
		t.Fatalf("process proxy bytes in = %d, want %d", got, want)
	}
	if got, want := handler.proxyBytesOut.Load(), uint64(len(response)); got != want {
		t.Fatalf("process proxy bytes out = %d, want %d", got, want)
	}
	waitForProcessRuntimeTest(t, func() bool {
		summary := manager.hostSummary("process-proxy")
		return summary.State == RuntimeDisabled && summary.ExitedAt != 0
	})
	assertProcessExited(t, summary.PID)
}

func TestGoPluginProcessDoesNotOpenPluginInGatewayProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host supervisor uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pidPath := filepath.Join(t.TempDir(), "plugin-open-pids.txt")
	pluginPath := buildProcessPIDRecordingPlugin(t, pidPath)
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin) error = %v", err)
	}

	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true

	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-pid", pluginBytes, nil)
	if _, err := manager.SetDesired(context.Background(), "admin", "process-pid", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "process-pid", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "process-pid"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	summary := manager.hostSummary("process-pid")
	if summary.PID == 0 || summary.PID == os.Getpid() {
		t.Fatalf("host summary = %+v, want child plugin-host PID", summary)
	}
	waitForProcessRuntimeTest(t, func() bool {
		data, err := os.ReadFile(pidPath)
		return err == nil && strings.Contains(string(data), strconv.Itoa(summary.PID))
	})
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("ReadFile(pidPath) error = %v", err)
	}
	pids := strings.Fields(string(data))
	if len(pids) == 0 {
		t.Fatalf("plugin pid file is empty")
	}
	gatewayPID := strconv.Itoa(os.Getpid())
	hostPID := strconv.Itoa(summary.PID)
	for _, pid := range pids {
		if pid == gatewayPID {
			t.Fatalf("plugin.Open ran in gateway test process pid %s; pids=%v", gatewayPID, pids)
		}
	}
	if !containsProcessRuntimePID(pids, hostPID) {
		t.Fatalf("plugin.Open pids=%v, want host pid %s", pids, hostPID)
	}
	if _, err := manager.Disable(context.Background(), "admin", "process-pid"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	waitForProcessRuntimeTest(t, func() bool {
		summary := manager.hostSummary("process-pid")
		return summary.State == RuntimeDisabled && summary.ExitedAt != 0
	})
	assertProcessExited(t, summary.PID)
}

func TestGoPluginProcessUpstreamDialerBridgeUsesHostProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host stream bridge uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pluginPath := buildProcessDialerPlugin(t)
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin) error = %v", err)
	}

	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true

	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-dialer", pluginBytes, nil)
	if _, err := manager.SetDesired(context.Background(), "admin", "process-dialer", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "process-dialer", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "process-dialer"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	summary := manager.hostSummary("process-dialer")
	hostPID := summary.PID
	if hostPID == 0 || hostPID == os.Getpid() {
		t.Fatalf("host summary = %+v, want child plugin-host PID", summary)
	}

	result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
		Host:       "play.example",
		Upstream:   "backend",
		SourceAddr: "client.example:25565",
	})
	if err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	if !result.Handled || result.Mode != UpstreamModeDialer || result.Conn == nil || result.Proxied {
		t.Fatalf("ConnectUpstream() = %+v, want dialer bridge conn", result)
	}
	defer result.Conn.Close()
	if _, err := result.Conn.Write([]byte("ping")); err != nil {
		t.Fatalf("dialer bridge write error = %v", err)
	}
	response := make([]byte, len("pong:client.example:25565"))
	if _, err := io.ReadFull(result.Conn, response); err != nil {
		t.Fatalf("dialer bridge read error = %v", err)
	}
	if !bytes.Equal(response, []byte("pong:client.example:25565")) {
		t.Fatalf("dialer bridge response = %q, want source-address response", response)
	}

	if _, err := manager.Disable(context.Background(), "admin", "process-dialer"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	waitForProcessRuntimeTest(t, func() bool {
		summary := manager.hostSummary("process-dialer")
		return summary.State == RuntimeDisabled && summary.ExitedAt != 0
	})
	assertProcessExited(t, hostPID)
}

func TestGoPluginProcessProtocolProxyForceCloseStopsDrainingHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host stream bridge uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pluginPath := buildProcessProtocolProxyPlugin(t)
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin) error = %v", err)
	}

	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true

	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-proxy-force", pluginBytes, func(manifest *Manifest) {
		manifest.Capabilities = testProtocolProxyCapabilities()
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "process-proxy-force", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "process-proxy-force", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "process-proxy-force"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	summary := manager.hostSummary("process-proxy-force")
	hostPID := summary.PID
	if hostPID == 0 {
		t.Fatalf("host summary before drain = %+v, want process pid", summary)
	}

	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:        "play.example",
			Upstream:    "backend",
			Source:      clientGateway,
			InitialData: []byte("initial"),
			SourceAddr:  "client.example:25565",
		})
		errCh <- err
	}()
	if _, err := clientSide.Write([]byte("next")); err != nil {
		t.Fatalf("client write error = %v", err)
	}
	response := make([]byte, len("ok:client.example:25565"))
	if _, err := io.ReadFull(clientSide, response); err != nil {
		t.Fatalf("client read response error = %v", err)
	}
	waitForProcessRuntimeTest(t, func() bool {
		return manager.activeProxyCountLocked("process-proxy-force") == 1
	})
	handler := manager.findHandler("process-proxy-force", "upstream.connect/v1")
	if handler == nil {
		t.Fatal("process-proxy-force handler not found")
	}
	if _, err := manager.Disable(context.Background(), "admin", "process-proxy-force"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	active, err := manager.ActiveProxyConnections(context.Background(), "process-proxy-force")
	if err != nil {
		t.Fatalf("ActiveProxyConnections() error = %v", err)
	}
	if len(active) != 1 || !active[0].Draining || active[0].ForceCloseRequested {
		t.Fatalf("active proxy after disable = %+v, want one draining connection", active)
	}
	closed, err := manager.ForceCloseDraining(context.Background(), "admin", "process-proxy-force")
	if err != nil {
		t.Fatalf("ForceCloseDraining() error = %v", err)
	}
	if closed != 1 {
		t.Fatalf("ForceCloseDraining() = %d, want 1", closed)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	waitForProcessRuntimeTest(t, func() bool {
		active, _ := manager.ActiveProxyConnections(context.Background(), "process-proxy-force")
		return len(active) == 0 && manager.activeProxyCountLocked("process-proxy-force") == 0
	})
	waitForProcessRuntimeTest(t, func() bool {
		summary := manager.hostSummary("process-proxy-force")
		return summary.State == RuntimeDisabled && summary.ExitedAt != 0
	})
	if got := handler.proxyForceClosed.Load(); got != 1 {
		t.Fatalf("proxy force-closed count = %d, want 1", got)
	}
	if got := handler.drainingProxy.Load(); got != 0 {
		t.Fatalf("draining proxy count = %d, want 0", got)
	}
	assertProcessExited(t, hostPID)
}

func TestGoPluginProcessAdapterReportsRealHostCrashWithoutExitingGateway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host supervisor uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	supervisor := PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, _, err := supervisor.Start(ctx, "process-crash", "artifact-crash")
	if err != nil {
		t.Fatalf("Start(plugin-host) error = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = process.Stop(stopCtx)
	})

	instance := RuntimeInstance{
		RuntimePrepared: RuntimePrepared{
			PluginID:   "process-crash",
			ArtifactID: "artifact-crash",
			Runtime:    RuntimeGoPlugin,
			Mode:       PluginServiceModeGoPluginProcess,
			PreparedAt: time.Now().Unix(),
		},
		Plugin:      processHostedPlugin{process: process},
		HostProcess: process,
		StartedAt:   time.Now().Unix(),
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill(plugin-host) error = %v", err)
	}
	select {
	case <-process.done:
		stopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("plugin-host process did not exit after kill")
	}

	adapter := GoPluginProcessAdapter{}
	health := adapter.HealthCheck(context.Background(), instance)
	if health.OK ||
		health.Status != RuntimeFailed ||
		health.Error == "" ||
		health.Details["pid"] != process.PID ||
		health.Details["drain_mode"] != PluginMigrationDrainOnly ||
		health.Details["crash_loop"] != true ||
		health.Details["crash_count"] != 1 ||
		health.Details["last_error"] != health.Error ||
		health.Details["started_at"] == int64(0) ||
		health.Details["exited_at"] == int64(0) ||
		health.Details["last_crash_at"] == int64(0) {
		t.Fatalf("HealthCheck(crashed host) = %+v, want failed crash details", health)
	}

	diag := adapter.Diagnostics(context.Background(), instance)
	if diag.PluginID != "process-crash" ||
		diag.ArtifactID != "artifact-crash" ||
		diag.Mode != PluginServiceModeGoPluginProcess ||
		diag.State != RuntimeFailed ||
		diag.Details["pid"] != process.PID ||
		diag.Details["drain_mode"] != PluginMigrationDrainOnly ||
		diag.Details["crash_loop"] != true ||
		diag.Details["crash_count"] != 1 ||
		diag.Details["last_error"] != health.Error ||
		diag.Details["exited_at"] == int64(0) ||
		diag.Details["last_crash_at"] == int64(0) {
		t.Fatalf("Diagnostics(crashed host) = %+v, want failed crash details", diag)
	}
}

func TestGoPluginProcessCrashUpdatesManagerStateWithoutExitingGateway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host supervisor uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pidPath := filepath.Join(t.TempDir(), "plugin-open-pids.txt")
	pluginPath := buildProcessPIDRecordingPlugin(t, pidPath)
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin) error = %v", err)
	}

	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable:   gatewayBinary,
		RuntimeDir:   shortProcessRuntimeDir(t),
		Protocol:     PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true

	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-crash-state", pluginBytes, nil)
	if _, err := manager.SetDesired(context.Background(), "admin", "process-crash-state", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, "process-crash-state", artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", "process-crash-state"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	manager.mu.Lock()
	loaded := manager.loaded["process-crash-state"]
	manager.mu.Unlock()
	if loaded == nil || loaded.runtime.HostProcess == nil {
		t.Fatalf("loaded process runtime = %+v, want host process", loaded)
	}
	process := loaded.runtime.HostProcess
	hostPID := process.PID
	if hostPID == 0 || hostPID == os.Getpid() {
		t.Fatalf("host PID = %d, want child process distinct from gateway PID %d", hostPID, os.Getpid())
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill(plugin-host) error = %v", err)
	}
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
		t.Fatal("plugin-host process did not exit after kill")
	}

	waitForProcessRuntimeTest(t, func() bool {
		for _, summary := range manager.PluginHostSummaries() {
			if summary.PluginID == "process-crash-state" {
				return summary.State == RuntimeFailed &&
					summary.CrashLoop &&
					summary.CrashCount == 1 &&
					summary.LastError != "" &&
					summary.ExitedAt != 0 &&
					summary.LastCrashAt != 0
			}
		}
		return false
	})
	summary := manager.hostSummary("process-crash-state")
	if summary.PID != hostPID || !summary.Isolated || summary.BackoffUntil == 0 {
		t.Fatalf("host summary after crash = %+v, want isolated failed host with backoff", summary)
	}
	if handlers := manager.DispatchPlan(context.Background()).Handlers; len(handlers) != 0 {
		t.Fatalf("dispatch handlers after crash = %+v, want crashed plugin removed", handlers)
	}
	manager.mu.Lock()
	_, stillLoaded := manager.loaded["process-crash-state"]
	manager.mu.Unlock()
	if stillLoaded {
		t.Fatal("crashed plugin remained cached in manager.loaded")
	}
	plugin, err := manager.repo.Plugin(context.Background(), "process-crash-state")
	if err != nil {
		t.Fatalf("Plugin(process-crash-state) error = %v", err)
	}
	if plugin.RuntimeState != RuntimeFailed || plugin.LastError == "" || !strings.Contains(plugin.RuntimeSummaryJSON, "plugin_host") {
		t.Fatalf("plugin after crash = %+v, want failed runtime with plugin_host summary", plugin)
	}
	service, err := manager.PluginServiceState(context.Background())
	if err != nil {
		t.Fatalf("PluginServiceState() error = %v", err)
	}
	if service.LastError == "" || !strings.Contains(service.LastError, summary.LastError) {
		t.Fatalf("service state = %+v, want crash error propagated", service)
	}
	nodes, err := manager.repo.ListPluginNodeRuntime(context.Background(), "process-crash-state", 0)
	if err != nil {
		t.Fatalf("ListPluginNodeRuntime() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].RuntimeState != RuntimeFailed || nodes[0].Enabled || nodes[0].Error == "" {
		t.Fatalf("node runtime states = %+v, want failed disabled node with crash error", nodes)
	}
	if os.Getpid() <= 0 {
		t.Fatal("gateway/Admin parent process exited during plugin-host crash")
	}
	assertProcessExited(t, hostPID)
}

func TestPluginHostUpstreamRequestCarriesDeadline(t *testing.T) {
	deadline := time.Now().Add(500 * time.Millisecond).Truncate(time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	req := pluginHostUpstreamRequest(api.UpstreamConnectRequest{
		Context: ctx,
		Host:    "play.example",
	})
	if req.Host != "play.example" || req.DeadlineUnixMS != deadline.UnixMilli() {
		t.Fatalf("pluginHostUpstreamRequest() = %+v, want deadline %d", req, deadline.UnixMilli())
	}
}

func TestPluginHostUpstreamRequestUsesCallerDeadlineThroughHandlerTimeout(t *testing.T) {
	callerDeadline := time.Now().Add(2 * time.Second).Truncate(time.Millisecond)
	handlerDeadline := time.Now().Add(100 * time.Millisecond).Truncate(time.Millisecond)
	callerCtx, callerCancel := context.WithDeadline(context.Background(), callerDeadline)
	defer callerCancel()
	handlerCtx, handlerCancel := context.WithDeadline(context.WithValue(callerCtx, pluginHostCallerContextKey{}, callerCtx), handlerDeadline)
	defer handlerCancel()
	req := pluginHostUpstreamRequest(api.UpstreamConnectRequest{
		Context: handlerCtx,
		Host:    "play.example",
	})
	if req.DeadlineUnixMS != callerDeadline.UnixMilli() {
		t.Fatalf("pluginHostUpstreamRequest() deadline = %d, want caller deadline %d", req.DeadlineUnixMS, callerDeadline.UnixMilli())
	}
}

func TestContextBoundConnClosesOnCancel(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := &contextBoundConn{Conn: local, stop: bindConnToContext(ctx, local)}
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := remote.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("remote read error = nil, want relay close after context cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not close relay connection")
	}
	_ = wrapped.Close()
}

func buildGatewayProcessTestBinary(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	binaryPath := filepath.Join(t.TempDir(), "gateway-process-test")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./cmd/gateway")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build gateway test binary error = %v\n%s", err, output)
	}
	return binaryPath
}

func shortProcessRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mcgph-")
	if err != nil {
		t.Fatalf("MkdirTemp(runtime dir) error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func buildProcessProtocolProxyPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	goMod := "module example.com/process-proxy\n\n" +
		"go 1.24.0\n\n" +
		"require github.com/tursom/mc-gateway v0.0.0\n\n" +
		"replace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}
	source := `package main

import (
	"bytes"
	"io"
	"net"

	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginImpl struct {
	api.AbstractPlugin
}

func Plugin() api.Plugin { return &pluginImpl{} }

func (p *pluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool { return req.Host == "play.example" },
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			gatewayEnd, pluginEnd := net.Pipe()
			initial := append([]byte(nil), req.InitialData...)
			sourceAddr := req.SourceAddr
			go func() {
				defer pluginEnd.Close()
				buf := make([]byte, len(initial)+len("next"))
				if _, err := io.ReadFull(pluginEnd, buf); err != nil {
					return
				}
				if !bytes.Equal(buf[:len(initial)], initial) || string(buf[len(initial):]) != "next" {
					_, _ = pluginEnd.Write([]byte("bad"))
					return
				}
				_, _ = pluginEnd.Write([]byte("ok:" + sourceAddr))
				_, _ = pluginEnd.Read(make([]byte, 1))
			}()
			return gatewayEnd, nil
		},
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	tidyProcessPluginModule(t, dir)
	pluginPath := filepath.Join(dir, "plugin.so")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build process protocol proxy plugin error = %v\n%s", err, output)
	}
	return pluginPath
}

func buildProcessPIDRecordingPlugin(t *testing.T, pidPath string) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	goMod := "module example.com/process-pid\n\n" +
		"go 1.24.0\n\n" +
		"require github.com/tursom/mc-gateway v0.0.0\n\n" +
		"replace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}
	source := fmt.Sprintf(`package main

import (
	"fmt"
	"net"
	"os"

	"github.com/tursom/mc-gateway/plugin/api"
)

func init() {
	file, err := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		_, _ = fmt.Fprintf(file, "%%d\n", os.Getpid())
		_ = file.Close()
	}
}

type pluginImpl struct {
	api.AbstractPlugin
}

func Plugin() api.Plugin { return &pluginImpl{} }

func (p *pluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(api.UpstreamConnectRequest) bool { return true },
		func(api.UpstreamConnectRequest) (net.Conn, error) {
			left, right := net.Pipe()
			go func() {
				_ = right.Close()
			}()
			return left, nil
		},
	)
}
`, pidPath)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	tidyProcessPluginModule(t, dir)
	pluginPath := filepath.Join(dir, "plugin.so")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build process pid plugin error = %v\n%s", err, output)
	}
	return pluginPath
}

func buildProcessDialerPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	goMod := "module example.com/process-dialer\n\n" +
		"go 1.24.0\n\n" +
		"require github.com/tursom/mc-gateway v0.0.0\n\n" +
		"replace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}
	source := `package main

import (
	"io"
	"net"

	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginImpl struct {
	api.AbstractPlugin
}

func Plugin() api.Plugin { return &pluginImpl{} }

func (p *pluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool { return req.Host == "play.example" },
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			gatewayEnd, pluginEnd := net.Pipe()
			sourceAddr := req.SourceAddr
			go func() {
				defer pluginEnd.Close()
				buf := make([]byte, len("ping"))
				if _, err := io.ReadFull(pluginEnd, buf); err != nil {
					return
				}
				if string(buf) != "ping" {
					_, _ = pluginEnd.Write([]byte("bad"))
					return
				}
				_, _ = pluginEnd.Write([]byte("pong:" + sourceAddr))
			}()
			return gatewayEnd, nil
		},
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	tidyProcessPluginModule(t, dir)
	pluginPath := filepath.Join(dir, "plugin.so")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build process dialer plugin error = %v\n%s", err, output)
	}
	return pluginPath
}

func containsProcessRuntimePID(pids []string, want string) bool {
	for _, pid := range pids {
		if pid == want {
			return true
		}
	}
	return false
}

func assertProcessExited(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || runtime.GOOS != "linux" {
		return
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
		t.Fatalf("process pid %d is still alive after plugin-host stop", pid)
	}
}

func tidyProcessPluginModule(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod tidy process plugin error = %v\n%s", err, output)
	}
}

func waitForProcessRuntimeTest(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for process runtime condition")
		case <-ticker.C:
			if done() {
				return
			}
		}
	}
}
