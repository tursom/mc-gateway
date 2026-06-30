package pluginmanager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
				Policy:    SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024, ExternalIsolation: true},
				SelfCheck: func(SandboxPolicy) error { return nil },
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

func TestSandboxRestartRecoveryRestoresEnabledPluginsByPriority(t *testing.T) {
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	first := newSandboxDispatchManagerWithDBForTest(t, db, root, &fakeSandboxControlPeer{}, registrations)
	failedArtifact := uploadSandboxDispatchArtifactForTest(t, first, "sandbox-recover-failed", registrations)
	firstArtifact := uploadSandboxDispatchArtifactForTest(t, first, "sandbox-recover-a", registrations)
	secondArtifact := uploadSandboxDispatchArtifactForTest(t, first, "sandbox-recover-b", registrations)
	ctx := context.Background()
	if _, err := first.SetDesired(ctx, "admin", failedArtifact.PluginID, failedArtifact.ID, DesiredEnabled, `{}`, 5); err != nil {
		t.Fatalf("SetDesired(failed) error = %v", err)
	}
	if _, err := first.SetDesired(ctx, "admin", secondArtifact.PluginID, secondArtifact.ID, DesiredEnabled, `{}`, 20); err != nil {
		t.Fatalf("SetDesired(second) error = %v", err)
	}
	if _, err := first.SetDesired(ctx, "admin", firstArtifact.PluginID, firstArtifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(first) error = %v", err)
	}

	recoveryPeer := &fakeSandboxControlPeer{startErrs: map[string]error{failedArtifact.PluginID: errors.New("fake sandbox start failed")}}
	second := newSandboxDispatchManagerWithDBForTest(t, db, root, recoveryPeer, registrations)
	if err := second.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	gotOrder := strings.Join(recoveryPeer.startOrder(), ",")
	wantOrder := strings.Join([]string{failedArtifact.PluginID, firstArtifact.PluginID, secondArtifact.PluginID}, ",")
	if gotOrder != wantOrder {
		t.Fatalf("sandbox recovery start order = %s, want %s", gotOrder, wantOrder)
	}
	failed, err := second.Plugin(ctx, failedArtifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin(failed) error = %v", err)
	}
	if failed.RuntimeState != RuntimeFailed || !strings.Contains(failed.LastError, "fake sandbox start failed") {
		t.Fatalf("failed plugin state = %+v, want failed start recorded", failed)
	}
	for _, artifact := range []ArtifactRecord{firstArtifact, secondArtifact} {
		plugin, err := second.Plugin(ctx, artifact.PluginID)
		if err != nil {
			t.Fatalf("Plugin(%s) error = %v", artifact.PluginID, err)
		}
		if plugin.RuntimeState != RuntimeEnabled || plugin.ActiveArtifactID != artifact.ID {
			t.Fatalf("recovered plugin = %+v, want enabled artifact %s", plugin, artifact.ID)
		}
	}
}

