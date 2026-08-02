package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

func TestPluginHostTakeoverStreamCloseCancelsHandlerContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	gateway := pluginmanager.NewGateway("cancel-plugin", nil, nil)
	if err := api.RegisterUpstreamConnectHandlerV2(gateway, func(req api.UpstreamConnectRequestV2) error {
		close(started)
		<-req.Context.Done()
		close(canceled)
		return req.Context.Err()
	}); err != nil {
		t.Fatal(err)
	}
	server := &pluginHostControlServer{
		gateway: gateway, state: pluginmanager.RuntimeEnabled,
		socketPath: filepath.Join(t.TempDir(), "plugin-host.sock"),
		takeovers:  make(map[string]*pluginHostTakeoverSession),
	}
	stream, err := server.openTakeover(pluginmanager.PluginHostTakeoverRequest{SessionID: "cancel-session"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("takeover handler did not start")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stream close did not cancel takeover handler context")
	}
}

func TestPluginHostTakeoverFlowExpiresWhenHandlerReturns(t *testing.T) {
	flow := &pluginHostTakeoverFlow{}
	used, running := flow.handlerReturned()
	if used || running {
		t.Fatalf("handlerReturned() = (%v, %v), want false, false", used, running)
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	err := flow.Next(api.ConnectionState{Stream: left})
	if err == nil || err.Error() != "takeover continuation is no longer available" {
		t.Fatalf("retained Next() error = %v, want unavailable continuation", err)
	}
}

func TestPluginHostHandshakeCommand(t *testing.T) {
	output := captureStdout(t, func() {
		handled, code := runPluginHostCLI([]string{"plugin-host", "handshake", "--protocol", pluginmanager.PluginHostProtocol})
		if !handled {
			t.Fatal("runPluginHostCLI() handled = false")
		}
		if code != 0 {
			t.Fatalf("runPluginHostCLI(handshake) code = %d, want 0", code)
		}
	})
	var handshake pluginmanager.PluginHostHandshake
	if err := json.Unmarshal([]byte(output), &handshake); err != nil {
		t.Fatalf("Unmarshal(handshake) error = %v\n%s", err, output)
	}
	if handshake.SchemaVersion != pluginmanager.PluginHostSchemaVersion ||
		handshake.Protocol != pluginmanager.PluginHostProtocol ||
		handshake.PID <= 0 ||
		handshake.ControlChannel != pluginmanager.PluginHostControlChannelUnix ||
		handshake.DataPlane ||
		!handshake.LifecycleImplemented ||
		!containsString(handshake.Capabilities, "stream.deadline") ||
		!containsString(handshake.Capabilities, "stream.cancel") {
		t.Fatalf("handshake = %+v, want lifecycle and stream control foundation without data plane", handshake)
	}
}

func TestPluginHostControlSocketHandshakeAndShutdown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "plugin-host.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- servePluginHostControl(socketPath, pluginmanager.PluginHostProtocol)
	}()
	waitForPluginHostSocket(t, socketPath)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial(plugin-host socket) error = %v", err)
	}
	defer conn.Close()
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(pluginmanager.PluginHostControlRequest{
		RequestID: "handshake-1",
		Command:   pluginmanager.PluginHostCommandHandshake,
		Protocol:  pluginmanager.PluginHostProtocol,
	}); err != nil {
		t.Fatalf("Encode(handshake) error = %v", err)
	}
	var resp pluginmanager.PluginHostControlResponse
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("Decode(handshake) error = %v", err)
	}
	if !resp.OK || resp.RequestID != "handshake-1" || resp.Handshake == nil || resp.Handshake.Protocol != pluginmanager.PluginHostProtocol {
		t.Fatalf("handshake response = %+v, want OK protocol response", resp)
	}

	if err := encoder.Encode(pluginmanager.PluginHostControlRequest{
		RequestID: "init-1",
		Command:   pluginmanager.PluginHostCommandInit,
		Protocol:  pluginmanager.PluginHostProtocol,
	}); err != nil {
		t.Fatalf("Encode(init) error = %v", err)
	}
	resp = pluginmanager.PluginHostControlResponse{}
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("Decode(init) error = %v", err)
	}
	if resp.OK || resp.Code != "invalid_request" {
		t.Fatalf("init response = %+v, want invalid request without lifecycle payload", resp)
	}

	if err := encoder.Encode(pluginmanager.PluginHostControlRequest{
		RequestID: "shutdown-1",
		Command:   pluginmanager.PluginHostCommandShutdown,
		Protocol:  pluginmanager.PluginHostProtocol,
	}); err != nil {
		t.Fatalf("Encode(shutdown) error = %v", err)
	}
	resp = pluginmanager.PluginHostControlResponse{}
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("Decode(shutdown) error = %v", err)
	}
	if !resp.OK || resp.RequestID != "shutdown-1" {
		t.Fatalf("shutdown response = %+v, want OK", resp)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("servePluginHostControl() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("servePluginHostControl() did not stop after shutdown")
	}
}

