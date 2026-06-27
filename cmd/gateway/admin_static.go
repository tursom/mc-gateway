// cmd/gateway/admin_static.go 嵌入构建后的管理前端，并通过网关 HTTP 处理器对外提供。

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
