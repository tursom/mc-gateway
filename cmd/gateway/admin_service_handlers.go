// cmd/gateway/admin_service_handlers.go 提供监听服务配置接口，用于维护端口、启停状态和是否需要重启。

package main

import (
	"net/http"
	"strings"

	"github.com/tursom/mc-gateway/internal/adminhttp"
	"github.com/tursom/mc-gateway/internal/adminservice"
)

func handleAdminServicesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	services, err := listServiceConfigs(r.Context(), adminDB)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"services": services})
}

func handleAdminServiceItem(w http.ResponseWriter, r *http.Request, rawName string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}

	if strings.HasSuffix(rawName, "/restart") {
		name := strings.TrimSuffix(rawName, "/restart")
		if r.Method != http.MethodPost {
			adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "service_restart", "service", name, false, "restart is not implemented")
		adminhttp.WriteAPIError(w, http.StatusNotImplemented, "service restart is not implemented; restart the gateway process")
		return
	}

	name, err := adminhttp.PathSegment(rawName)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Method != http.MethodPut {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req adminhttp.ServiceRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.Port == 0 {
		req.Port = defaultServicePort(name)
	}

	err = updateServiceConfig(r.Context(), session.Username, name, enabled, req.Port, req.Options)
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "service_update", "service", name, false, err.Error())
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "service_update", "service", name, true, "service saved")
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": true})
}

func defaultServicePort(name string) int {
	return adminservice.DefaultPort(name, adminStartup.TCPAdminPort)
}