func TestSandboxOldGenerationRuntimeUpdateCannotOverwriteCurrentStatus(t *testing.T) {
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	manager := newSandboxDispatchManagerForTest(t, &fakeSandboxControlPeer{}, registrations)
	artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-generation-fence", registrations)
	ctx := context.Background()
	first, err := manager.SetDesired(ctx, "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{"version":1}`, 10)
	if err != nil {
		t.Fatalf("SetDesired(first) error = %v", err)
	}
	second, err := manager.SetDesired(ctx, "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{"version":2}`, 10)
	if err != nil {
		t.Fatalf("SetDesired(second) error = %v", err)
	}
	updated, err := manager.repo.MarkRuntimeIfDesiredGeneration(ctx, artifact.PluginID, first.DesiredGeneration, RuntimeFailed, artifact.ID, artifact.ID, first.DesiredGeneration, "old generation failed late", map[string]any{"old_generation": true}, nil)
	if err != nil {
		t.Fatalf("MarkRuntimeIfDesiredGeneration(old) error = %v", err)
	}
	if updated {
		t.Fatal("old generation runtime update unexpectedly matched current desired generation")
	}
	plugin, err := manager.Plugin(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin() error = %v", err)
	}
	if plugin.DesiredGeneration != second.DesiredGeneration || plugin.RuntimeState == RuntimeFailed || plugin.LastError != "" {
		t.Fatalf("plugin after old generation callback = %+v, want generation %d without failed overwrite", plugin, second.DesiredGeneration)
	}
}

func TestSandboxReloadFailureKeepsCurrentRuntimeConfig(t *testing.T) {
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionConfigValidate, HandlerID: "config-main", FailPolicy: api.FailPolicyClose, TimeoutMS: 1000, SchemaVersion: 1},
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	peer := &fakeSandboxControlPeer{}
	manager := newSandboxDispatchManagerForTest(t, peer, registrations)
	artifact := uploadSandboxDispatchArtifactForTest(t, manager, "sandbox-reload-failure", registrations)
	ctx := context.Background()
	if _, err := manager.SetDesired(ctx, "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{"version":"active"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(ctx, "admin", artifact.PluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	before, err := manager.Plugin(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin(before) error = %v", err)
	}
	summary := jsonMapFromJSONString(before.RuntimeSummaryJSON)
	if metadataString(summary["runtime_plugin_id"]) != artifact.PluginID ||
		metadataString(summary["runtime_artifact_id"]) != artifact.ID ||
		metadataString(summary["runtime_instance_id"]) == "" ||
		metadataString(summary["config_hash"]) != stableHashJSONRaw(`{"version":"active"}`) ||
		metadataString(summary["runtime_limits_hash"]) == "" ||
		metadataString(summary["capability_hash"]) == "" {
		t.Fatalf("runtime summary = %+v, want sandbox instance binding hashes", summary)
	}
	loaded := manager.loaded[artifact.PluginID]
	if loaded == nil {
		t.Fatal("loaded sandbox plugin missing")
	}
	process := sandboxHostedPluginFromInstance(loaded.instance).process
	if process == nil {
		t.Fatal("loaded sandbox process missing")
	}
	oldConfig := process.ConfigJSON
	peer.failNextReload()
	next := before
	next.ConfigJSON = `{"version":"rejected-live"}`
	if err := manager.reloadLoadedRuntime(ctx, "admin", next, "test_reload_failure"); err == nil || !strings.Contains(err.Error(), "reload rejected") {
		t.Fatalf("reloadLoadedRuntime() error = %v, want reload rejection", err)
	}
	if process.ConfigJSON != oldConfig {
		t.Fatalf("sandbox process config = %s, want unchanged %s after reload failure", process.ConfigJSON, oldConfig)
	}
	after, err := manager.Plugin(ctx, artifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin(after) error = %v", err)
	}
	if after.ActiveArtifactID != before.ActiveArtifactID || after.AppliedGeneration != before.AppliedGeneration || after.RuntimeState != before.RuntimeState {
		t.Fatalf("plugin after reload failure = %+v, want active runtime unchanged from %+v", after, before)
	}
}

