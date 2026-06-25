package adminhttp

import (
	"io/fs"
	"net/http"
	"strings"
)

const (
	staticIndexFile = "admin_static/index.html"
	staticCSSFile   = "admin_static/app.css"
	staticJSFile    = "admin_static/app.js"
)

type GatewayHandlerOptions struct {
	AdminPath        string
	AdminAPIPrefix   string
	Assets           fs.FS
	APIHandler       http.HandlerFunc
	WebSocketEnabled bool
	WebSocketPath    string
	WebSocketHandler http.HandlerFunc
}

func NewGatewayHandler(opts GatewayHandlerOptions) http.Handler {
	mux := http.NewServeMux()
	registerAdminHandlers(mux, opts)

	if opts.WebSocketEnabled &&
		opts.WebSocketHandler != nil &&
		!WebSocketPathConflictsWithAdmin(opts.WebSocketPath, opts.AdminPath, opts.AdminAPIPrefix) {
		mux.HandleFunc(opts.WebSocketPath, opts.WebSocketHandler)
	}

	return mux
}

func WebSocketPathConflictsWithAdmin(webSocketPath, adminPath, adminAPIPrefix string) bool {
	adminRoot := strings.TrimRight(adminPath, "/")
	if webSocketPath == adminPath || webSocketPath == adminRoot {
		return true
	}
	if webSocketPath == adminAPIPrefix || strings.HasPrefix(adminAPIPrefix+"/", webSocketPath+"/") {
		return webSocketPath != "/"
	}
	return false
}

func registerAdminHandlers(mux *http.ServeMux, opts GatewayHandlerOptions) {
	adminRoot := strings.TrimRight(opts.AdminPath, "/")

	mux.HandleFunc(opts.AdminAPIPrefix, opts.APIHandler)
	mux.HandleFunc(opts.AdminAPIPrefix+"/", opts.APIHandler)
	if adminRoot != "" {
		mux.HandleFunc(adminRoot, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, opts.AdminPath, http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc(opts.AdminPath, func(w http.ResponseWriter, r *http.Request) {
		serveAdminStatic(w, r, opts)
	})
}

func serveAdminStatic(w http.ResponseWriter, r *http.Request, opts GatewayHandlerOptions) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if r.URL.Path == opts.AdminPath {
		serveAdminIndex(w, opts)
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, opts.AdminPath)
	switch rel {
	case "app.css":
		serveAdminFile(w, r, opts.Assets, staticCSSFile, "text/css; charset=utf-8")
	case "app.js":
		serveAdminFile(w, r, opts.Assets, staticJSFile, "application/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}

func serveAdminIndex(w http.ResponseWriter, opts GatewayHandlerOptions) {
	data, err := fs.ReadFile(opts.Assets, staticIndexFile)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	html := strings.ReplaceAll(string(data), "__ADMIN_API_PREFIX__", opts.AdminAPIPrefix)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

func serveAdminFile(w http.ResponseWriter, r *http.Request, assets fs.FS, name, contentType string) {
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
