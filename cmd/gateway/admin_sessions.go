package main

import (
	"net/http"
	"strings"

	"github.com/tursom/mc-gateway/internal/adminhttp"
	"github.com/tursom/mc-gateway/internal/adminsession"
	"github.com/tursom/mc-gateway/internal/adminuser"
)

var adminSessionManager = adminsession.NewManager()

func createSession(username, role string) (adminsession.Session, error) {
	return adminSessionManager.Create(username, role, adminStartup.SessionTTL)
}

func getSession(token string) (adminsession.Session, bool) {
	return adminSessionManager.Get(token)
}

func deleteSession(token string) {
	adminSessionManager.Delete(token)
}

func removeSessionsForUser(username string) {
	adminSessionManager.RemoveUser(username)
}

func sessionFromRequest(r *http.Request) (adminsession.Session, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return adminsession.Session{}, false
	}
	return getSession(strings.TrimSpace(token))
}

func requireSession(w http.ResponseWriter, r *http.Request) (adminsession.Session, bool) {
	session, ok := sessionFromRequest(r)
	if !ok {
		adminhttp.WriteAPIError(w, http.StatusUnauthorized, "login required")
		return adminsession.Session{}, false
	}
	return session, true
}

func requireRole(w http.ResponseWriter, r *http.Request, role string) (adminsession.Session, bool) {
	session, ok := requireSession(w, r)
	if !ok {
		return adminsession.Session{}, false
	}
	if !adminuser.HasRole(session.Role, role) {
		adminhttp.WriteAPIError(w, http.StatusForbidden, "permission denied")
		return adminsession.Session{}, false
	}
	return session, true
}
