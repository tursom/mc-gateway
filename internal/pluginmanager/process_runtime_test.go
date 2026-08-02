package pluginmanager

import (
	"bytes"
	"context"
	"errors"
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

func buildProcessTakeoverPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module example.com/process-takeover\n\ngo 1.24.0\n\nrequire github.com/tursom/mc-gateway v0.0.0\n\nreplace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	source := `package main
import (
	"errors"
	"io"
	"net"
	"github.com/tursom/mc-gateway/plugin/api"
)
type pluginImpl struct{ api.AbstractPlugin }
type replayConn struct { net.Conn; prefix []byte }
func (c *replayConn) Read(p []byte) (int, error) {
	n := copy(p, c.prefix)
	c.prefix = c.prefix[n:]
	if n > 0 { return n, nil }
	r, err := c.Conn.Read(p[n:])
	if n+r > 0 && errors.Is(err, io.EOF) { err = nil }
	return n+r, err
}
func (c *replayConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok { return closer.CloseWrite() }
	return c.Conn.Close()
}
func (c *replayConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok { return closer.CloseRead() }
	return nil
}
func Plugin() api.Plugin { return &pluginImpl{} }
func (p *pluginImpl) Init(g api.Gateway) error {
	return api.RegisterUpstreamConnectHandlerV2(g, func(req api.UpstreamConnectRequestV2) error {
		mode := make([]byte, 1)
		if _, err := io.ReadFull(req.Connection.Stream, mode); err != nil { return err }
		if mode[0] == 'H' {
			_, err := req.Connection.Stream.Write([]byte("handled"))
			return err
		}
		if mode[0] == 'F' {
			if _, err := io.ReadAll(req.Connection.Stream); err != nil { return err }
			_, err := req.Connection.Stream.Write([]byte("handled-half"))
			return err
		}
		if mode[0] == 'B' {
			_, err := io.ReadAll(req.Connection.Stream)
			return err
		}
		state := req.Connection
		state.Stream = &replayConn{Conn: req.Connection.Stream, prefix: mode}
		state.EffectiveSourceAddr = "198.51.100.9:0"
		state.Metadata = map[string]string{"runtime":"process"}
		switch mode[0] {
		case 'N': return req.Flow.Next(state)
		case 'C': return req.Flow.Core(state)
		default: return errors.New("unknown action")
		}
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	tidyProcessPluginModule(t, dir)
	pluginPath := filepath.Join(dir, "plugin.so")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build process takeover plugin: %v\n%s", err, output)
	}
	return pluginPath
}

func TestGoPluginProcessTakeoverConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host supervisor uses Unix-domain sockets")
	}
	gatewayBinary := buildGatewayProcessTestBinary(t)
	pluginBytes, err := os.ReadFile(buildProcessTakeoverPlugin(t))
	if err != nil {
		t.Fatal(err)
	}
	manager := newManagerForTest(t, nil)
	manager.serviceMode = PluginServiceModeGoPluginProcess
	manager.adapter = GoPluginProcessAdapter{Supervisor: PluginHostSupervisor{
		Executable: gatewayBinary, RuntimeDir: shortProcessRuntimeDir(t), Protocol: PluginHostProtocol, StartTimeout: 5 * time.Second,
	}}
	manager.adapterManaged = true
	artifact := uploadTestArtifactWithManifestBytes(t, manager, "process-takeover", pluginBytes, nil)
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	approveGovernanceForTest(t, manager, artifact.PluginID, artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}

	value := manager.snapshot.Load()
	processHandlers := value.([]*upstreamHandler)
	if len(processHandlers) != 1 {
		t.Fatalf("process handlers = %d, want 1", len(processHandlers))
	}
	secondCalled := false
	second := &upstreamHandler{pluginID: "second", priority: 20, handlerID: "second", handle: func(req api.UpstreamConnectRequestV2) error {
		secondCalled = true
		return req.Flow.Next(req.Connection)
	}}
	manager.publish([]*upstreamHandler{processHandlers[0], second})

	for _, test := range []struct {
		name       string
		payload    string
		wantReply  string
		wantSecond bool
		wantCore   bool
	}{
		{name: "next replacement", payload: "Ndata", wantReply: "core", wantSecond: true, wantCore: true},
		{name: "core bypass", payload: "Cdata", wantReply: "core", wantSecond: false, wantCore: true},
		{name: "handled", payload: "H", wantReply: "handled", wantSecond: false, wantCore: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			secondCalled = false
			root, client := net.Pipe()
			defer root.Close()
			defer client.Close()
			coreCalled := false
			done := make(chan error, 1)
			go func() {
				done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(_ context.Context, state api.ConnectionState) error {
					coreCalled = true
					if state.EffectiveSourceAddr != "198.51.100.9:0" || state.Metadata["runtime"] != "process" {
						return fmt.Errorf("replacement state = %+v", state)
					}
					data := make([]byte, len(test.payload))
					if _, err := io.ReadFull(state.Stream, data); err != nil {
						return err
					}
					if string(data) != test.payload {
						return fmt.Errorf("core bytes = %q, want %q", data, test.payload)
					}
					_, err := state.Stream.Write([]byte("core"))
					return err
				})
			}()
			if _, err := client.Write([]byte(test.payload)); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, len(test.wantReply))
			if _, err := io.ReadFull(client, reply); err != nil {
				t.Fatal(err)
			}
			_ = client.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("takeover chain did not finish")
			}
			if string(reply) != test.wantReply || secondCalled != test.wantSecond || coreCalled != test.wantCore {
				t.Fatalf("reply=%q second=%v core=%v", reply, secondCalled, coreCalled)
			}
		})
	}

	t.Run("half-close backpressure and byte integrity", func(t *testing.T) {
		root, client := newTestTCPConnPair(t)
		defer root.Close()
		defer client.Close()
		payload := append([]byte{'N'}, bytes.Repeat([]byte("x"), 512*1024)...)
		done := make(chan error, 1)
		go func() {
			done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(_ context.Context, state api.ConnectionState) error {
				var received []byte
				buf := make([]byte, 4096)
				for {
					n, err := state.Stream.Read(buf)
					received = append(received, buf[:n]...)
					if err != nil {
						if !errors.Is(err, io.EOF) {
							return err
						}
						break
					}
					time.Sleep(100 * time.Microsecond)
				}
				if !bytes.Equal(received, payload) {
					return fmt.Errorf("core bytes = %d, want %d", len(received), len(payload))
				}
				if _, err := state.Stream.Write([]byte("reply")); err != nil {
					return err
				}
				if closer, ok := state.Stream.(interface{ CloseWrite() error }); ok {
					return closer.CloseWrite()
				}
				return nil
			})
		}()
		if _, err := client.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := client.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(client)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("takeover error = %v; reply = %q", err, reply)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("half-closed process takeover did not finish")
		}
		if string(reply) != "reply" {
			t.Fatalf("reply = %q, want reply", reply)
		}
	})

	t.Run("handled half-close", func(t *testing.T) {
		root, client := newTestTCPConnPair(t)
		defer root.Close()
		defer client.Close()
		done := make(chan error, 1)
		go func() {
			done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(context.Context, api.ConnectionState) error {
				return errors.New("core unexpectedly called")
			})
		}()
		if _, err := client.Write([]byte("Fpayload")); err != nil {
			t.Fatal(err)
		}
		if err := client.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(client)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if string(reply) != "handled-half" {
			t.Fatalf("reply = %q, want handled-half", reply)
		}
	})

	t.Run("disable and force close replacement", func(t *testing.T) {
		root, client := net.Pipe()
		defer client.Close()
		coreEntered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(_ context.Context, state api.ConnectionState) error {
				if _, err := io.ReadFull(state.Stream, make([]byte, 1)); err != nil {
					return err
				}
				close(coreEntered)
				_, err := state.Stream.Read(make([]byte, 1))
				return err
			})
		}()
		if _, err := client.Write([]byte("N")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-coreEntered:
		case err := <-done:
			t.Fatalf("takeover returned before core: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("replacement stream did not reach core")
		}
		disabled, err := manager.Disable(context.Background(), "admin", artifact.PluginID)
		if err != nil {
			t.Fatal(err)
		}
		if disabled.RuntimeState != RuntimeDraining {
			t.Fatalf("disabled runtime state = %q, want draining", disabled.RuntimeState)
		}
		closed, err := manager.ForceCloseDraining(context.Background(), "admin", artifact.PluginID)
		if err != nil || closed != 1 {
			t.Fatalf("ForceCloseDraining() = %d, %v", closed, err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("force close did not unwind process takeover")
		}
	})

	t.Run("host crash closes active takeover", func(t *testing.T) {
		if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
			t.Fatal(err)
		}
		manager.mu.Lock()
		process := manager.loaded[artifact.PluginID].runtime.HostProcess
		manager.mu.Unlock()
		if process == nil {
			t.Fatal("re-enabled takeover runtime has no host process")
		}
		root, client := net.Pipe()
		defer client.Close()
		coreCalled := false
		done := make(chan error, 1)
		go func() {
			err := manager.HandleConnection(context.Background(), takeoverRequest(root), func(context.Context, api.ConnectionState) error {
				coreCalled = true
				return nil
			})
			_ = root.Close()
			done <- err
		}()
		if _, err := client.Write([]byte("B")); err != nil {
			t.Fatal(err)
		}
		waitForProcessRuntimeTest(t, func() bool {
			sessions, err := manager.ActiveConnectionSessions(context.Background(), artifact.PluginID)
			return err == nil && len(sessions) == 1
		})
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("active takeover returned nil after host crash")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("host crash did not unwind active takeover")
		}
		if coreCalled {
			t.Fatal("host crash entered core")
		}
		if _, err := client.Read(make([]byte, 1)); err == nil {
			t.Fatal("client connection remained open after host crash")
		}
	})
}

func buildProcessPIDRecordingPlugin(t *testing.T, pidPath string) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module example.com/process-pid\n\ngo 1.24.0\n\nrequire github.com/tursom/mc-gateway v0.0.0\n\nreplace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`package main
import (
	"fmt"
	"os"
	"github.com/tursom/mc-gateway/plugin/api"
)
func init() {
	f, err := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil { _, _ = fmt.Fprintf(f, "%%d\n", os.Getpid()); _ = f.Close() }
}
type pluginImpl struct{ api.AbstractPlugin }
func Plugin() api.Plugin { return &pluginImpl{} }
func (p *pluginImpl) Init(g api.Gateway) error {
	return api.RegisterUpstreamConnectHandlerV2(g, func(api.UpstreamConnectRequestV2) error { return nil })
}
`, pidPath)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	tidyProcessPluginModule(t, dir)
	pluginPath := filepath.Join(dir, "plugin.so")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build process pid plugin: %v\n%s", err, output)
	}
	return pluginPath
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
