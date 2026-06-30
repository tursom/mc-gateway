package pluginmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSandboxSecretRPCAuthorizationValueRedactionAndRotation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter:      &fakeAdapter{},
		Now:          func() time.Time { return now },
	})
	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-s6-secret", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"api_secret_ref":{"type":"string"},"unused_secret_ref":{"type":"string"}}}`)
		manifest.Secrets = []SecretSpec{
			{Name: "api_token", Type: "secret", Rotation: SecretRotation{GracePeriod: "2m", Reload: "hot"}},
			{Name: "unused_token", Type: "secret", Rotation: SecretRotation{GracePeriod: "2m"}},
		}
	})
	if _, err := manager.UpsertSecret(context.Background(), "admin", artifact.PluginID, artifact.ID, "api_token", "secret-one", false, true); err != nil {
		t.Fatalf("UpsertSecret(api_token) error = %v", err)
	}
	if _, err := manager.UpsertSecret(context.Background(), "admin", artifact.PluginID, artifact.ID, "unused_token", "unused-secret", false, false); err != nil {
		t.Fatalf("UpsertSecret(unused_token) error = %v", err)
	}
	plugin, err := manager.repo.UpsertDesired(context.Background(), "admin", artifact.PluginID, artifact.ID, DesiredDisabled, `{"api_secret_ref":"api_token"}`, 10)
	if err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	baseReq := SandboxSecretRequest{PluginID: artifact.PluginID, ArtifactID: artifact.ID, Generation: plugin.DesiredGeneration, Handle: "api_token"}
	undeclared, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: artifact.PluginID, ArtifactID: artifact.ID, Generation: plugin.DesiredGeneration, Handle: "missing"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(undeclared) error = %v", err)
	}
	if undeclared.OK || undeclared.ErrorCode != "secret_not_declared" {
		t.Fatalf("undeclared secret response = %+v, want secret_not_declared", undeclared)
	}
	outOfScope, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: artifact.PluginID, ArtifactID: artifact.ID, Generation: plugin.DesiredGeneration, Handle: "unused_token"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(out of scope) error = %v", err)
	}
	if outOfScope.OK || outOfScope.ErrorCode != "secret_not_in_config_scope" {
		t.Fatalf("out-of-scope secret response = %+v, want secret_not_in_config_scope", outOfScope)
	}
	first, err := manager.ResolveSandboxSecret(context.Background(), baseReq)
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(first) error = %v", err)
	}
	if !first.OK || first.Value != "secret-one" || first.Version != 1 || first.TTLSeconds <= 0 || first.RedactionHandle == "" || first.Scope == "" {
		t.Fatalf("first secret response = %+v, want short-lived value", first)
	}
	now = now.Add(10 * time.Second)
	if _, err := manager.UpsertSecret(context.Background(), "admin", artifact.PluginID, artifact.ID, "api_token", "secret-two", false, true); err != nil {
		t.Fatalf("UpsertSecret(rotation) error = %v", err)
	}
	current, err := manager.ResolveSandboxSecret(context.Background(), baseReq)
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(current) error = %v", err)
	}
	if !current.OK || current.Value != "secret-two" || current.Version != 2 {
		t.Fatalf("current secret response = %+v, want rotated current version", current)
	}
	previous, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: artifact.PluginID, ArtifactID: artifact.ID, Generation: plugin.DesiredGeneration, Handle: "api_token", Version: 1})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(previous) error = %v", err)
	}
	if !previous.OK || previous.Value != "secret-one" || previous.Version != 1 || previous.TTLSeconds <= 0 {
		t.Fatalf("previous secret response = %+v, want previous version inside grace", previous)
	}
	now = now.Add(3 * time.Minute)
	expired, err := manager.ResolveSandboxSecret(context.Background(), SandboxSecretRequest{PluginID: artifact.PluginID, ArtifactID: artifact.ID, Generation: plugin.DesiredGeneration, Handle: "api_token", Version: 1})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret(expired) error = %v", err)
	}
	if expired.OK || expired.ErrorCode != "previous_secret_expired" {
		t.Fatalf("expired previous response = %+v, want previous_secret_expired", expired)
	}

	process := &SandboxProcess{
		PluginID:          artifact.PluginID,
		ArtifactID:        artifact.ID,
		RuntimeInstanceID: "sandbox-s6-secret-runtime",
		Generation:        plugin.DesiredGeneration,
		Protocol:          sandboxProcessProtocol,
		Policy: SandboxPolicy{
			Env:               map[string]string{"SAFE": "1", "API_SECRET": "secret-two"},
			SecretHandles:     []string{"api_token"},
			CPUSeconds:        1,
			MemoryBytes:       8 * 1024 * 1024,
			ExternalIsolation: true,
		},
	}
	diag, _ := json.Marshal(process.Diagnostics())
	env := strings.Join(sandboxEnv(process.Policy), "\n")
	operations, err := manager.repo.ListOperations(context.Background(), artifact.PluginID, 50)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	audit, _ := json.Marshal(operations)
	for _, forbidden := range []string{"secret-one", "secret-two", "unused-secret"} {
		if strings.Contains(string(diag), forbidden) || strings.Contains(env, forbidden) || strings.Contains(string(audit), forbidden) {
			t.Fatalf("secret value %q leaked into diagnostics/env/audit: diag=%s env=%s audit=%s", forbidden, diag, env, audit)
		}
	}
}