func TestPluginHostSupervisorLifecycleCommands(t *testing.T) {
	binaryPath := buildGatewayTestBinary(t)
	pluginPath := buildPluginHostLifecyclePlugin(t)
	supervisor := pluginmanager.PluginHostSupervisor{
		Executable:   binaryPath,
		RuntimeDir:   t.TempDir(),
		Protocol:     pluginmanager.PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	process, _, err := supervisor.Start(ctx, "hosted-plugin", "artifact-1")
	if err != nil {
		t.Fatalf("Start(plugin-host) error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = process.Stop(stopCtx)
	}()

	initPayload, err := json.Marshal(pluginmanager.PluginHostInitRequest{
		PluginID:     "hosted-plugin",
		ArtifactID:   "artifact-1",
		ArtifactPath: pluginPath,
		ConfigJSON:   `{"message":"hello"}`,
	})
	if err != nil {
		t.Fatalf("Marshal(init payload) error = %v", err)
	}
	resp, err := pluginmanager.SendPluginHostControlRequest(ctx, process.SocketPath, pluginmanager.PluginHostControlRequest{
		RequestID: "init-1",
		Command:   pluginmanager.PluginHostCommandInit,
		Protocol:  pluginmanager.PluginHostProtocol,
		Payload:   initPayload,
	})
	if err != nil {
		t.Fatalf("Send(init) error = %v", err)
	}
	if !resp.OK || resp.Lifecycle == nil || resp.Lifecycle.State != pluginmanager.RuntimeEnabled || !containsString(resp.Lifecycle.RegisteredHooks, pluginmanager.ExtensionRouteResolve) {
		t.Fatalf("init response = %+v, want enabled lifecycle with route hook", resp)
	}

	reloadPayload, err := json.Marshal(pluginmanager.PluginHostReloadConfigRequest{ConfigJSON: `{"message":"updated"}`})
	if err != nil {
		t.Fatalf("Marshal(reload payload) error = %v", err)
	}
	resp, err = pluginmanager.SendPluginHostControlRequest(ctx, process.SocketPath, pluginmanager.PluginHostControlRequest{
		RequestID: "reload-1",
		Command:   pluginmanager.PluginHostCommandReloadConfig,
		Protocol:  pluginmanager.PluginHostProtocol,
		Payload:   reloadPayload,
	})
	if err != nil {
		t.Fatalf("Send(reload) error = %v", err)
	}
	if !resp.OK || resp.Lifecycle == nil || resp.Lifecycle.State != pluginmanager.RuntimeEnabled {
		t.Fatalf("reload response = %+v, want enabled lifecycle", resp)
	}

	resp, err = pluginmanager.SendPluginHostControlRequest(ctx, process.SocketPath, pluginmanager.PluginHostControlRequest{
		RequestID: "drain-1",
		Command:   pluginmanager.PluginHostCommandDrain,
		Protocol:  pluginmanager.PluginHostProtocol,
	})
	if err != nil {
		t.Fatalf("Send(drain) error = %v", err)
	}
	if !resp.OK || resp.Lifecycle == nil || resp.Lifecycle.State != pluginmanager.RuntimeDraining {
		t.Fatalf("drain response = %+v, want draining lifecycle", resp)
	}

	resp, err = pluginmanager.SendPluginHostControlRequest(ctx, process.SocketPath, pluginmanager.PluginHostControlRequest{
		RequestID: "destroy-1",
		Command:   pluginmanager.PluginHostCommandDestroy,
		Protocol:  pluginmanager.PluginHostProtocol,
	})
	if err != nil {
		t.Fatalf("Send(destroy) error = %v", err)
	}
	if !resp.OK || resp.Lifecycle == nil || resp.Lifecycle.State != pluginmanager.RuntimeDisabled {
		t.Fatalf("destroy response = %+v, want disabled lifecycle", resp)
	}
}

func TestPluginHostHandshakeSubprocess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "plugin-host", "handshake", "--protocol", pluginmanager.PluginHostProtocol)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run plugin-host handshake error = %v\n%s", err, output)
	}
	var handshake pluginmanager.PluginHostHandshake
	if err := json.Unmarshal(output, &handshake); err != nil {
		t.Fatalf("Unmarshal(subprocess handshake) error = %v\n%s", err, output)
	}
	if handshake.Protocol != pluginmanager.PluginHostProtocol || handshake.PID <= 0 || handshake.DataPlane {
		t.Fatalf("subprocess handshake = %+v, want protocol response without data plane", handshake)
	}
}

