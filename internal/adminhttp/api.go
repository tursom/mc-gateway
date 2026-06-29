// internal/adminhttp/api.go 构建隔离的进程内 Admin API 服务，供测试和包级消费者使用。

package adminhttp

import (
	"net/http"
	"strings"
)

type SegmentHandlerFunc func(http.ResponseWriter, *http.Request, string)

type APIHandlers struct {
	SetupStatus http.HandlerFunc
	Setup       http.HandlerFunc
	Login       http.HandlerFunc
	Logout      http.HandlerFunc
	Me          http.HandlerFunc
	Status      http.HandlerFunc

	RoutesList http.HandlerFunc
	RouteItem  SegmentHandlerFunc

	ServicesList http.HandlerFunc
	ServiceItem  SegmentHandlerFunc

	Metrics http.HandlerFunc

	UsersList   http.HandlerFunc
	UsersCreate http.HandlerFunc
	UserItem    SegmentHandlerFunc

	AuditLogs http.HandlerFunc

	PluginArtifacts       http.HandlerFunc
	PluginArtifact        SegmentHandlerFunc
	PluginSources         http.HandlerFunc
	PluginBuilds          http.HandlerFunc
	PluginBuild           SegmentHandlerFunc
	PluginGC              http.HandlerFunc
	PluginsList           http.HandlerFunc
	PluginItem            SegmentHandlerFunc
	PluginAction          SegmentHandlerFunc
	PluginConfig          SegmentHandlerFunc
	PluginSecrets         SegmentHandlerFunc
	PluginRollback        SegmentHandlerFunc
	PluginOperations      SegmentHandlerFunc
	PluginOperationsGC    http.HandlerFunc
	PluginDraining        SegmentHandlerFunc
	PluginDispatch        http.HandlerFunc
	PluginGovernance      SegmentHandlerFunc
	PluginAdvisories      http.HandlerFunc
	PluginVulnerabilities http.HandlerFunc
	PluginDiagnostics     SegmentHandlerFunc
	PluginFeatures        http.HandlerFunc
	PluginService         http.HandlerFunc
	PluginRepositories    http.HandlerFunc
	PluginSupplyChain     http.HandlerFunc
	PluginInstrumentation http.HandlerFunc
	PluginPromotions      http.HandlerFunc
}

