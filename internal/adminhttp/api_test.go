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
		{name: "plugin sources", method: http.MethodGet, path: "/admin/api/plugin-sources", wantCall: "plugin_sources"},
		{name: "plugin builds", method: http.MethodGet, path: "/admin/api/plugin-builds", wantCall: "plugin_builds"},
		{name: "plugin build", method: http.MethodGet, path: "/admin/api/plugin-builds/7", wantCall: "plugin_build", wantSegment: "7"},
		{name: "plugin build retry", method: http.MethodPost, path: "/admin/api/plugin-builds/7/retry", wantCall: "plugin_build", wantSegment: "7/retry"},
		{name: "plugin gc", method: http.MethodGet, path: "/admin/api/plugin-gc", wantCall: "plugin_gc"},
		{name: "plugin operations gc", method: http.MethodGet, path: "/admin/api/plugin-operations-gc", wantCall: "plugin_operations_gc"},
		{name: "plugins list", method: http.MethodGet, path: "/admin/api/plugins", wantCall: "plugins_list"},
		{name: "plugin item", method: http.MethodPut, path: "/admin/api/plugins/upstream-rewrite", wantCall: "plugin_item", wantSegment: "upstream-rewrite"},
		{name: "plugin action", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/enable", wantCall: "plugin_action", wantSegment: "upstream-rewrite/enable"},
		{name: "plugin config", method: http.MethodPut, path: "/admin/api/plugins/upstream-rewrite/config", wantCall: "plugin_config", wantSegment: "upstream-rewrite/config"},
		{name: "plugin config dry-run", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/config/dry-run", wantCall: "plugin_config", wantSegment: "upstream-rewrite/config/dry-run"},
		{name: "plugin secrets", method: http.MethodGet, path: "/admin/api/plugins/upstream-rewrite/secrets", wantCall: "plugin_secrets", wantSegment: "upstream-rewrite/secrets"},
		{name: "plugin rollback artifact", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/rollback/artifact", wantCall: "plugin_rollback", wantSegment: "upstream-rewrite/rollback/artifact"},
		{name: "plugin governance", method: http.MethodGet, path: "/admin/api/plugins/upstream-rewrite/governance", wantCall: "plugin_governance", wantSegment: "upstream-rewrite/governance"},
		{name: "plugin governance review", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/governance/review", wantCall: "plugin_governance", wantSegment: "upstream-rewrite/governance/review"},
		{name: "plugin operations", method: http.MethodGet, path: "/admin/api/plugins/upstream-rewrite/operations", wantCall: "plugin_operations", wantSegment: "upstream-rewrite/operations"},
		{name: "plugin operations task trigger", method: http.MethodPost, path: "/admin/api/plugins/upstream-rewrite/operations/tasks/sync/trigger", wantCall: "plugin_operations", wantSegment: "upstream-rewrite/operations/tasks/sync/trigger"},
		{name: "plugin diagnostics", method: http.MethodGet, path: "/admin/api/plugins/upstream-rewrite/diagnostics", wantCall: "plugin_diagnostics", wantSegment: "upstream-rewrite"},
		{name: "plugin draining force close", method: http.MethodPost, path: "/admin/api/plugins/mc-auth-proxy/draining/force-close", wantCall: "plugin_draining", wantSegment: "mc-auth-proxy"},
		{name: "plugin dispatch", method: http.MethodGet, path: "/admin/api/plugins/dispatch-plan", wantCall: "plugin_dispatch"},
		{name: "plugin advisories", method: http.MethodGet, path: "/admin/api/plugin-advisories", wantCall: "plugin_advisories"},
		{name: "plugin service", method: http.MethodGet, path: "/admin/api/plugin-service", wantCall: "plugin_service"},
		{name: "plugin repositories", method: http.MethodGet, path: "/admin/api/plugin-repositories/imports", wantCall: "plugin_repositories"},
		{name: "plugin supply chain", method: http.MethodGet, path: "/admin/api/plugin-supply-chain", wantCall: "plugin_supply_chain"},
		{name: "plugin instrumentation", method: http.MethodGet, path: "/admin/api/plugin-instrumentation", wantCall: "plugin_instrumentation"},
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

				PluginArtifacts:       recordCall(&gotCall, "plugin_artifacts"),
				PluginArtifact:        recordSegmentCall(&gotCall, &gotSegment, "plugin_artifact"),
				PluginSources:         recordCall(&gotCall, "plugin_sources"),
				PluginBuilds:          recordCall(&gotCall, "plugin_builds"),
				PluginBuild:           recordSegmentCall(&gotCall, &gotSegment, "plugin_build"),
				PluginGC:              recordCall(&gotCall, "plugin_gc"),
				PluginOperationsGC:    recordCall(&gotCall, "plugin_operations_gc"),
				PluginsList:           recordCall(&gotCall, "plugins_list"),
				PluginItem:            recordSegmentCall(&gotCall, &gotSegment, "plugin_item"),
				PluginAction:          recordSegmentCall(&gotCall, &gotSegment, "plugin_action"),
				PluginConfig:          recordSegmentCall(&gotCall, &gotSegment, "plugin_config"),
				PluginSecrets:         recordSegmentCall(&gotCall, &gotSegment, "plugin_secrets"),
				PluginRollback:        recordSegmentCall(&gotCall, &gotSegment, "plugin_rollback"),
				PluginOperations:      recordSegmentCall(&gotCall, &gotSegment, "plugin_operations"),
				PluginDraining:        recordSegmentCall(&gotCall, &gotSegment, "plugin_draining"),
				PluginDispatch:        recordCall(&gotCall, "plugin_dispatch"),
				PluginGovernance:      recordSegmentCall(&gotCall, &gotSegment, "plugin_governance"),
				PluginAdvisories:      recordCall(&gotCall, "plugin_advisories"),
				PluginDiagnostics:     recordSegmentCall(&gotCall, &gotSegment, "plugin_diagnostics"),
				PluginService:         recordCall(&gotCall, "plugin_service"),
				PluginRepositories:    recordCall(&gotCall, "plugin_repositories"),
				PluginSupplyChain:     recordCall(&gotCall, "plugin_supply_chain"),
				PluginInstrumentation: recordCall(&gotCall, "plugin_instrumentation"),
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
