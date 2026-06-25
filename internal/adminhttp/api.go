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
