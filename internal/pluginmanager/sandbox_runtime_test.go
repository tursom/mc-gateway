package pluginmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSandboxControlRPCValidationErrors(t *testing.T) {
	ctx := context.Background()

	unknownProcess := newSandboxControlProcessForTest("sandbox-rpc", "artifact-rpc", 3, "")
	unknown := unknownProcess.HandleControlRequest(ctx, sandboxControlRequestForTest(t, unknownProcess, "bogus", nil), nil)
	if unknown.OK || unknown.ErrorCode != sandboxControlErrorUnknownCommand {
		t.Fatalf("unknown command response = %+v, want %s", unknown, sandboxControlErrorUnknownCommand)
	}

	protocolProcess := newSandboxControlProcessForTest("sandbox-rpc", "artifact-rpc", 3, "")
	protocolReq := sandboxControlRequestForTest(t, protocolProcess, sandboxControlCommandHandshake, SandboxHandshakeRequest{ABIVersion: SandboxProcessABIVersionV1})
	protocolReq.Protocol = "mc-gateway-sandbox-process/v0"
	protocol := protocolProcess.HandleControlRequest(ctx, protocolReq, nil)
	if protocol.OK || protocol.ErrorCode != sandboxControlErrorProtocolMismatch || !strings.Contains(protocol.Error, "unsupported sandbox-process protocol") {
		t.Fatalf("protocol mismatch response = %+v, want diagnosable %s", protocol, sandboxControlErrorProtocolMismatch)
	}
	if err := protocolProcess.waitForStartup(shortSandboxControlContext(t)); err == nil || !strings.Contains(err.Error(), sandboxControlErrorProtocolMismatch) {
		t.Fatalf("waitForStartup(protocol mismatch) error = %v, want %s", err, sandboxControlErrorProtocolMismatch)
	}

	abiProcess := newSandboxControlProcessForTest("sandbox-rpc", "artifact-rpc", 3, "")
	abi := abiProcess.HandleControlRequest(ctx, sandboxControlRequestForTest(t, abiProcess, sandboxControlCommandHandshake, SandboxHandshakeRequest{ABIVersion: "mc-gateway.sandbox-process.abi/v0"}), nil)
	if abi.OK || abi.ErrorCode != sandboxControlErrorABIMismatch || !strings.Contains(abi.Error, "unsupported sandbox abi_version") {
		t.Fatalf("abi mismatch response = %+v, want diagnosable %s", abi, sandboxControlErrorABIMismatch)
	}
	if err := abiProcess.waitForStartup(shortSandboxControlContext(t)); err == nil || !strings.Contains(err.Error(), sandboxControlErrorABIMismatch) {
		t.Fatalf("waitForStartup(abi mismatch) error = %v, want %s", err, sandboxControlErrorABIMismatch)
	}

	oversizedProcess := newSandboxControlProcessForTest("sandbox-rpc", "artifact-rpc", 3, "")
	oversizedReq := sandboxControlRequestForTest(t, oversizedProcess, sandboxControlCommandMetrics, nil)
	oversizedReq.Payload = json.RawMessage(strings.Repeat("x", sandboxControlMaxPayloadBytes+1))
	oversized := oversizedProcess.HandleControlRequest(ctx, oversizedReq, nil)
	if oversized.OK || oversized.ErrorCode != sandboxControlErrorOversizedPayload {
		t.Fatalf("oversized response = %+v, want %s", oversized, sandboxControlErrorOversizedPayload)
	}

	server, client := net.Pipe()
	defer client.Close()
	malformedProcess := newSandboxControlProcessForTest("sandbox-rpc", "artifact-rpc", 3, "")
	go malformedProcess.handleControlConn(ctx, server, nil)
	if _, err := client.Write([]byte("{bad-json\n")); err != nil {
		t.Fatalf("write malformed json error = %v", err)
	}
	var malformed SandboxControlResponse
	if err := json.NewDecoder(client).Decode(&malformed); err != nil {
		t.Fatalf("decode malformed json response error = %v", err)
	}
	if malformed.OK || malformed.ErrorCode != sandboxControlErrorBadJSON {
		t.Fatalf("malformed json response = %+v, want %s", malformed, sandboxControlErrorBadJSON)
	}
}

