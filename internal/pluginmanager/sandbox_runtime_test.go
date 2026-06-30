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

	"github.com/tursom/mc-gateway/plugin/api"
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

func TestSandboxManagerDispatchInvokesFakeControlPeer(t *testing.T) {
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionConfigValidate, HandlerID: "config-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionRuleEvaluate, HandlerID: "rule-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionStatusPing, HandlerID: "status-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionProvider, HandlerID: "provider-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionEventSubscriber, HandlerID: "event-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	peer := &fakeSandboxControlPeer{}
	manager := newSandboxDispatchManagerForTest(t, peer, registrations)
	artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-dispatch", registrations)

	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{"version":"1"}`, 10); err != nil {
		t.Fatalf("SetDesired(disabled) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	route, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
	if err != nil {
		t.Fatalf("ResolveRoute() error = %v", err)
	}
	if route.Decision.Action != api.RouteDecisionOverride || route.Decision.Upstream != "sandbox-upstream:25565" {
		t.Fatalf("ResolveRoute() = %+v, want sandbox override", route)
	}

	rule, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{Subject: "player", Action: "join", Resource: "server"})
	if err != nil {
		t.Fatalf("EvaluateRule() error = %v", err)
	}
	if !rule.Handled || !rule.Decision.Allow || rule.Decision.ProviderID != artifact.PluginID {
		t.Fatalf("EvaluateRule() = %+v, want sandbox allow", rule)
	}

	status, err := manager.StatusPing(context.Background(), api.StatusPingRequest{Host: "play.example"})
	if err != nil {
		t.Fatalf("StatusPing() error = %v", err)
	}
	if !status.Handled || status.Response.MOTD != "sandbox play.example" {
		t.Fatalf("StatusPing() = %+v, want sandbox response", status)
	}

	plan := manager.DispatchPlan(context.Background())
	if len(plan.Providers) != 1 || plan.Providers[0].Name != "sandbox-provider" || plan.Providers[0].Status != "ready" {
		t.Fatalf("DispatchPlan providers = %+v, want queried sandbox provider", plan.Providers)
	}
	state := manager.extensionState()
	if len(state.subscribers) != 1 {
		t.Fatalf("subscribers = %d, want one sandbox subscriber", len(state.subscribers))
	}
	manager.operations.deliverSubscriberEvent(state.subscribers[0], queuedEvent{pluginID: "producer", name: "test.event", fields: map[string]string{"kind": "smoke"}})

	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{"version":"2"}`, 10); err != nil {
		t.Fatalf("SetDesired(enabled reload) error = %v", err)
	}
	if peer.commandCount(sandboxControlCommandReloadConfig) == 0 {
		t.Fatalf("reload_config command count = 0, want hot reload through sandbox RPC")
	}
	for _, point := range []string{ExtensionConfigValidate, ExtensionRouteResolve, ExtensionRuleEvaluate, ExtensionStatusPing, ExtensionProvider, ExtensionEventSubscriber} {
		if peer.invokeCount(point) == 0 {
			t.Fatalf("sandbox invoke count for %s = 0, want dispatch through fake control peer", point)
		}
	}

	beforeDisable := peer.invokeCount(ExtensionRouteResolve)
	if _, err := manager.Disable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	route, err = manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
	if err != nil {
		t.Fatalf("ResolveRoute(after disable) error = %v", err)
	}
	if route.Decision.Action != api.RouteDecisionFallback || route.Decision.Upstream != "sqlite:25565" {
		t.Fatalf("ResolveRoute(after disable) = %+v, want sqlite fallback", route)
	}
	if got := peer.invokeCount(ExtensionRouteResolve); got != beforeDisable {
		t.Fatalf("route invoke count after disable = %d, want unchanged %d", got, beforeDisable)
	}
}

