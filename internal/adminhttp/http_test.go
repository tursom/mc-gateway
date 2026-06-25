package adminhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeJSONRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api", strings.NewReader(`{"port":25565}`))
	resp := httptest.NewRecorder()

	var body map[string]any
	if !DecodeJSONRequest(resp, req, &body) {
		t.Fatal("DecodeJSONRequest() = false, want true")
	}
	port, ok := body["port"].(json.Number)
	if !ok {
		t.Fatalf("port = %#v, want json.Number", body["port"])
	}
	if port.String() != "25565" {
		t.Fatalf("port = %q, want 25565", port.String())
	}
}

func TestDecodeJSONRequestWritesError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api", strings.NewReader(`{`))
	resp := httptest.NewRecorder()

	var body map[string]any
	if DecodeJSONRequest(resp, req, &body) {
		t.Fatal("DecodeJSONRequest(invalid) = true, want false")
	}
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	var errorBody map[string]string
	if err := json.Unmarshal(resp.Body.Bytes(), &errorBody); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", resp.Body.String(), err)
	}
	if !strings.HasPrefix(errorBody["error"], "invalid JSON: ") {
		t.Fatalf("error = %q, want invalid JSON prefix", errorBody["error"])
	}
}

func TestWriteJSONAndAPIError(t *testing.T) {
	resp := httptest.NewRecorder()
	WriteJSON(resp, http.StatusCreated, map[string]any{"ok": true})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusCreated)
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if strings.TrimSpace(resp.Body.String()) != `{"ok":true}` {
		t.Fatalf("body = %q", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	WriteAPIError(resp, http.StatusForbidden, "permission denied")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("error status = %d, want %d", resp.Code, http.StatusForbidden)
	}
	if strings.TrimSpace(resp.Body.String()) != `{"error":"permission denied"}` {
		t.Fatalf("error body = %q", resp.Body.String())
	}
}

func TestPathSegment(t *testing.T) {
	tests := map[string]string{
		"play.example": "play.example",
		"play%2Etest":  "play.test",
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := PathSegment(raw)
			if err != nil {
				t.Fatalf("PathSegment(%q) error = %v", raw, err)
			}
			if got != want {
				t.Fatalf("PathSegment(%q) = %q, want %q", raw, got, want)
			}
		})
	}

	for _, raw := range []string{"", "a/b", "%2F", "%zz"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			if _, err := PathSegment(raw); err == nil {
				t.Fatalf("PathSegment(%q) error = nil, want error", raw)
			}
		})
	}
}

func TestRequestSourceIP(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{"127.0.0.1:1234", "127.0.0.1"},
		{"[::1]:1234", "::1"},
		{"unix-socket", "unix-socket"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if got := RequestSourceIP(req); got != tt.want {
				t.Fatalf("RequestSourceIP(%q) = %q, want %q", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

func TestNewGatewayHandlerServesAdminAndAPI(t *testing.T) {
	staticDir := writeTestAdminStatic(t, map[string]string{
		"index.html": "<html><script src=\"config.js\"></script></html>",
		"app.css":    "body{color:red}",
		"js/main.js": `console.log("admin")`,
	})
	apiCalled := false
	handler := NewGatewayHandler(GatewayHandlerOptions{
		AdminPath:      "/ops/",
		AdminAPIPrefix: "/ops/api",
		StaticDir:      staticDir,
		APIHandler: func(w http.ResponseWriter, r *http.Request) {
			apiCalled = true
			w.WriteHeader(http.StatusNoContent)
		},
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ops", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("redirect status = %d, want %d", resp.Code, http.StatusMovedPermanently)
	}
	if got := resp.Header().Get("Location"); got != "/ops/" {
		t.Fatalf("redirect location = %q, want /ops/", got)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin page status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `script src="config.js"`) {
		t.Fatalf("admin page = %q, want static index", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/app.css", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || strings.TrimSpace(resp.Body.String()) != `body{color:red}` {
		t.Fatalf("css response status=%d body=%q", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/js/main.js", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `console.log("admin")`) {
		t.Fatalf("js response status=%d body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("js Cache-Control = %q, want no-store", got)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/js/", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("js directory status=%d, want %d", resp.Code, http.StatusNotFound)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/config.js", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || strings.TrimSpace(resp.Body.String()) != `window.MCGatewayAdmin={"apiPrefix":"/ops/api"};` {
		t.Fatalf("config response status=%d body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("config Cache-Control = %q, want no-store", got)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/ops/", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST admin status = %d, want %d", resp.Code, http.StatusMethodNotAllowed)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ops/api/setup", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("api status = %d, want %d", resp.Code, http.StatusNoContent)
	}
	if !apiCalled {
		t.Fatal("API handler was not called")
	}
}

func TestNewGatewayHandlerRegistersWebSocketWhenPathDoesNotConflict(t *testing.T) {
	staticDir := writeTestAdminStatic(t, map[string]string{"index.html": ""})
	handler := NewGatewayHandler(GatewayHandlerOptions{
		AdminPath:        "/admin/",
		AdminAPIPrefix:   "/admin/api",
		StaticDir:        staticDir,
		APIHandler:       func(w http.ResponseWriter, r *http.Request) {},
		WebSocketEnabled: true,
		WebSocketPath:    "/ws",
		WebSocketHandler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		},
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("websocket path status = %d, want %d", resp.Code, http.StatusAccepted)
	}
}

func writeTestAdminStatic(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}
	return dir
}

func TestWebSocketPathConflictsWithAdmin(t *testing.T) {
	tests := []struct {
		name          string
		webSocketPath string
		want          bool
	}{
		{name: "admin path", webSocketPath: "/admin/", want: true},
		{name: "admin root", webSocketPath: "/admin", want: true},
		{name: "api prefix", webSocketPath: "/admin/api", want: true},
		{name: "api child", webSocketPath: "/admin/api/ws", want: false},
		{name: "root", webSocketPath: "/", want: false},
		{name: "separate path", webSocketPath: "/ws", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WebSocketPathConflictsWithAdmin(tt.webSocketPath, "/admin/", "/admin/api")
			if got != tt.want {
				t.Fatalf("WebSocketPathConflictsWithAdmin(%q) = %v, want %v", tt.webSocketPath, got, tt.want)
			}
		})
	}
}
