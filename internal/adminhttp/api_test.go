package adminhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAPIHandlerRoutesRequests(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		wantCall    string
		wantSegment string
	}{
		{name: "setup status", method: http.MethodGet, path: "/admin/api/setup", wantCall: "setup_status"},
		{name: "setup", method: http.MethodPost, path: "/admin/api/setup", wantCall: "setup"},
		{name: "login", method: http.MethodPost, path: "/admin/api/auth/login", wantCall: "login"},
		{name: "logout", method: http.MethodPost, path: "/admin/api/auth/logout", wantCall: "logout"},
		{name: "me", method: http.MethodGet, path: "/admin/api/me", wantCall: "me"},
		{name: "status", method: http.MethodGet, path: "/admin/api/status", wantCall: "status"},
		{name: "routes list", method: http.MethodGet, path: "/admin/api/routes", wantCall: "routes_list"},
		{name: "route item", method: http.MethodPut, path: "/admin/api/routes/play.example", wantCall: "route_item", wantSegment: "play.example"},
		{name: "services list", method: http.MethodGet, path: "/admin/api/services", wantCall: "services_list"},
		{name: "service restart", method: http.MethodPost, path: "/admin/api/services/kcp/restart", wantCall: "service_item", wantSegment: "kcp/restart"},
		{name: "metrics", method: http.MethodGet, path: "/admin/api/metrics", wantCall: "metrics"},
		{name: "users list", method: http.MethodGet, path: "/admin/api/users", wantCall: "users_list"},
		{name: "users create", method: http.MethodPost, path: "/admin/api/users", wantCall: "users_create"},
		{name: "user item", method: http.MethodPatch, path: "/admin/api/users/member", wantCall: "user_item", wantSegment: "member"},
		{name: "audit logs", method: http.MethodGet, path: "/admin/api/audit-logs", wantCall: "audit_logs"},
		{name: "plugin artifacts", method: http.MethodGet, path: "/admin/api/plugin-artifacts", wantCall: "plugin_artifacts"},
		{name: "plugin artifact", method: http.MethodGet, path: "/admin/api/plugin-artifacts/abc", wantCall: "plugin_artifact", wantSegment: "abc"},
		{name: "plugins list", method: http.MethodGet, path: "/admin/api/plugins", wantCall: "plugins_list"},
		{name: "plugin item", method: http.MethodPut, path: "/admin/api/plugins/upstream-rewrite", wantCall: "plugin_item", wantSegment: "upstream-rewrite"},
		{name: "plugin action", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/enable", wantCall: "plugin_action", wantSegment: "upstream-rewrite/enable"},
		{name: "plugin draining force close", method: http.MethodPost, path: "/admin/api/plugins/mc-auth-proxy/draining/force-close", wantCall: "plugin_draining", wantSegment: "mc-auth-proxy"},
		{name: "plugin dispatch", method: http.MethodGet, path: "/admin/api/plugins/dispatch-plan", wantCall: "plugin_dispatch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotCall, gotSegment string
			handler := NewAPIHandler("/admin/api", APIHandlers{
				SetupStatus: recordCall(&gotCall, "setup_status"),
				Setup:       recordCall(&gotCall, "setup"),
				Login:       recordCall(&gotCall, "login"),
				Logout:      recordCall(&gotCall, "logout"),
				Me:          recordCall(&gotCall, "me"),
				Status:      recordCall(&gotCall, "status"),

				RoutesList: recordCall(&gotCall, "routes_list"),
				RouteItem:  recordSegmentCall(&gotCall, &gotSegment, "route_item"),

				ServicesList: recordCall(&gotCall, "services_list"),
				ServiceItem:  recordSegmentCall(&gotCall, &gotSegment, "service_item"),

				Metrics: recordCall(&gotCall, "metrics"),

				UsersList:   recordCall(&gotCall, "users_list"),
				UsersCreate: recordCall(&gotCall, "users_create"),
				UserItem:    recordSegmentCall(&gotCall, &gotSegment, "user_item"),

				AuditLogs: recordCall(&gotCall, "audit_logs"),

				PluginArtifacts: recordCall(&gotCall, "plugin_artifacts"),
				PluginArtifact:  recordSegmentCall(&gotCall, &gotSegment, "plugin_artifact"),
				PluginsList:     recordCall(&gotCall, "plugins_list"),
				PluginItem:      recordSegmentCall(&gotCall, &gotSegment, "plugin_item"),
				PluginAction:    recordSegmentCall(&gotCall, &gotSegment, "plugin_action"),
				PluginDraining:  recordSegmentCall(&gotCall, &gotSegment, "plugin_draining"),
				PluginDispatch:  recordCall(&gotCall, "plugin_dispatch"),
			})

			resp := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusNoContent, resp.Body.String())
			}
			if gotCall != tt.wantCall {
				t.Fatalf("call = %q, want %q", gotCall, tt.wantCall)
			}
			if gotSegment != tt.wantSegment {
				t.Fatalf("segment = %q, want %q", gotSegment, tt.wantSegment)
			}
			if got := resp.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestNewAPIHandlerWritesNotFound(t *testing.T) {
	handler := NewAPIHandler("/admin/api", APIHandlers{})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/auth/login", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("wrong method status = %d, want %d", resp.Code, http.StatusNotFound)
	}
	if strings.TrimSpace(resp.Body.String()) != `{"error":"not found"}` {
		t.Fatalf("wrong method body = %q", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/missing", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func recordCall(got *string, call string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*got = call
		w.WriteHeader(http.StatusNoContent)
	}
}

func recordSegmentCall(gotCall, gotSegment *string, call string) SegmentHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, segment string) {
		*gotCall = call
		*gotSegment = segment
		w.WriteHeader(http.StatusNoContent)
	}
}
