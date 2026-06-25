package adminhttp

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultStaticDir = "cmd/gateway/admin_static"
	staticConfigFile = "config.js"
	staticIndexFile  = "index.html"
)

type GatewayHandlerOptions struct {
	AdminPath        string
	AdminAPIPrefix   string
	StaticDir        string
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
		serveAdminFile(w, r, opts, staticIndexFile)
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, opts.AdminPath)
	if rel == staticConfigFile {
		serveAdminConfig(w, opts)
		return
	}
	serveAdminFile(w, r, opts, rel)
}

func serveAdminConfig(w http.ResponseWriter, opts GatewayHandlerOptions) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`window.MCGatewayAdmin={"apiPrefix":` + strconv.Quote(opts.AdminAPIPrefix) + `};`))
}

func serveAdminFile(w http.ResponseWriter, r *http.Request, opts GatewayHandlerOptions, rel string) {
	name, ok := cleanStaticPath(rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(staticDir(opts), name)
	info, err := os.Stat(file)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, file)
}

func cleanStaticPath(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	cleaned := path.Clean("/" + rel)
	if cleaned == "/" || strings.HasPrefix(cleaned, "/../") {
		return "", false
	}
	name := strings.TrimPrefix(cleaned, "/")
	if name == staticConfigFile {
		return "", false
	}
	return name, true
}

func staticDir(opts GatewayHandlerOptions) string {
	if strings.TrimSpace(opts.StaticDir) != "" {
		return opts.StaticDir
	}
	if value := strings.TrimSpace(os.Getenv("MC_GATEWAY_ADMIN_STATIC_DIR")); value != "" {
		return value
	}
	if _, err := os.Stat(defaultStaticDir); err == nil {
		return defaultStaticDir
	}
	if _, err := os.Stat("admin_static"); err == nil {
		return "admin_static"
	}
	return defaultStaticDir
}