func TestSandboxExternalDependencyRPCBlocksUndeclaredAndEndpointBypass(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	var (
		hitsMu sync.Mutex
		hits   []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits = append(hits, r.URL.RequestURI())
		hitsMu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	artifact := uploadTestArtifactWithManifest(t, manager, "sandbox-s6-external", func(manifest *Manifest) {
		manifest.Runtime.Type = RuntimeSandbox
		manifest.ExternalDeps = []ExternalSpec{{
			Name:       "session",
			Endpoint:   server.URL + "/session",
			Purpose:    "auth",
			Timeout:    "100ms",
			FailPolicy: ExternalFailPolicyClosed,
		}}
	})
	manifest := mustManifestFromArtifact(t, artifact)
	process := &SandboxProcess{
		PluginID:          artifact.PluginID,
		ArtifactID:        artifact.ID,
		RuntimeInstanceID: "sandbox-s6-external-runtime",
		Generation:        7,
		Protocol:          sandboxProcessProtocol,
		operations:        manager.operations.ForPlugin(artifact.PluginID, artifact.ID, manifest),
	}
	callExternal := func(traceID string, req SandboxExternalRequest) SandboxControlResponse {
		payload, _ := json.Marshal(req)
		return process.HandleControlRequest(context.Background(), SandboxControlRequest{
			Command:           sandboxControlCommandExternal,
			Protocol:          sandboxProcessProtocol,
			PluginID:          artifact.PluginID,
			ArtifactID:        artifact.ID,
			RuntimeInstanceID: process.RuntimeInstanceID,
			Generation:        process.Generation,
			TraceID:           traceID,
			Payload:           payload,
		}, nil)
	}

	declared := callExternal("trace-declared", SandboxExternalRequest{Name: "session", Method: http.MethodGet, URL: "/session"})
	if !declared.OK || declared.External == nil || !declared.External.OK || declared.External.StatusCode != http.StatusOK || string(declared.External.Body) != "ok" || declared.External.Summary == nil || !declared.External.Summary.NetworkEnforced {
		t.Fatalf("declared external response = %+v, want successful host-mediated sandbox external call", declared)
	}
	query := callExternal("trace-query", SandboxExternalRequest{Name: "session", Method: http.MethodGet, URL: "/session?next=/admin"})
	if !query.OK || query.External == nil || !query.External.OK || query.External.StatusCode != http.StatusOK || string(query.External.Body) != "ok" {
		t.Fatalf("query external response = %+v, want query allowed on declared path", query)
	}
	child := callExternal("trace-child", SandboxExternalRequest{Name: "session", Method: http.MethodGet, URL: server.URL + "/session/profile?next=/admin"})
	if !child.OK || child.External == nil || !child.External.OK || child.External.StatusCode != http.StatusOK || string(child.External.Body) != "ok" {
		t.Fatalf("child external response = %+v, want query allowed below declared path", child)
	}
	undeclared := callExternal("trace-undeclared", SandboxExternalRequest{Name: "payments", Method: http.MethodGet, URL: "/session"})
	if undeclared.OK || undeclared.External == nil || undeclared.External.ErrorCode != "external_dependency_not_declared" {
		t.Fatalf("undeclared external response = %+v, want external_dependency_not_declared", undeclared)
	}
	for _, tc := range []struct {
		traceID string
		rawURL  string
	}{
		{traceID: "trace-admin-bypass", rawURL: server.URL + "/admin?next=/session"},
		{traceID: "trace-sibling-bypass", rawURL: server.URL + "/session-admin?next=/session"},
		{traceID: "trace-relative-sibling-bypass", rawURL: "/session-admin?next=/session"},
		{traceID: "trace-host-bypass", rawURL: "http://example.com/session"},
	} {
		bypass := callExternal(tc.traceID, SandboxExternalRequest{Name: "session", Method: http.MethodGet, URL: tc.rawURL})
		if bypass.OK || bypass.External == nil || bypass.External.ErrorCode != "external_dependency_request_failed" {
			t.Fatalf("bypass %s external response = %+v, want declared endpoint policy denial", tc.rawURL, bypass)
		}
	}
	hitsMu.Lock()
	gotHits := append([]string(nil), hits...)
	hitsMu.Unlock()
	wantHits := []string{"/session", "/session?next=/admin", "/session/profile?next=/admin"}
	if strings.Join(gotHits, "\n") != strings.Join(wantHits, "\n") {
		t.Fatalf("server hits = %+v, want only in-scope requests %+v", gotHits, wantHits)
	}
	traces, err := manager.repo.RecentTraces(context.Background(), artifact.PluginID, 20)
	if err != nil {
		t.Fatalf("RecentTraces() error = %v", err)
	}
	seen := map[string]bool{}
	for _, trace := range traces {
		seen[trace.TraceID+":"+trace.Status] = true
	}
	if !seen["trace-declared:200"] || !seen["trace-query:200"] || !seen["trace-admin-bypass:policy_denied"] || !seen["trace-sibling-bypass:policy_denied"] {
		t.Fatalf("traces = %+v, want declared success and bypass policy denial traces", traces)
	}
}