func TestPluginHostSupervisorStartsAndStopsHost(t *testing.T) {
	binaryPath := buildGatewayTestBinary(t)
	supervisor := pluginmanager.PluginHostSupervisor{
		Executable:   binaryPath,
		RuntimeDir:   t.TempDir(),
		Protocol:     pluginmanager.PluginHostProtocol,
		StartTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, handshake, err := supervisor.Start(ctx, "plugin-a", "artifact-a")
	if err != nil {
		t.Fatalf("Start(plugin-host) error = %v", err)
	}
	if handshake.Protocol != pluginmanager.PluginHostProtocol || handshake.PID <= 0 || handshake.DataPlane {
		t.Fatalf("supervisor handshake = %+v, want protocol response without data plane", handshake)
	}
	running := process.Summary()
	if running.PluginID != "plugin-a" || running.ArtifactID != "artifact-a" || running.PID <= 0 || running.State != pluginmanager.RuntimeEnabled {
		t.Fatalf("running summary = %+v, want enabled supervised host", running)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop(plugin-host) error = %v", err)
	}
	stopped := process.Summary()
	if stopped.State != pluginmanager.RuntimeDisabled || stopped.CrashCount != 0 || stopped.ExitedAt == 0 {
		t.Fatalf("stopped summary = %+v, want clean disabled host", stopped)
	}
}

func waitForPluginHostSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("plugin-host socket %s was not ready", socketPath)
}

func buildGatewayTestBinary(t *testing.T) string {
	t.Helper()
	binaryPath := filepath.Join(t.TempDir(), "gateway-test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build gateway test binary error = %v\n%s", err, output)
	}
	return binaryPath
}

func buildPluginHostLifecyclePlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	goMod := "module example.com/plugin-host-lifecycle\n\n" +
		"go 1.24.0\n\n" +
		"require github.com/tursom/mc-gateway v0.0.0\n\n" +
		"replace github.com/tursom/mc-gateway => " + filepath.ToSlash(repoRoot) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}
	source := `package main

import "github.com/tursom/mc-gateway/plugin/api"

type config struct {
	Message string ` + "`json:\"message\"`" + `
}

type pluginImpl struct {
	api.AbstractPlugin
	config config
}

func Plugin() api.Plugin { return &pluginImpl{} }

func (p *pluginImpl) NewConfigObj() any { return &config{} }

func (p *pluginImpl) ReloadConfig(value any) error {
	if cfg, ok := value.(*config); ok {
		p.config = *cfg
	}
	return nil
}

func (p *pluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookRouteResolve,
		func(api.RouteResolveRequest) bool { return true },
		func(api.RouteResolveRequest) (api.RouteDecision, error) {
			return api.RouteDecision{Action: api.RouteDecisionPass}, nil
		},
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tidy := exec.CommandContext(ctx, "go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = append(os.Environ(), "GOWORK=off")
	if output, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy lifecycle plugin error = %v\n%s", err, output)
	}
	pluginPath := filepath.Join(dir, "plugin.so")
	cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=plugin", "-o", pluginPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build lifecycle plugin error = %v\n%s", err, output)
	}
	return pluginPath
}
