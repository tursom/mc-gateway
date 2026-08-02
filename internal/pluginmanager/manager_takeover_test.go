package pluginmanager

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestManagerTakeoverLifecycleAndRestartRecovery(t *testing.T) {
	db := openPluginManagerTestDB(t)
	root := t.TempDir()
	firstAdapter := &fakeAdapter{}
	first := New(Options{DB: db, ArtifactRoot: root, Adapter: firstAdapter})
	artifact := uploadTestArtifact(t, first, "takeover-lifecycle")
	if _, err := first.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Load(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if firstAdapter.loads != 1 || takeoverHandledForTest(t, first) {
		t.Fatal("load-only runtime entered takeover dispatch")
	}
	if _, err := first.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if firstAdapter.loads != 1 || !takeoverHandledForTest(t, first) {
		t.Fatal("enabled runtime did not handle takeover")
	}

	secondAdapter := &fakeAdapter{}
	second := New(Options{DB: db, ArtifactRoot: root, Adapter: secondAdapter})
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondAdapter.loads != 1 || !takeoverHandledForTest(t, second) {
		t.Fatal("reconciled runtime did not restore takeover dispatch")
	}
	if _, err := second.Disable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if takeoverHandledForTest(t, second) {
		t.Fatal("disabled runtime remained in takeover dispatch")
	}
	if _, err := second.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if err := second.Delete(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Plugin(context.Background(), artifact.PluginID); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("Plugin(after delete) error = %v", err)
	}
}

func TestManagerTakeoverLoadFailureKeepsExistingDispatch(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{loadErrs: map[string]error{"plugin-b": errors.New("open failed")}})
	artifactA := uploadTestArtifact(t, manager, "plugin-a")
	artifactB := uploadTestArtifact(t, manager, "plugin-b")
	if _, err := manager.SetDesired(context.Background(), "admin", artifactA.PluginID, artifactA.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifactA.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifactB.PluginID, artifactB.ID, DesiredEnabled, `{}`, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifactB.PluginID); err == nil {
		t.Fatal("Enable(plugin-b) error = nil")
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 || plan.Handlers[0].PluginID != artifactA.PluginID || !takeoverHandledForTest(t, manager) {
		t.Fatalf("dispatch after failed load = %+v", plan.Handlers)
	}
}

func TestManagerReconcileRemovesTakeoverRuntimeOutsideDesiredSet(t *testing.T) {
	recorder := &reconcileLifecycleRecorder{}
	manager := newManagerForTest(t, &reconcileLifecycleAdapter{recorder: recorder})
	artifact := uploadTestArtifact(t, manager, "reconcile-remove-takeover")
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drain:reconcile-remove-takeover:1",
		"destroy:reconcile-remove-takeover:1",
		"stop:reconcile-remove-takeover:1",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime lifecycle = %v, want %v", got, want)
	}
	if takeoverHandledForTest(t, manager) {
		t.Fatal("reconciled disabled runtime remained in takeover dispatch")
	}
}

