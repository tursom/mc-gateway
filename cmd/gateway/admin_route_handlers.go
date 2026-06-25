package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func handleAdminRoutesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleGuest); !ok {
		return
	}
	routes, err := listRoutes(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"routes": routes})
}

func handleAdminRouteItem(w http.ResponseWriter, r *http.Request, rawHost string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	host, err := adminhttp.PathSegment(rawHost)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req adminhttp.RouteRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		err := upsertRoute(r.Context(), session.Username, host, req.Upstream, enabled, req.Note)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "route_upsert", "route", host, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "route_upsert", "route", host, true, "route saved")
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		err := deleteRoute(r.Context(), session.Username, host)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "route_delete", "route", host, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "route_delete", "route", host, true, "route deleted")
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
