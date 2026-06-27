// cmd/gateway/admin_auth_handlers.go 处理初始化、登录、登出和当前会话查询等嵌入式管理端认证接口。

package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tursom/mc-gateway/internal/adminhttp"
	"github.com/tursom/mc-gateway/internal/adminuser"
)

func handleAdminSetupStatus(w http.ResponseWriter, r *http.Request) {
	empty, err := usersTableEmpty(r.Context(), adminDB)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"required": empty})
}

func handleAdminSetup(w http.ResponseWriter, r *http.Request) {
	var req adminhttp.SetupRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		req.Username = "admin"
	}

	err := createInitialAdmin(r.Context(), req.Username, req.Password)
	if err != nil {
		recordAudit(r.Context(), "setup", adminhttp.RequestSourceIP(r), "setup", "user", req.Username, false, err.Error())
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already") {
			status = http.StatusConflict
		}
		adminhttp.WriteAPIError(w, status, err.Error())
		return
	}
	recordAudit(r.Context(), "setup", adminhttp.RequestSourceIP(r), "setup", "user", req.Username, true, "created initial admin")
	adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req adminhttp.LoginRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}

	user, err := authenticateUser(r.Context(), req.Username, req.Password)
	if err != nil {
		recordAudit(r.Context(), req.Username, adminhttp.RequestSourceIP(r), "login", "user", req.Username, false, err.Error())
		adminhttp.WriteAPIError(w, http.StatusUnauthorized, err.Error())
		return
	}

	session, err := createSession(user.Username, user.Role)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	recordAudit(r.Context(), user.Username, adminhttp.RequestSourceIP(r), "login", "user", user.Username, true, "login success")
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"token":      session.Token,
		"expires_at": session.ExpiresAt.Unix(),
		"user":       user,
	})
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := requireSession(w, r)
	if !ok {
		return
	}
	deleteSession(session.Token)
	recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "logout", "session", session.Username, true, "logout success")
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleAdminMe(w http.ResponseWriter, r *http.Request) {
	session, ok := requireSession(w, r)
	if !ok {
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"username":    session.Username,
		"role":        session.Role,
		"expires_at":  session.ExpiresAt.Unix(),
		"permissions": adminuser.Permissions(session.Role),
	})
}

func handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	services, err := listServiceConfigs(r.Context(), adminDB)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"pid":              os.Getpid(),
		"uptime_seconds":   int64(time.Since(processStartAt).Seconds()),
		"db_path":          adminDBPath,
		"tcp_admin_port":   adminStartup.TCPAdminPort,
		"admin_path":       adminStartup.AdminPath,
		"admin_api_prefix": adminStartup.AdminAPIPrefix,
		"services":         services,
	})
}