func TestSandboxManagerFailPolicyForTimeoutBadResponseAndExit(t *testing.T) {
	t.Run("route timeout fail open falls back", func(t *testing.T) {
		peer := &fakeSandboxControlPeer{mode: "timeout"}
		registrations := []SandboxHandlerRegistration{{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 5, SchemaVersion: 1}}
		manager := newSandboxDispatchManagerForTest(t, peer, registrations)
		artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-timeout-open", registrations)
		enableSandboxDispatchArtifactForTest(t, manager, artifact)

		route, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
		if err != nil {
			t.Fatalf("ResolveRoute() error = %v", err)
		}
		if route.Decision.Action != api.RouteDecisionFallback || route.Decision.Upstream != "sqlite:25565" {
			t.Fatalf("ResolveRoute(timeout fail_open) = %+v, want fallback", route)
		}
	})

	t.Run("route bad response fail closed rejects", func(t *testing.T) {
		peer := &fakeSandboxControlPeer{mode: "bad_response"}
		registrations := []SandboxHandlerRegistration{{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1}}
		manager := newSandboxDispatchManagerForTest(t, peer, registrations)
		artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-bad-closed", registrations)
		enableSandboxDispatchArtifactForTest(t, manager, artifact)

		route, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "play.example", FallbackUpstream: "sqlite:25565", FallbackHit: true}, nil)
		if err != nil {
			t.Fatalf("ResolveRoute() error = %v", err)
		}
		if route.Decision.Action != api.RouteDecisionReject || !strings.Contains(route.Decision.Reason, sandboxControlErrorBadResponse) {
			t.Fatalf("ResolveRoute(bad response fail_closed) = %+v, want reject with stable bad_response", route)
		}
	})

	t.Run("rule process exit fail closed denies", func(t *testing.T) {
		peer := &fakeSandboxControlPeer{}
		registrations := []SandboxHandlerRegistration{{ExtensionPoint: ExtensionRuleEvaluate, HandlerID: "rule-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1}}
		manager := newSandboxDispatchManagerForTest(t, peer, registrations)
		artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-exit-closed", registrations)
		enableSandboxDispatchArtifactForTest(t, manager, artifact)
		closeLoadedSandboxProcessForTest(t, manager, artifact.PluginID)

		rule, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{Subject: "player", Action: "join"})
		if err != nil {
			t.Fatalf("EvaluateRule() error = %v", err)
		}
		if !rule.Handled || !rule.Decision.Deny || !strings.Contains(rule.Decision.Reason, sandboxControlErrorProcessExited) {
			t.Fatalf("EvaluateRule(process exit fail_closed) = %+v, want deny with stable process_exited", rule)
		}
	})
}