func TestSandboxRollbackRerunsCurrentCapabilityGate(t *testing.T) {
	registrations := []SandboxHandlerRegistration{
		{ExtensionPoint: ExtensionRouteResolve, HandlerID: "route-main", FailPolicy: api.FailPolicyOpen, TimeoutMS: 1000, SchemaVersion: 1},
	}
	manager := newSandboxDispatchManagerForTest(t, &fakeSandboxControlPeer{}, registrations)
	pluginID := "sandbox-rollback-capability"
	oldArtifact := uploadTestArtifactWithManifestBytes(t, manager, pluginID, []byte("old sandbox rollback bytes"), func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionRouteResolve}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["route.resolve/v1"],"runtime":{"required_capabilities":["network.egress"]}}`)
		manifest.RuntimeLimits.HandlerTimeoutMS = 1000
	})
	newArtifact := uploadTestArtifactWithManifestBytes(t, manager, pluginID, []byte("new sandbox rollback bytes"), func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionRouteResolve}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["route.resolve/v1"]}`)
		manifest.RuntimeLimits.HandlerTimeoutMS = 1000
	})
	ctx := context.Background()
	if _, err := manager.SetDesired(ctx, "admin", pluginID, oldArtifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(old) error = %v", err)
	}
	current, err := manager.SetDesired(ctx, "admin", pluginID, newArtifact.ID, DesiredDisabled, `{}`, 10)
	if err != nil {
		t.Fatalf("SetDesired(new) error = %v", err)
	}
	manager.sandboxPolicy = normalizeSandboxPolicy(SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024})
	if _, err := manager.RollbackArtifact(ctx, "admin", pluginID, oldArtifact.ID); err == nil || !strings.Contains(err.Error(), "network.egress") {
		t.Fatalf("RollbackArtifact() error = %v, want current capability gate block", err)
	}
	after, err := manager.Plugin(ctx, pluginID)
	if err != nil {
		t.Fatalf("Plugin(after rollback failure) error = %v", err)
	}
	if after.DesiredArtifactID != current.DesiredArtifactID || after.DesiredGeneration != current.DesiredGeneration {
		t.Fatalf("plugin after blocked rollback = %+v, want desired artifact/generation unchanged from %+v", after, current)
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

func TestSandboxProtocolProxyStreamRelayConformanceFocused(t *testing.T) {
	peer := newFakeSandboxStreamPeer(t)
	manager, artifact, _ := enableSandboxStreamProxyForTest(t, "sandbox-stream", peer, api.FailPolicyClose, 1000)

	clientGateway, clientSide := newTestTCPConnPair(t)
	defer clientSide.Close()
	initial := []byte("initial")
	next := []byte("next")
	reply := []byte("sandbox-reply")
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:         "play.example",
			Upstream:     "backend",
			Source:       clientGateway,
			InitialData:  initial,
			ConnectionID: "conn-1",
			TraceID:      "trace-1",
			Metadata: map[string]string{
				"safe":          "value",
				"api_token":     "plain-secret-value",
				"authorization": "Bearer secret",
			},
		})
		errCh <- err
	}()
	pluginConn := peer.nextPluginConn(t)
	pluginDone := make(chan error, 1)
	go func() {
		defer pluginConn.Close()
		got := make([]byte, len(initial)+len(next))
		if _, err := io.ReadFull(pluginConn, got); err != nil {
			pluginDone <- err
			return
		}
		if !bytes.Equal(got, append(append([]byte(nil), initial...), next...)) {
			pluginDone <- fmt.Errorf("plugin stream bytes = %q, want initial replay plus client bytes", got)
			return
		}
		if _, err := pluginConn.Write(reply); err != nil {
			pluginDone <- err
			return
		}
		closeWrite(pluginConn)
		_, err := io.Copy(io.Discard, pluginConn)
		pluginDone <- err
	}()
	if _, err := clientSide.Write(next); err != nil {
		t.Fatalf("client write error = %v", err)
	}
	gotReply := make([]byte, len(reply))
	if _, err := io.ReadFull(clientSide, gotReply); err != nil {
		t.Fatalf("client read reply error = %v", err)
	}
	if !bytes.Equal(gotReply, reply) {
		t.Fatalf("client reply = %q, want %q", gotReply, reply)
	}
	if err := clientSide.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite() error = %v", err)
	}
	if err := <-pluginDone; err != nil {
		t.Fatalf("plugin stream error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ConnectUpstream() error = %v", err)
	}
	open := peer.lastOpen(t)
	if open.Protocol != StreamProxyProtocolV1 || open.ExtensionPoint != ExtensionUpstreamConnect || open.HandlerID != "upstream.connect/v1" {
		t.Fatalf("stream_open request = %+v, want stream.proxy/v1 upstream.connect/v1", open)
	}
	if open.Metadata["safe"] != "value" || open.Metadata["api_token"] != "[redacted]" || open.Metadata["authorization"] != "[redacted]" {
		t.Fatalf("stream_open metadata = %+v, want redacted secret metadata", open.Metadata)
	}
	if raw := peer.rawOpenPayload(t); bytes.Contains(raw, initial) || bytes.Contains(raw, []byte("plain-secret-value")) {
		t.Fatalf("stream_open payload leaked initial bytes or secret metadata: %s", raw)
	}
	waitForPluginManagerTest(t, func() bool { return peer.closeCount() >= 1 })
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 || plan.Handlers[0].PluginID != artifact.PluginID {
		t.Fatalf("DispatchPlan handlers = %+v, want sandbox stream handler", plan.Handlers)
	}
	handler := plan.Handlers[0]
	if handler.ProxyStarted != 1 || handler.ProxyCompleted != 1 || handler.ActiveProxy != 0 || handler.ProxyErrors != 0 {
		t.Fatalf("proxy summary = %+v, want one clean completed sandbox stream", handler)
	}
	if got, want := handler.ProxyBytesIn, uint64(len(initial)+len(next)); got != want {
		t.Fatalf("proxy bytes in = %d, want %d", got, want)
	}
	if got, want := handler.ProxyBytesOut, uint64(len(reply)); got != want {
		t.Fatalf("proxy bytes out = %d, want %d", got, want)
	}
}