func NewAPIHandler(prefix string, handlers APIHandlers) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "" {
			path = "/"
		}

		switch {
		case path == "/setup" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.SetupStatus)
		case path == "/setup" && r.Method == http.MethodPost:
			callHandler(w, r, handlers.Setup)
		case path == "/auth/login" && r.Method == http.MethodPost:
			callHandler(w, r, handlers.Login)
		case path == "/auth/logout" && r.Method == http.MethodPost:
			callHandler(w, r, handlers.Logout)
		case path == "/me" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.Me)
		case path == "/status" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.Status)
		case path == "/routes" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.RoutesList)
		case strings.HasPrefix(path, "/routes/"):
			callSegmentHandler(w, r, handlers.RouteItem, strings.TrimPrefix(path, "/routes/"))
		case path == "/services" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.ServicesList)
		case strings.HasPrefix(path, "/services/"):
			callSegmentHandler(w, r, handlers.ServiceItem, strings.TrimPrefix(path, "/services/"))
		case path == "/metrics" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.Metrics)
		case path == "/users" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.UsersList)
		case path == "/users" && r.Method == http.MethodPost:
			callHandler(w, r, handlers.UsersCreate)
		case strings.HasPrefix(path, "/users/"):
			callSegmentHandler(w, r, handlers.UserItem, strings.TrimPrefix(path, "/users/"))
		case path == "/audit-logs" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.AuditLogs)
		case path == "/plugin-artifacts" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginArtifacts)
		case strings.HasPrefix(path, "/plugin-artifacts/"):
			callSegmentHandler(w, r, handlers.PluginArtifact, strings.TrimPrefix(path, "/plugin-artifacts/"))
		case path == "/plugin-sources" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginSources)
		case path == "/plugin-builds" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginBuilds)
		case strings.HasPrefix(path, "/plugin-builds/"):
			callSegmentHandler(w, r, handlers.PluginBuild, strings.TrimPrefix(path, "/plugin-builds/"))
		case path == "/plugin-gc" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginGC)
		case path == "/plugin-operations-gc" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginOperationsGC)
		case path == "/plugins" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.PluginsList)
		case path == "/plugins/dispatch-plan" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginDispatch)
		case path == "/plugin-advisories" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginAdvisories)
		case path == "/plugin-vulnerabilities" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginVulnerabilities)
		case path == "/plugin-features" && r.Method == http.MethodGet:
			callHandler(w, r, handlers.PluginFeatures)
		case path == "/plugin-service" && (r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginService)
		case path == "/plugin-repositories/imports" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginRepositories)
		case path == "/plugin-supply-chain" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginSupplyChain)
		case path == "/plugin-instrumentation" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			callHandler(w, r, handlers.PluginInstrumentation)
		case path == "/plugin-promotions" && r.Method == http.MethodPost:
			callHandler(w, r, handlers.PluginPromotions)
		case strings.HasPrefix(path, "/plugins/"):
			pluginPath := strings.TrimPrefix(path, "/plugins/")
			if strings.Contains(pluginPath, "/governance/") || strings.HasSuffix(pluginPath, "/governance") {
				callSegmentHandler(w, r, handlers.PluginGovernance, pluginPath)
				return
			}
			if strings.Contains(pluginPath, "/operations/") || strings.HasSuffix(pluginPath, "/operations") {
				callSegmentHandler(w, r, handlers.PluginOperations, pluginPath)
				return
			}
			if strings.HasSuffix(pluginPath, "/diagnostics") {
				callSegmentHandler(w, r, handlers.PluginDiagnostics, strings.TrimSuffix(pluginPath, "/diagnostics"))
				return
			}
			if strings.HasSuffix(pluginPath, "/draining/force-close") {
				callSegmentHandler(w, r, handlers.PluginDraining, strings.TrimSuffix(pluginPath, "/draining/force-close"))
				return
			}
			if strings.Contains(pluginPath, "/rollback/") || strings.HasSuffix(pluginPath, "/rollback") {
				callSegmentHandler(w, r, handlers.PluginRollback, pluginPath)
				return
			}
			if strings.Contains(pluginPath, "/config/") || strings.HasSuffix(pluginPath, "/config") {
				callSegmentHandler(w, r, handlers.PluginConfig, pluginPath)
				return
			}
			if strings.HasSuffix(pluginPath, "/proxy-connections") {
				callSegmentHandler(w, r, handlers.PluginItem, pluginPath)
				return
			}
			if strings.Contains(pluginPath, "/secrets/") || strings.HasSuffix(pluginPath, "/secrets") {
				callSegmentHandler(w, r, handlers.PluginSecrets, pluginPath)
				return
			}
			if strings.Count(pluginPath, "/") == 1 {
				callSegmentHandler(w, r, handlers.PluginAction, pluginPath)
				return
			}
			callSegmentHandler(w, r, handlers.PluginItem, pluginPath)
		default:
			WriteAPIError(w, http.StatusNotFound, "not found")
		}
	}
}

func callHandler(w http.ResponseWriter, r *http.Request, handler http.HandlerFunc) {
	if handler == nil {
		WriteAPIError(w, http.StatusNotFound, "not found")
		return
	}
	handler(w, r)
}

func callSegmentHandler(w http.ResponseWriter, r *http.Request, handler SegmentHandlerFunc, segment string) {
	if handler == nil {
		WriteAPIError(w, http.StatusNotFound, "not found")
		return
	}
	handler(w, r, segment)
}
