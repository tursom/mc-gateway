package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func newGatewayHTTPHandler() http.Handler {
	return adminhttp.NewGatewayHandler(adminhttp.GatewayHandlerOptions{
		AdminPath:        adminStartup.AdminPath,
		AdminAPIPrefix:   adminStartup.AdminAPIPrefix,
		APIHandler:       newAdminAPIHandler(),
		WebSocketEnabled: config.WebSocket.Enable,
		WebSocketPath:    normalizedWebSocketPath(),
		WebSocketHandler: handleWebSocket,
	})
}
