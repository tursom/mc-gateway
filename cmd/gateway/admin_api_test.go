package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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
