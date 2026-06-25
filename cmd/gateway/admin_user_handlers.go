package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleAdmin); !ok {
		return
	}
	users, err := listUsers(r.Context())
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

func handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}

	var req adminhttp.CreateUserRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}
	err := createUser(r.Context(), session.Username, req.Username, req.Role, req.Password, req.Disabled)
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_create", "user", req.Username, false, err.Error())
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_create", "user", req.Username, true, "user created")
	adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func handleAdminUserItem(w http.ResponseWriter, r *http.Request, rawUsername string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	username, err := adminhttp.PathSegment(rawUsername)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req adminhttp.PatchUserRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		err := patchUser(r.Context(), session.Username, username, req.Role, req.Disabled, req.Password)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_patch", "user", username, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_patch", "user", username, true, "user updated")
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		err := deleteUser(r.Context(), username)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_delete", "user", username, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "user_delete", "user", username, true, "user deleted")
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminAuditLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleAdmin); !ok {
		return
	}
	logs, err := listAuditLogs(r.Context())
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"audit_logs": logs})
}