func TestSandboxProtocolProxyClientAndEndpointClose(t *testing.T) {
	t.Run("client close", func(t *testing.T) {
		peer := newFakeSandboxStreamPeer(t)
		manager, _, _ := enableSandboxStreamProxyForTest(t, "sandbox-client-close", peer, api.FailPolicyClose, 1000)
		clientGateway, clientSide := newTestTCPConnPair(t)
		errCh := make(chan error, 1)
		go func() {
			_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
				Host:   "play.example",
				Source: clientGateway,
			})
			errCh <- err
		}()
		pluginConn := peer.nextPluginConn(t)
		pluginDone := make(chan error, 1)
		go func() {
			defer pluginConn.Close()
			_, err := io.Copy(io.Discard, pluginConn)
			pluginDone <- err
		}()
		if err := clientSide.Close(); err != nil {
			t.Fatalf("client Close() error = %v", err)
		}
		if err := <-pluginDone; err != nil {
			t.Fatalf("plugin read after client close error = %v", err)
		}
		if err := <-errCh; err != nil {
			t.Fatalf("ConnectUpstream() error = %v", err)
		}
	})

	t.Run("endpoint close", func(t *testing.T) {
		peer := newFakeSandboxStreamPeer(t)
		peer.mode = "endpoint_close"
		manager, _, _ := enableSandboxStreamProxyForTest(t, "sandbox-endpoint-close", peer, api.FailPolicyClose, 1000)
		clientGateway, clientSide := newTestTCPConnPair(t)
		defer clientSide.Close()
		errCh := make(chan error, 1)
		go func() {
			_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
				Host:   "play.example",
				Source: clientGateway,
			})
			errCh <- err
		}()
		_ = peer.nextPluginConn(t)
		_ = clientSide.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("ConnectUpstream() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ConnectUpstream() did not return after sandbox endpoint close")
		}
	})
}