func TestManagerReconcileStopsInvalidTakeoverReplacement(t *testing.T) {
	recorder := &reconcileLifecycleRecorder{}
	adapter := &reconcileLifecycleAdapter{recorder: recorder, skipHandlers: make(map[string]bool)}
	manager := newManagerForTest(t, adapter)
	first := uploadTestArtifact(t, manager, "reconcile-invalid-replacement")
	second := uploadTestArtifactWithManifestBytes(t, manager, first.PluginID, []byte("replacement without handlers"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	adapter.skipHandlers[second.ID] = true
	if _, err := manager.SetDesired(context.Background(), "admin", first.PluginID, first.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", first.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", second.PluginID, second.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drain:reconcile-invalid-replacement:1", "destroy:reconcile-invalid-replacement:1", "stop:reconcile-invalid-replacement:1",
		"drain:reconcile-invalid-replacement:2", "destroy:reconcile-invalid-replacement:2", "stop:reconcile-invalid-replacement:2",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime lifecycle = %v, want %v", got, want)
	}
	if takeoverHandledForTest(t, manager) {
		t.Fatal("invalid replacement remained in takeover dispatch")
	}
}

func TestManagerReconcileCleanupErrorsDoNotRestoreTakeoverRuntime(t *testing.T) {
	recorder := &reconcileLifecycleRecorder{}
	manager := newManagerForTest(t, &reconcileLifecycleAdapter{
		recorder: recorder, drainErr: errors.New("drain failed"), destroyErr: errors.New("destroy failed"), stopErr: errors.New("stop failed"),
	})
	artifact := uploadTestArtifact(t, manager, "reconcile-cleanup-errors")
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("cleanup errors should be warnings: %v", err)
	}
	want := []string{
		"drain:reconcile-cleanup-errors:1", "destroy:reconcile-cleanup-errors:1", "stop:reconcile-cleanup-errors:1",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime lifecycle = %v, want %v", got, want)
	}
	if takeoverHandledForTest(t, manager) {
		t.Fatal("removed runtime was restored after cleanup errors")
	}
}

func TestManagerReconcileStopsOldTakeoverRuntimeWhenReplacementFails(t *testing.T) {
	recorder := &reconcileLifecycleRecorder{}
	adapter := &reconcileLifecycleAdapter{recorder: recorder, startErrsByArtifact: make(map[string]error)}
	manager := newManagerForTest(t, adapter)
	first := uploadTestArtifact(t, manager, "reconcile-failed-replacement")
	second := uploadTestArtifactWithManifestBytes(t, manager, first.PluginID, []byte("failed replacement"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	adapter.startErrsByArtifact[second.ID] = errors.New("replacement start failed")
	if _, err := manager.SetDesired(context.Background(), "admin", first.PluginID, first.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", first.PluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDesired(context.Background(), "admin", second.PluginID, second.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drain:reconcile-failed-replacement:1", "destroy:reconcile-failed-replacement:1", "stop:reconcile-failed-replacement:1",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime lifecycle = %v, want %v", got, want)
	}
	if takeoverHandledForTest(t, manager) {
		t.Fatal("failed replacement retained a takeover handler")
	}
	plugin, err := manager.Plugin(context.Background(), first.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.RuntimeState != RuntimeFailed || !strings.Contains(plugin.LastError, "replacement start failed") {
		t.Fatalf("plugin after failed replacement = %+v", plugin)
	}
}

func TestManagerReconcileDefersInProcessRuntimeRetirementUntilTakeoverReturns(t *testing.T) {
	recorder := &reconcileLifecycleRecorder{}
	invoking := make(chan struct{})
	release := make(chan struct{})
	adapter := &reconcileLifecycleAdapter{
		recorder: recorder,
		handlers: map[int64]api.UpstreamConnectHandlerV2{
			1: func(api.UpstreamConnectRequestV2) error {
				close(invoking)
				<-release
				return nil
			},
		},
	}
	manager := newManagerForTest(t, adapter)
	defer manager.Close(context.Background())
	first := uploadTestArtifact(t, manager, "reconcile-handler-reservation")
	second := uploadTestArtifactWithManifestBytes(t, manager, first.PluginID, []byte("handler replacement"), func(manifest *Manifest) {
		manifest.Version = "0.2.0"
	})
	if _, err := manager.SetDesired(context.Background(), "admin", first.PluginID, first.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable(context.Background(), "admin", first.PluginID); err != nil {
		t.Fatal(err)
	}
	root, peer := net.Pipe()
	defer root.Close()
	defer peer.Close()
	done := make(chan error, 1)
	go func() {
		done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(context.Context, api.ConnectionState) error { return nil })
	}()
	select {
	case <-invoking:
	case <-time.After(2 * time.Second):
		t.Fatal("old handler did not start")
	}
	if _, err := manager.SetDesired(context.Background(), "admin", second.PluginID, second.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("lifecycle while handler runs = %v, want no retirement calls", got)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old handler did not return")
	}
	waitForPluginManagerTest(t, func() bool {
		return reflect.DeepEqual(recorder.snapshot(), []string{
			"drain:reconcile-handler-reservation:1",
			"destroy:reconcile-handler-reservation:1",
			"stop:reconcile-handler-reservation:1",
		})
	})
}

func takeoverHandledForTest(t *testing.T, manager *Manager) bool {
	t.Helper()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	coreCalled := false
	if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error {
		coreCalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return !coreCalled
}

func TestHandleConnectionDispatchFlow(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer right.Close()
	replacement, replacementPeer := net.Pipe()
	defer replacementPeer.Close()
	var order []string
	manager.publish([]*upstreamHandler{
		{pluginID: "second", priority: 20, handlerID: ExtensionUpstreamConnect, handle: func(req api.UpstreamConnectRequestV2) error {
			order = append(order, "second")
			if req.PeerAddr != "192.0.2.1:1234" || req.Ingress.Transport != "websocket" || req.Ingress.HTTP.Headers.Get("Authorization") != "secret" {
				t.Fatalf("immutable ingress facts changed: %+v", req)
			}
			return req.Flow.Next(req.Connection)
		}},
		{pluginID: "first", priority: 10, handlerID: ExtensionUpstreamConnect, handle: func(req api.UpstreamConnectRequestV2) error {
			order = append(order, "first")
			req.Ingress.Transport = "mutated"
			req.Ingress.HTTP.Headers.Set("Authorization", "changed")
			state := req.Connection
			state.Stream = replacement
			state.EffectiveSourceAddr = "198.51.100.8:0"
			state.Metadata["first"] = "yes"
			return req.Flow.Next(state)
		}},
	})
	coreCalled := false
	err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(_ context.Context, state api.ConnectionState) error {
		coreCalled = true
		if state.Stream != replacement || state.EffectiveSourceAddr != "198.51.100.8:0" || state.Metadata["first"] != "yes" {
			t.Fatalf("core state = %+v", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !coreCalled || !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("core=%v order=%v", coreCalled, order)
	}
}

func TestHandleConnectionCoreBypassesRemainingHandlers(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	secondCalled := false
	manager.publish([]*upstreamHandler{
		{pluginID: "first", priority: 10, handle: func(req api.UpstreamConnectRequestV2) error { return req.Flow.Core(req.Connection) }},
		{pluginID: "second", priority: 20, handle: func(api.UpstreamConnectRequestV2) error { secondCalled = true; return nil }},
	})
	coreCalled := false
	if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { coreCalled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if secondCalled || !coreCalled {
		t.Fatalf("second=%v core=%v", secondCalled, coreCalled)
	}
}

func TestHandleConnectionHandledAndContinuationOneShot(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	manager.publish([]*upstreamHandler{{pluginID: "handled", handle: func(api.UpstreamConnectRequestV2) error { return nil }}})
	coreCalled := false
	if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { coreCalled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if coreCalled {
		t.Fatal("fully handled connection entered core")
	}

	manager.publish([]*upstreamHandler{{pluginID: "double", handle: func(req api.UpstreamConnectRequestV2) error {
		if err := req.Flow.Core(req.Connection); err != nil {
			return err
		}
		return req.Flow.Next(req.Connection)
	}}})
	if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { return nil }); !errors.Is(err, api.ErrContinuationUsed) {
		t.Fatalf("duplicate continuation error = %v", err)
	}
}

func TestHandleConnectionHandlerFailureDoesNotFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler api.UpstreamConnectHandlerV2
	}{
		{name: "error", handler: func(api.UpstreamConnectRequestV2) error { return errors.New("failed") }},
		{name: "panic", handler: func(api.UpstreamConnectRequestV2) error { panic("failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newManagerForTest(t, nil)
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			downstream := false
			manager.publish([]*upstreamHandler{
				{pluginID: "bad", handle: test.handler},
				{pluginID: "downstream", handle: func(api.UpstreamConnectRequestV2) error { downstream = true; return nil }},
			})
			core := false
			if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { core = true; return nil }); err == nil {
				t.Fatal("handler failure returned nil")
			}
			if downstream || core {
				t.Fatalf("failure fell back: downstream=%v core=%v", downstream, core)
			}
		})
	}
}

func TestConnectionSessionTracksChainAndForceClosesRoot(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer right.Close()
	entered := make(chan struct{})
	first := &upstreamHandler{pluginID: "first", handle: func(req api.UpstreamConnectRequestV2) error { return req.Flow.Next(req.Connection) }}
	second := &upstreamHandler{pluginID: "second", handle: func(req api.UpstreamConnectRequestV2) error {
		close(entered)
		buf := make([]byte, 1)
		_, err := req.Connection.Stream.Read(buf)
		return err
	}}
	manager.publish([]*upstreamHandler{first, second})
	done := make(chan error, 1)
	go func() {
		done <- manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("chain did not enter second handler")
	}
	manager.markDrainingLocked("first")
	closed, err := manager.ForceCloseDraining(context.Background(), "test", "first")
	if err != nil || closed != 1 {
		t.Fatalf("force close = %d, %v", closed, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("force close did not unwind chain")
	}
	if got := manager.activeConnectionSessionCountLocked("first"); got != 0 {
		t.Fatalf("active sessions = %d", got)
	}
}

func TestConnectionSessionForceCloseClosesReplacementStream(t *testing.T) {
	manager := newManagerForTest(t, nil)
	root, client := net.Pipe()
	defer client.Close()
	replacement, replacementPeer := net.Pipe()
	defer replacementPeer.Close()
	entered := make(chan struct{})
	manager.publish([]*upstreamHandler{
		{pluginID: "replace", priority: 10, handle: func(req api.UpstreamConnectRequestV2) error {
			state := req.Connection
			state.Stream = replacement
			return req.Flow.Next(state)
		}},
		{pluginID: "block", priority: 20, handle: func(req api.UpstreamConnectRequestV2) error {
			close(entered)
			_, err := req.Connection.Stream.Read(make([]byte, 1))
			return err
		}},
	})
	done := make(chan error, 1)
	go func() {
		done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(context.Context, api.ConnectionState) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("replacement stream did not reach downstream handler")
	}
	manager.markDrainingLocked("replace")
	if closed, err := manager.ForceCloseDraining(context.Background(), "test", "replace"); err != nil || closed != 1 {
		t.Fatalf("ForceCloseDraining() = %d, %v", closed, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replacement stream kept the takeover chain blocked")
	}
}

func TestConnectionSessionForceCloseCancelsHandlerContext(t *testing.T) {
	manager := newManagerForTest(t, nil)
	root, peer := net.Pipe()
	defer peer.Close()
	entered := make(chan struct{})
	manager.publish([]*upstreamHandler{{pluginID: "context", handle: func(req api.UpstreamConnectRequestV2) error {
		close(entered)
		<-req.Context.Done()
		return req.Context.Err()
	}}})
	done := make(chan error, 1)
	go func() {
		done <- manager.HandleConnection(context.Background(), takeoverRequest(root), func(context.Context, api.ConnectionState) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	manager.markDrainingLocked("context")
	if closed, err := manager.ForceCloseDraining(context.Background(), "test", "context"); err != nil || closed != 1 {
		t.Fatalf("ForceCloseDraining() = %d, %v", closed, err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("force close did not cancel handler context")
	}
}

func TestConnectionFlowCannotBeUsedAfterHandlerReturns(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var retained api.UpstreamConnectFlow
	var state api.ConnectionState
	manager.publish([]*upstreamHandler{{pluginID: "retain", handle: func(req api.UpstreamConnectRequestV2) error {
		retained = req.Flow
		state = req.Connection
		return nil
	}}})
	if err := manager.HandleConnection(context.Background(), takeoverRequest(left), func(context.Context, api.ConnectionState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := retained.Next(state); err == nil {
		t.Fatal("retained continuation remained usable after handler return")
	}
}

func TestOfficialTrustedRealIPIsRegisteredButDisabledByDefault(t *testing.T) {
	manager := newManagerForTest(t, nil)
	artifact, err := manager.Artifact(context.Background(), "builtin-official-trusted-real-ip-0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.PluginID != "official.trusted-real-ip" || artifact.RuntimeType != RuntimeBuiltin {
		t.Fatalf("trusted real IP artifact = %+v", artifact)
	}
	if _, err := manager.Plugin(context.Background(), artifact.PluginID); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("default plugin state error = %v, want disabled/unconfigured plugin", err)
	}
	for _, configJSON := range []string{
		`{"header":"   ","trusted_peers":["127.0.0.1/32"]}`,
		`{"header":"X-Real-IP","trusted_peers":[]}`,
		`{"header":"X-Real-IP","trusted_peers":["invalid"]}`,
	} {
		if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, configJSON, 10); err == nil {
			t.Fatalf("SetDesired(%s) error = nil", configJSON)
		}
	}
	if _, err := manager.SetDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(default config) error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", artifact.PluginID); err != nil {
		t.Fatalf("Enable(default config) error = %v", err)
	}
	plan := manager.DispatchPlan(context.Background())
	if len(plan.Handlers) != 1 || plan.Handlers[0].PluginID != artifact.PluginID || plan.Handlers[0].ExtensionPoint != ExtensionUpstreamConnect {
		t.Fatalf("trusted real IP dispatch = %+v", plan.Handlers)
	}
}

func TestHandleConnectionDoesNotPersistIngressHeaders(t *testing.T) {
	manager := newManagerForTest(t, nil)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	manager.publish([]*upstreamHandler{{
		pluginID: "privacy", handlerID: ExtensionUpstreamConnect,
		handle: func(req api.UpstreamConnectRequestV2) error { return req.Flow.Core(req.Connection) },
	}})
	req := takeoverRequest(left)
	req.Ingress.HTTP.Headers.Set("Authorization", "top-secret-token")
	if err := manager.HandleConnection(context.Background(), req, func(context.Context, api.ConnectionState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var fieldsJSON string
	if err := manager.repo.db.QueryRowContext(context.Background(), `SELECT fields_json FROM plugin_traces WHERE plugin_id = ? ORDER BY id DESC LIMIT 1`, "privacy").Scan(&fieldsJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fieldsJSON, "Authorization") || strings.Contains(fieldsJSON, "top-secret-token") {
		t.Fatalf("trace persisted HTTP header data: %s", fieldsJSON)
	}
}

func takeoverRequest(stream net.Conn) api.UpstreamConnectRequestV2 {
	return api.UpstreamConnectRequestV2{
		Context: context.Background(), ConnectionID: "connection", TraceID: "trace",
		PeerAddr: "192.0.2.1:1234", LocalAddr: "192.0.2.2:25565",
		Connection: api.ConnectionState{Stream: stream, EffectiveSourceAddr: "192.0.2.1:1234", Metadata: map[string]string{}},
		Ingress:    api.IngressContext{Transport: "websocket", ServiceName: "websocket", ListenerPort: 25565, HTTP: &api.HTTPIngressContext{Method: "GET", Host: "example", Path: "/mc", Headers: http.Header{"Authorization": []string{"secret"}}}},
	}
}
