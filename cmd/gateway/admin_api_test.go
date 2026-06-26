package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

func TestAdminSetupLoginAndPermissions(t *testing.T) {
	handler := newAdminTestHandler(t)

	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/setup", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET setup status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := adminTestJSON(t, resp)["required"]; got != true {
		t.Fatalf("setup required = %v, want true", got)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin",
		"password": "secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST setup status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin2",
		"password": "secret",
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("repeat setup status = %d, want %d", resp.Code, http.StatusConflict)
	}

	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "guest",
		"role":     "guest",
		"password": "guest-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create guest status = %d, body=%s", resp.Code, resp.Body.String())
	}

	guestToken := adminTestLogin(t, handler, "guest", "guest-secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/routes", guestToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("guest routes status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/routes/play.example", guestToken, map[string]any{
		"upstream": "127.0.0.1:25565",
		"enabled":  true,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("guest route write status = %d, want %d", resp.Code, http.StatusForbidden)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/users", guestToken, nil)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("guest users status = %d, want %d", resp.Code, http.StatusForbidden)
	}
}

func TestAdminRoutesRefreshSnapshot(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPut, "/admin/api/routes/play.example", token, map[string]any{
		"upstream": "127.0.0.1:25565",
		"enabled":  true,
		"note":     "primary",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("route upsert status = %d, body=%s", resp.Code, resp.Body.String())
	}

	upstream, ok := lookupRoute("play.example")
	if !ok || upstream != "127.0.0.1:25565" {
		t.Fatalf("lookupRoute() = %q, %v; want route", upstream, ok)
	}

	resp = adminTestRequest(t, handler, http.MethodDelete, "/admin/api/routes/play.example", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("route delete status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if upstream, ok := lookupRoute("play.example"); ok || upstream != "" {
		t.Fatalf("lookupRoute() after delete = %q, %v; want miss", upstream, ok)
	}
}

func TestAdminCustomPathAndAPIPrefixFromEnv(t *testing.T) {
	t.Cleanup(saveGatewayState(t))

	t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
	t.Setenv(adminEnvPath, "/ops")
	t.Setenv(adminEnvAPIPrefix, "/ops/api")
	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	handler := newGatewayHTTPHandler()

	resp := adminTestRequest(t, handler, http.MethodGet, "/ops", "", nil)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("admin path redirect status = %d, want %d", resp.Code, http.StatusMovedPermanently)
	}
	if got := resp.Header().Get("Location"); got != "/ops/" {
		t.Fatalf("admin path redirect location = %q, want /ops/", got)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom admin page status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `script src="config.js"`) {
		t.Fatalf("custom admin page does not reference runtime config: %s", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/config.js", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom admin config status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.TrimSpace(resp.Body.String()) != `window.MCGatewayAdmin={"apiPrefix":"/ops/api"};` {
		t.Fatalf("custom admin config body = %q", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/api/setup", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom setup status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/setup", "", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("default setup status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestAdminServiceUpdateMarksRestartRequired(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPut, "/admin/api/services/kcp", token, map[string]any{
		"enabled": true,
		"port":    25570,
		"options": map[string]any{
			"data_shards":   12,
			"parity_shards": 4,
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("service update status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/services", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("services status = %d, body=%s", resp.Code, resp.Body.String())
	}
	body := adminTestJSON(t, resp)
	services := body["services"].([]any)
	var found map[string]any
	for _, item := range services {
		service := item.(map[string]any)
		if service["name"] == "kcp" {
			found = service
			break
		}
	}
	if found == nil {
		t.Fatal("kcp service not found")
	}
	if found["enabled"] != true || found["restart_required"] != true || found["running"] != false {
		t.Fatalf("kcp service = %#v, want enabled restart_required and not running", found)
	}
}

func TestAdminUserPatchInvalidatesExistingSession(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	adminToken := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}

	memberToken := adminTestLogin(t, handler, "member", "member-secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member status before patch = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPatch, "/admin/api/users/member", adminToken, map[string]any{
		"role": "guest",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("patch member status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", memberToken, nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("old member token status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestAdminPluginPhase4API(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	artifact := uploadGatewayPhase4Artifact(t, "phase4-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "phase4-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{"token":"old","host":"a"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member plugins list status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/secrets", memberToken, map[string]any{
		"name":  "api_token",
		"value": "member-secret-value",
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member secret write status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/secrets", adminToken, map[string]any{
		"artifact_id":     artifact.ID,
		"name":            "api_token",
		"value":           "super-secret-value",
		"reload_required": true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("secret write status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "super-secret-value") {
		t.Fatalf("secret response leaked value: %s", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/config/dry-run", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"new","host":"b"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config dry-run status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "new") || strings.Contains(resp.Body.String(), "old") {
		t.Fatalf("dry-run leaked sensitive value: %s", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/plugins/phase4-plugin/config", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"new","host":"b"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config update status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase4-plugin/config/snapshots", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, body=%s", resp.Code, resp.Body.String())
	}
	snapshotID := firstSnapshotID(t, resp)
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase4-plugin/config/snapshots/"+strconv.FormatInt(snapshotID, 10)+"/diff", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("snapshot diff status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "new") || strings.Contains(resp.Body.String(), "old") {
		t.Fatalf("snapshot diff leaked sensitive value: %s", resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/rollback/config", adminToken, map[string]any{
		"snapshot_id": snapshotID,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config rollback status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "super-secret-value") {
		t.Fatalf("audit leaked secret value: %s", resp.Body.String())
	}
}

func TestAdminPluginPhase5GovernanceAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	artifact := uploadGatewayPhase5ProtocolProxyArtifact(t, "phase5-proxy")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "phase5-proxy", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase5-proxy/governance?profile=prod", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member governance status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "review_required") {
		t.Fatalf("governance status body = %s, want review_required", resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/review", memberToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member review write status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/review", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"decision":    pluginmanager.ReviewDecisionApproved,
		"notes":       "phase 5 approval",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("admin review write status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/preflight", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"action":      pluginmanager.GovernanceActionEnable,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/benchmark", adminToken, map[string]any{
		"artifact_id":           artifact.ID,
		"profile":               pluginmanager.PolicyProfileProd,
		"benchmark_profile":     "release",
		"p95_ms":                10,
		"p99_ms":                20,
		"baseline_diff":         0.25,
		"active_proxy_capacity": 100,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("benchmark status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/override", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"action":      pluginmanager.GovernanceActionEnable,
		"reason":      "accepted warning for rollout",
		"ttl_seconds": 3600,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("override status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-advisories", adminToken, map[string]any{
		"advisory_id":     "MCG-2026-ADMIN",
		"status":          pluginmanager.AdvisoryStatusRevoked,
		"action":          pluginmanager.AdvisoryActionRevoke,
		"artifact_sha256": artifact.SHA256,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("advisory status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-advisories?plugin_id=phase5-proxy", memberToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "MCG-2026-ADMIN") {
		t.Fatalf("member advisory read status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
	for _, action := range []string{"plugin_governance_review", "plugin_governance_override", "plugin_governance_advisory"} {
		if !strings.Contains(resp.Body.String(), action) {
			t.Fatalf("audit body missing %s: %s", action, resp.Body.String())
		}
	}
}

func uploadGatewayPhase4Artifact(t *testing.T, pluginID string) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, pluginID), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	manifest.Secrets = []pluginmanager.SecretSpec{{
		Name:     "api_token",
		Required: false,
		Type:     "api_token",
		Rotation: pluginmanager.SecretRotation{
			Reload: "reload_required",
		},
	}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes " + pluginID),
		}),
		FileName: pluginID + ".mcgp",
		Actor:    "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func uploadGatewayPhase5ProtocolProxyArtifact(t *testing.T, pluginID string) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifestWithCapabilities(t, pluginID, gatewayProtocolProxyCapabilities()), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.RuntimeLimits = pluginmanager.RuntimeLimits{HandlerTimeoutMS: 3000}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes " + pluginID),
		}),
		FileName: pluginID + ".mcgp",
		Actor:    "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func newAdminTestHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(saveGatewayState(t))

	t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	return newGatewayHTTPHandler()
}

func newAdminTestHandlerWithAdmin(t *testing.T) http.Handler {
	t.Helper()
	handler := newAdminTestHandler(t)
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin",
		"password": "secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body=%s", resp.Code, resp.Body.String())
	}
	return handler
}

func adminTestLogin(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/auth/login", "", map[string]any{
		"username": username,
		"password": password,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", resp.Code, resp.Body.String())
	}
	body := adminTestJSON(t, resp)
	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Fatalf("login token = %#v", body["token"])
	}
	return token
}

func adminTestRequest(t *testing.T, handler http.Handler, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func adminTestJSON(t *testing.T, resp *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", resp.Body.String(), err)
	}
	return body
}

func firstSnapshotID(t *testing.T, resp *httptest.ResponseRecorder) int64 {
	t.Helper()
	body := adminTestJSON(t, resp)
	snapshots, ok := body["snapshots"].([]any)
	if !ok || len(snapshots) == 0 {
		t.Fatalf("snapshots = %#v, want at least one", body["snapshots"])
	}
	first, ok := snapshots[0].(map[string]any)
	if !ok {
		t.Fatalf("first snapshot = %#v", snapshots[0])
	}
	id, ok := first["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("snapshot id = %#v", first["id"])
	}
	return int64(id)
}