func TestSandboxProtocolProxyTimeoutBackpressureAndForceClose(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		peer := newFakeSandboxStreamPeer(t)
		peer.mode = "timeout"
		manager, _, _ := enableSandboxStreamProxyForTest(t, "sandbox-timeout-stream", peer, api.FailPolicyClose, 20)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		result, err := manager.ConnectUpstream(ctx, api.UpstreamConnectRequest{Host: "play.example"})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ConnectUpstream(timeout) = %+v err=%v, want deadline exceeded", result, err)
		}
	})

	t.Run("backpressure and force close", func(t *testing.T) {
		peer := newFakeSandboxStreamPeer(t)
		manager, _, _ := enableSandboxStreamProxyForTest(t, "sandbox-force-close", peer, api.FailPolicyClose, 1000)
		clientGateway, clientSide := newTestTCPConnPair(t)
		defer clientSide.Close()
		errCh := make(chan error, 1)
		go func() {
			_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
				Host:   "play.example",
				Source: clientGateway,
			})
			errCh <- err
		}()
		pluginConn := peer.nextPluginConn(t)
		defer pluginConn.Close()
		payload := bytes.Repeat([]byte("x"), 512*1024)
		readDone := make(chan int, 1)
		go func() {
			buf := make([]byte, 4096)
			total := 0
			for total < len(payload) {
				n, err := pluginConn.Read(buf)
				if n > 0 {
					total += n
					time.Sleep(time.Millisecond)
				}
				if err != nil {
					break
				}
			}
			readDone <- total
		}()
		writeDone := make(chan error, 1)
		go func() {
			_, err := clientSide.Write(payload)
			writeDone <- err
		}()
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatalf("client large write error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("client large write timed out under sandbox stream relay backpressure")
		}
		if got := <-readDone; got != len(payload) {
			t.Fatalf("plugin read bytes = %d, want %d", got, len(payload))
		}
		waitForPluginManagerTest(t, func() bool {
			return manager.activeProxyCountLocked("sandbox-force-close") == 1
		})
		if _, err := manager.Disable(context.Background(), "admin", "sandbox-force-close"); err != nil {
			t.Fatalf("Disable() error = %v", err)
		}
		active, err := manager.ActiveProxyConnections(context.Background(), "sandbox-force-close")
		if err != nil {
			t.Fatalf("ActiveProxyConnections() error = %v", err)
		}
		if len(active) != 1 || !active[0].Draining || active[0].ForceCloseRequested {
			t.Fatalf("active sandbox proxy after disable = %+v, want draining stream", active)
		}
		closed, err := manager.ForceCloseDraining(context.Background(), "admin", "sandbox-force-close")
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
			return manager.activeProxyCountLocked("sandbox-force-close") == 0
		})
	})
}

func TestSandboxProtocolProxyProcessCrashClosesActiveStreams(t *testing.T) {
	peer := newFakeSandboxStreamPeer(t)
	manager, _, process := enableSandboxStreamProxyForTest(t, "sandbox-crash-stream", peer, api.FailPolicyClose, 1000)
	clientGateway, clientSide := newTestTCPConnPair(t)
	defer clientSide.Close()
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:   "play.example",
			Source: clientGateway,
		})
		errCh <- err
	}()
	pluginConn := peer.nextPluginConn(t)
	defer pluginConn.Close()
	waitForPluginManagerTest(t, func() bool {
		return manager.activeProxyCountLocked("sandbox-crash-stream") == 1
	})
	closeSandboxProcessForTest(process)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ConnectUpstream() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active sandbox stream did not close after process crash")
	}
	waitForPluginManagerTest(t, func() bool {
		return manager.activeProxyCountLocked("sandbox-crash-stream") == 0
	})
	if os.Getpid() <= 0 {
		t.Fatal("gateway process exited during sandbox process crash")
	}
}

type fakeSandboxStreamPeer struct {
	mu          sync.Mutex
	mode        string
	opens       []SandboxStreamOpenRequest
	rawOpens    [][]byte
	closes      []SandboxStreamCloseRequest
	pluginConns chan net.Conn
	makePair    func() (net.Conn, net.Conn)
	process     *SandboxProcess
}

func newFakeSandboxStreamPeer(t *testing.T) *fakeSandboxStreamPeer {
	t.Helper()
	peer := &fakeSandboxStreamPeer{
		pluginConns: make(chan net.Conn, 8),
	}
	peer.makePair = func() (net.Conn, net.Conn) {
		gatewayEnd, pluginEnd := newTestTCPConnPair(t)
		return gatewayEnd, pluginEnd
	}
	return peer
}