func TestSandboxControlStartupSequenceRegisters(t *testing.T) {
	process := newSandboxControlProcessForTest("sandbox-startup", "artifact-startup", 9, `{"safe":true}`)
	handshake := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandHandshake, SandboxHandshakeRequest{ABIVersion: SandboxProcessABIVersionV1}), nil)
	if !handshake.OK {
		t.Fatalf("handshake response = %+v, want ok", handshake)
	}
	init := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandInit, SandboxInitRequest{}), nil)
	if !init.OK || init.Init == nil || init.Init.ConfigJSON != `{"safe":true}` {
		t.Fatalf("init response = %+v, want config-bearing init", init)
	}
	register := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandRegister, SandboxRegisterRequest{
		ExtensionPoint:       ExtensionRouteResolve,
		HandlerID:            "route-main",
		FailPolicy:           ExternalFailPolicyClosed,
		TimeoutMS:            1500,
		SchemaVersion:        1,
		DeclaredCapabilities: []string{"route.resolve"},
	}), nil)
	if !register.OK || register.Register == nil || len(register.Register.Registrations) != 1 {
		t.Fatalf("register response = %+v, want accepted registration", register)
	}
	if err := process.waitForStartup(shortSandboxControlContext(t)); err != nil {
		t.Fatalf("waitForStartup() error = %v", err)
	}
	if len(process.Registrations) != 1 {
		t.Fatalf("registrations = %+v, want one accepted registration", process.Registrations)
	}
	reg := process.Registrations[0]
	if reg.ExtensionPoint != ExtensionRouteResolve || reg.HandlerID != "route-main" ||
		reg.FailPolicy != ExternalFailPolicyClosed || reg.TimeoutMS != 1500 ||
		reg.SchemaVersion != 1 || len(reg.DeclaredCapabilities) != 1 {
		t.Fatalf("registration = %+v, want normalized register response fields", reg)
	}
	if diag := process.Diagnostics(); diag.State != RuntimeEnabled || diag.UnsupportedReason != "" {
		t.Fatalf("Diagnostics() = %+v, want enabled registered process", diag)
	}
}

func TestSandboxControlRegisterFailureDoesNotPolluteDispatchTable(t *testing.T) {
	process := newSandboxControlProcessForTest("sandbox-register-fail", "artifact-register-fail", 4, "")
	gateway := NewGateway(process.PluginID, nil, nil, nil)

	handshake := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandHandshake, SandboxHandshakeRequest{ABIVersion: SandboxProcessABIVersionV1}), nil)
	if !handshake.OK {
		t.Fatalf("handshake response = %+v, want ok", handshake)
	}
	init := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandInit, SandboxInitRequest{}), nil)
	if !init.OK {
		t.Fatalf("init response = %+v, want ok", init)
	}
	register := process.HandleControlRequest(context.Background(), sandboxControlRequestForTest(t, process, sandboxControlCommandRegister, SandboxRegisterRequest{
		ExtensionPoint: "unknown.extension/v1",
		HandlerID:      "bad",
		SchemaVersion:  1,
	}), nil)
	if register.OK || register.ErrorCode != sandboxControlErrorRegisterFailed {
		t.Fatalf("register response = %+v, want %s", register, sandboxControlErrorRegisterFailed)
	}
	if err := process.waitForStartup(shortSandboxControlContext(t)); err == nil || !strings.Contains(err.Error(), sandboxControlErrorRegisterFailed) {
		t.Fatalf("waitForStartup(register failure) error = %v, want %s", err, sandboxControlErrorRegisterFailed)
	}
	if hooks := gateway.RegisteredHooks(); len(hooks) != 0 {
		t.Fatalf("registered hooks after failed sandbox register = %+v, want empty dispatch table", hooks)
	}
}