func TestSandboxManagerConfigValidateBlocksDryRun(t *testing.T) {
	registrations := []SandboxHandlerRegistration{{ExtensionPoint: ExtensionConfigValidate, HandlerID: "config-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1}}
	peer := &fakeSandboxControlPeer{}
	manager := newSandboxDispatchManagerForTest(t, peer, registrations)
	artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-config-block", registrations)

	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{"reject":true}`, 10); err == nil || !strings.Contains(err.Error(), "sandbox config validation failed") {
		t.Fatalf("SetDesired(invalid config) error = %v, want sandbox config validation failure", err)
	}
}

type fakeSandboxControlPeer struct {
	mu       sync.Mutex
	mode     string
	calls    []SandboxInvokeRequest
	commands []string
}

func (p *fakeSandboxControlPeer) invoke(ctx context.Context, _ string, req SandboxControlRequest) (SandboxControlResponse, error) {
	p.mu.Lock()
	p.commands = append(p.commands, req.Command)
	p.mu.Unlock()
	if p.mode == "timeout" && req.Command == sandboxControlCommandInvoke {
		<-ctx.Done()
		return SandboxControlResponse{}, ctx.Err()
	}
	resp := SandboxControlResponse{
		RequestID:         req.RequestID,
		Command:           req.Command,
		Protocol:          sandboxProcessProtocol,
		PluginID:          req.PluginID,
		ArtifactID:        req.ArtifactID,
		RuntimeInstanceID: req.RuntimeInstanceID,
		Generation:        req.Generation,
		TraceID:           req.TraceID,
		DeadlineUnixMS:    req.DeadlineUnixMS,
		OK:                true,
	}
	if req.Command == sandboxControlCommandReloadConfig {
		return resp, nil
	}
	if req.Command != sandboxControlCommandInvoke {
		return setSandboxControlError(resp, sandboxControlErrorUnknownCommand, "unexpected fake peer command"), nil
	}
	var payload SandboxInvokeRequest
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return SandboxControlResponse{}, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, payload)
	p.mu.Unlock()
	invoke := SandboxInvokeResponse{ExtensionPoint: payload.ExtensionPoint, HandlerID: payload.HandlerID, OK: true}
	if p.mode == "bad_response" {
		resp.Invoke = &invoke
		return resp, nil
	}
	switch payload.ExtensionPoint {
	case ExtensionConfigValidate:
		valid := true
		if strings.Contains(string(payload.ConfigJSON), `"reject":true`) {
			valid = false
			invoke.Reason = "config rejected by fake sandbox"
		}
		invoke.Valid = &valid
	case ExtensionRouteResolve, ExtensionRouteResolver:
		host := ""
		if payload.RouteResolve != nil {
			host = payload.RouteResolve.Host
		}
		invoke.RouteDecision = &api.RouteDecision{
			Action:     api.RouteDecisionOverride,
			Host:       host,
			Upstream:   "sandbox-upstream:25565",
			ProviderID: req.PluginID,
			Reason:     "fake sandbox route",
		}
	case ExtensionRuleEvaluate:
		invoke.RuleDecision = &api.RuleEvaluateDecision{Allow: true, ProviderID: req.PluginID, Reason: "fake sandbox allow"}
	case ExtensionStatusPing:
		host := ""
		if payload.StatusPing != nil {
			host = payload.StatusPing.Host
		}
		invoke.StatusResponse = &api.StatusPingResponse{MOTD: "sandbox " + host, VersionText: "sandbox"}
	case ExtensionProvider:
		invoke.Provider = &api.ProviderRegistration{Type: ExtensionProvider, Name: "sandbox-provider", Priority: 7}
	case ExtensionEventSubscriber:
		invoke.EventResult = &api.EventDeliveryResult{OK: true, Reason: "delivered"}
	default:
		return setSandboxControlError(resp, sandboxControlErrorUnknownCommand, "unexpected extension point"), nil
	}
	resp.Invoke = &invoke
	return resp, nil
}

func (p *fakeSandboxControlPeer) invokeCount(point string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, call := range p.calls {
		if call.ExtensionPoint == point {
			count++
		}
	}
	return count
}

func (p *fakeSandboxControlPeer) commandCount(command string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, got := range p.commands {
		if got == command {
			count++
		}
	}
	return count
}

func newSandboxDispatchManagerForTest(t *testing.T, peer *fakeSandboxControlPeer, registrations []SandboxHandlerRegistration) *Manager {
	t.Helper()
	policy := SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024}
	manager := New(Options{
		DB:                 openPluginManagerTestDB(t),
		ArtifactRoot:       t.TempDir(),
		FutureRuntimeGates: FutureRuntimeGates{SandboxProcess: true},
		SandboxSelfCheck:   func(SandboxPolicy) error { return nil },
		SandboxPolicy:      policy,
	})
	manager.adapter = SandboxProcessAdapter{
		Policy: policy,
		startProcess: func(_ context.Context, _ SandboxSupervisor, pluginID, artifactID, _ string, generation int64, configJSON string, _ SandboxSecretResolver) (*SandboxProcess, error) {
			process := newSandboxControlProcessForTest(pluginID, artifactID, generation, configJSON)
			process.SocketPath = "fake-sandbox-control.sock"
			process.Registrations = append([]SandboxHandlerRegistration(nil), registrations...)
			process.registerOK = true
			process.controlInvoker = peer.invoke
			return process, nil
		},
	}
	manager.serviceMode = PluginServiceModeSandboxProcess
	return manager
}

func uploadSandboxDispatchArtifactForTest(t *testing.T, manager *Manager, pluginID string, registrations []SandboxHandlerRegistration) ArtifactRecord {
	t.Helper()
	points := make([]ExtensionPoint, 0, len(registrations))
	capPoints := make([]string, 0, len(registrations))
	for _, reg := range registrations {
		points = append(points, ExtensionPoint{Type: "hook", Key: reg.ExtensionPoint})
		capPoints = append(capPoints, reg.ExtensionPoint)
	}
	capabilities, err := json.Marshal(map[string]any{"extension_points": capPoints})
	if err != nil {
		t.Fatalf("Marshal capabilities error = %v", err)
	}
	return uploadTestArtifactWithManifest(t, manager, pluginID, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ExtensionPoints = points
		manifest.Capabilities = capabilities
		manifest.RuntimeLimits.HandlerTimeoutMS = 1000
	})
}

func enableSandboxDispatchArtifactForTest(t *testing.T, manager *Manager, artifact ArtifactRecord) {
	t.Helper()
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
}

func closeLoadedSandboxProcessForTest(t *testing.T, manager *Manager, pluginID string) {
	t.Helper()
	loaded := manager.loaded[pluginID]
	if loaded == nil {
		t.Fatalf("loaded plugin %q missing", pluginID)
	}
	closeSandboxProcessForTest(sandboxHostedPluginFromInstance(loaded.instance).process)
}

func closeSandboxProcessForTest(process *SandboxProcess) {
	if process == nil || process.done == nil {
		return
	}
	defer func() { _ = recover() }()
	close(process.done)
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