func (p *fakeSandboxStreamPeer) invoke(ctx context.Context, _ string, req SandboxControlRequest) (SandboxControlResponse, error) {
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
	switch req.Command {
	case sandboxControlCommandStreamOpen:
		if p.mode == "timeout" {
			<-ctx.Done()
			return SandboxControlResponse{}, ctx.Err()
		}
		var payload SandboxStreamOpenRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return SandboxControlResponse{}, err
		}
		p.mu.Lock()
		p.opens = append(p.opens, payload)
		p.rawOpens = append(p.rawOpens, append([]byte(nil), req.Payload...))
		p.mu.Unlock()
		resp.Stream = &SandboxStreamOpenResponse{
			Connected:    true,
			Protocol:     StreamProxyProtocolV1,
			StreamID:     payload.StreamID,
			Endpoint:     "/run/streams/" + payload.StreamID + ".sock",
			EndpointType: "unix",
		}
		return resp, nil
	case sandboxControlCommandStreamClose:
		var payload SandboxStreamCloseRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return SandboxControlResponse{}, err
		}
		p.mu.Lock()
		p.closes = append(p.closes, payload)
		p.mu.Unlock()
		return resp, nil
	case sandboxControlCommandDrain, sandboxControlCommandReloadConfig:
		return resp, nil
	default:
		return setSandboxControlError(resp, sandboxControlErrorUnknownCommand, "unexpected fake stream peer command"), nil
	}
}

func (p *fakeSandboxStreamPeer) dial(ctx context.Context, _ *SandboxProcess, _ SandboxStreamOpenResponse) (net.Conn, error) {
	_ = ctx
	gatewayEnd, pluginEnd := p.makePair()
	p.pluginConns <- pluginEnd
	if p.mode == "endpoint_close" {
		go func() {
			time.Sleep(10 * time.Millisecond)
			_ = pluginEnd.Close()
		}()
	}
	return gatewayEnd, nil
}

func (p *fakeSandboxStreamPeer) nextPluginConn(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-p.pluginConns:
		return conn
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sandbox stream endpoint")
		return nil
	}
}

func (p *fakeSandboxStreamPeer) lastOpen(t *testing.T) SandboxStreamOpenRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.opens) == 0 {
		t.Fatal("no sandbox stream_open request recorded")
	}
	return p.opens[len(p.opens)-1]
}

func (p *fakeSandboxStreamPeer) rawOpenPayload(t *testing.T) []byte {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rawOpens) == 0 {
		t.Fatal("no sandbox raw stream_open payload recorded")
	}
	return append([]byte(nil), p.rawOpens[len(p.rawOpens)-1]...)
}

func (p *fakeSandboxStreamPeer) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.closes)
}

func enableSandboxStreamProxyForTest(t *testing.T, pluginID string, peer *fakeSandboxStreamPeer, failPolicy string, timeoutMS int64) (*Manager, ArtifactRecord, *SandboxProcess) {
	t.Helper()
	if failPolicy == "" {
		failPolicy = api.FailPolicyClose
	}
	if timeoutMS == 0 {
		timeoutMS = 1000
	}
	policy := SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024, ExternalIsolation: true}
	manager := New(Options{
		DB:                 openPluginManagerTestDB(t),
		ArtifactRoot:       t.TempDir(),
		FutureRuntimeGates: FutureRuntimeGates{SandboxProcess: true},
		SandboxSelfCheck:   func(SandboxPolicy) error { return nil },
		SandboxPolicy:      policy,
	})
	registration := SandboxHandlerRegistration{
		ExtensionPoint: ExtensionUpstreamConnect,
		HandlerID:      "upstream.connect/v1",
		FailPolicy:     failPolicy,
		TimeoutMS:      timeoutMS,
		SchemaVersion:  1,
		DeclaredCapabilities: []string{
			StreamProxyProtocolV1,
		},
	}
	manager.adapter = SandboxProcessAdapter{
		Policy:    policy,
		SelfCheck: func(SandboxPolicy) error { return nil },
		startProcess: func(_ context.Context, _ SandboxSupervisor, pluginID, artifactID, _ string, generation int64, configJSON string, _ SandboxSecretResolver) (*SandboxProcess, error) {
			process := newSandboxControlProcessForTest(pluginID, artifactID, generation, configJSON)
			process.SocketPath = "fake-sandbox-stream-control.sock"
			process.Registrations = []SandboxHandlerRegistration{registration}
			process.registerOK = true
			process.UpstreamMode = UpstreamModeProtocolProxy
			process.controlInvoker = peer.invoke
			process.streamDialer = peer.dial
			peer.process = process
			return process, nil
		},
	}
	manager.serviceMode = PluginServiceModeSandboxProcess
	artifact := uploadSandboxStreamArtifactForTest(t, manager, pluginID, timeoutMS)
	if _, err := manager.SetDesired(context.Background(), "admin", pluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	approveGovernanceForTest(t, manager, pluginID, artifact.ID)
	if _, err := manager.Enable(context.Background(), "admin", pluginID); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if peer.process == nil {
		t.Fatal("sandbox stream fake process was not started")
	}
	return manager, artifact, peer.process
}