func TestSandboxProcessAdapterStartRequiresControlStartup(t *testing.T) {
	prepared := RuntimePrepared{
		PluginID:   "sandbox-adapter",
		ArtifactID: "artifact-adapter",
		Runtime:    RuntimeSandbox,
		Mode:       PluginServiceModeSandboxProcess,
		PreparedAt: time.Now().Unix(),
	}
	artifact := ArtifactRecord{ID: prepared.ArtifactID, RuntimeType: RuntimeSandbox, ArtifactType: ArtifactTypeBinary, FilePath: "/unused"}
	pluginRecord := PluginRecord{ID: prepared.PluginID, DesiredGeneration: 7, ConfigJSON: `{"adapter":true}`}

	tests := []struct {
		name      string
		peer      string
		wantError string
	}{
		{name: "missing handshake", peer: "none", wantError: sandboxControlErrorTimeout},
		{name: "failed register", peer: "bad_register", wantError: sandboxControlErrorRegisterFailed},
		{name: "compliant peer", peer: "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := SandboxProcessAdapter{
				Policy: SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024},
				startProcess: func(ctx context.Context, supervisor SandboxSupervisor, pluginID, artifactID, executable string, generation int64, configJSON string, resolver SandboxSecretResolver) (*SandboxProcess, error) {
					process, cleanup, err := startSandboxControlProcessForTest(t, ctx, pluginID, artifactID, generation, configJSON)
					if err != nil {
						return nil, err
					}
					var peerDone chan error
					switch tt.peer {
					case "ok":
						peerDone = make(chan error, 1)
						go func() {
							peerDone <- runSandboxControlPeerForTest(ctx, process.SocketPath, process, SandboxProcessABIVersionV1, SandboxRegisterRequest{
								ExtensionPoint: ExtensionRouteResolve,
								HandlerID:      "route-main",
								FailPolicy:     ExternalFailPolicyClosed,
								TimeoutMS:      1000,
								SchemaVersion:  1,
							})
						}()
					case "bad_register":
						peerDone = make(chan error, 1)
						go func() {
							peerDone <- runSandboxControlPeerForTest(ctx, process.SocketPath, process, SandboxProcessABIVersionV1, SandboxRegisterRequest{
								ExtensionPoint: "bad.extension/v1",
								HandlerID:      "bad",
								SchemaVersion:  1,
							})
						}()
					}
					err = process.waitForStartup(ctx)
					if peerDone != nil {
						peerErr := <-peerDone
						if tt.wantError == "" && peerErr != nil {
							cleanup()
							return nil, peerErr
						}
					}
					if err != nil {
						cleanup()
						return nil, err
					}
					t.Cleanup(cleanup)
					return process, nil
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			instance, err := adapter.Start(ctx, prepared, artifact, pluginRecord, NewGateway(prepared.PluginID, nil, nil, nil))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Start() error = %v, want %s", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			hosted, ok := instance.Plugin.(sandboxHostedPlugin)
			if !ok || hosted.process == nil || len(hosted.process.Registrations) != 1 {
				t.Fatalf("Start() instance = %+v plugin=%T, want registered sandboxHostedPlugin", instance, instance.Plugin)
			}
		})
	}
}

func newSandboxControlProcessForTest(pluginID, artifactID string, generation int64, configJSON string) *SandboxProcess {
	return &SandboxProcess{
		PluginID:          pluginID,
		ArtifactID:        artifactID,
		RuntimeInstanceID: "runtime-" + pluginID,
		Generation:        generation,
		Protocol:          sandboxProcessProtocol,
		StartedAt:         time.Now().Unix(),
		Policy:            normalizeSandboxPolicy(SandboxPolicy{}),
		SocketPath:        filepath.Join("/tmp", "unused.sock"),
		ConfigJSON:        configJSON,
		done:              make(chan struct{}),
		startupDone:       make(chan struct{}),
	}
}

func startSandboxControlProcessForTest(t *testing.T, ctx context.Context, pluginID, artifactID string, generation int64, configJSON string) (*SandboxProcess, func(), error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mcgw-sb-*")
	if err != nil {
		return nil, nil, err
	}
	socketPath := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	process := newSandboxControlProcessForTest(pluginID, artifactID, generation, configJSON)
	process.SocketPath = socketPath
	var closeDone sync.Once
	cleanup := func() {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
		closeDone.Do(func() { close(process.done) })
	}
	if err := process.startControlRPC(ctx, listener, nil); err != nil {
		cleanup()
		return nil, nil, err
	}
	return process, cleanup, nil
}

func runSandboxControlPeerForTest(ctx context.Context, socketPath string, process *SandboxProcess, abiVersion string, register SandboxRegisterRequest) error {
	steps := []struct {
		command string
		payload any
	}{
		{sandboxControlCommandHandshake, SandboxHandshakeRequest{ABIVersion: abiVersion}},
		{sandboxControlCommandInit, SandboxInitRequest{}},
		{sandboxControlCommandRegister, register},
	}
	for _, step := range steps {
		resp, err := SendSandboxControlRequest(ctx, socketPath, sandboxControlRequestForTest(nil, process, step.command, step.payload))
		if err != nil {
			return err
		}
		if !resp.OK {
			if step.command == sandboxControlCommandRegister && register.ExtensionPoint == "bad.extension/v1" {
				return nil
			}
			return fmt.Errorf("%s failed: %s %s", step.command, resp.ErrorCode, resp.Error)
		}
	}
	return nil
}

func sandboxControlRequestForTest(t *testing.T, process *SandboxProcess, command string, payload any) SandboxControlRequest {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			if t != nil {
				t.Fatalf("Marshal(%s payload) error = %v", command, err)
			}
			panic(err)
		}
		raw = data
	}
	return SandboxControlRequest{
		Command:           command,
		Protocol:          sandboxProcessProtocol,
		PluginID:          process.PluginID,
		ArtifactID:        process.ArtifactID,
		RuntimeInstanceID: process.RuntimeInstanceID,
		Generation:        process.Generation,
		TraceID:           "trace-" + command,
		DeadlineUnixMS:    time.Now().Add(time.Second).UnixMilli(),
		Payload:           raw,
	}
}

func shortSandboxControlContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}
