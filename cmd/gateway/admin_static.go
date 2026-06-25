package main

import (
	"embed"
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

//go:embed admin_static/index.html admin_static/app.css admin_static/app.js
var adminStaticFS embed.FS

func newGatewayHTTPHandler() http.Handler {
	return adminhttp.NewGatewayHandler(adminhttp.GatewayHandlerOptions{
		AdminPath:        adminStartup.AdminPath,
		AdminAPIPrefix:   adminStartup.AdminAPIPrefix,
		Assets:           adminStaticFS,
		APIHandler:       newAdminAPIHandler(),
		WebSocketEnabled: config.WebSocket.Enable,
		WebSocketPath:    normalizedWebSocketPath(),
		WebSocketHandler: handleWebSocket,
	})
}