func uploadSandboxStreamArtifactForTest(t *testing.T, manager *Manager, pluginID string, timeoutMS int64) ArtifactRecord {
	t.Helper()
	return uploadTestArtifactWithManifest(t, manager, pluginID, func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ExtensionPoints = []ExtensionPoint{{Type: "hook", Key: ExtensionUpstreamConnect}}
		manifest.Capabilities = json.RawMessage(`{
			"extension_points":["upstream.connect/v1"],
			"upstream_connect":{"mode":"protocol-proxy"},
			"scope":{"type":"host","values":["play.example"]},
			"rollout":{"mode":"canary"},
			"minecraft":{
				"protocol_versions":{"tested":[767]},
				"forwarding":{"supported":["none"],"default":"none"}
			}
		}`)
		manifest.RuntimeLimits.HandlerTimeoutMS = int(timeoutMS)
		manifest.RuntimeLimits.InitialWriteTimeoutMS = 1000
	})
}

type fakeSandboxControlPeer struct {
	mu             sync.Mutex
	mode           string
	calls          []SandboxInvokeRequest
	commands       []string
	starts         []string
	reloadFailures int
	startErrs      map[string]error
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
		p.mu.Lock()
		failReload := p.reloadFailures > 0
		if failReload {
			p.reloadFailures--
		}
		p.mu.Unlock()
		if failReload {
			return setSandboxControlError(resp, sandboxControlErrorSchemaInvalid, "reload rejected by fake sandbox"), nil
		}
		return resp, nil
	}
	if req.Command == sandboxControlCommandDrain {
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

func (p *fakeSandboxControlPeer) recordStart(pluginID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, pluginID)
}

func (p *fakeSandboxControlPeer) startOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...)
}

func (p *fakeSandboxControlPeer) failNextReload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloadFailures++
}

func newSandboxDispatchManagerForTest(t *testing.T, peer *fakeSandboxControlPeer, registrations []SandboxHandlerRegistration) *Manager {
	t.Helper()
	return newSandboxDispatchManagerWithDBForTest(t, openPluginManagerTestDB(t), t.TempDir(), peer, registrations)
}

func newSandboxDispatchManagerWithDBForTest(t *testing.T, db *sql.DB, artifactRoot string, peer *fakeSandboxControlPeer, registrations []SandboxHandlerRegistration) *Manager {
	t.Helper()
	policy := SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024, ExternalIsolation: true}
	manager := New(Options{
		DB:                 db,
		ArtifactRoot:       artifactRoot,
		FutureRuntimeGates: FutureRuntimeGates{SandboxProcess: true},
		SandboxSelfCheck:   func(SandboxPolicy) error { return nil },
		SandboxPolicy:      policy,
	})
	if _, err := manager.SetPluginServiceDesired(context.Background(), "admin", PluginServiceModeSandboxProcess); err != nil {
		t.Fatalf("SetPluginServiceDesired(sandbox) error = %v", err)
	}
	if err := manager.ApplyPluginServiceMode(context.Background()); err != nil {
		t.Fatalf("ApplyPluginServiceMode(sandbox) error = %v", err)
	}
	manager.adapter = SandboxProcessAdapter{
		Policy:    policy,
		SelfCheck: func(SandboxPolicy) error { return nil },
		startProcess: func(_ context.Context, _ SandboxSupervisor, pluginID, artifactID, _ string, generation int64, configJSON string, _ SandboxSecretResolver) (*SandboxProcess, error) {
			peer.recordStart(pluginID)
			if peer.startErrs != nil && peer.startErrs[pluginID] != nil {
				return nil, peer.startErrs[pluginID]
			}
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
