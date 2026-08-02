// cmd/gateway/admin_static.go 嵌入构建后的管理前端，并通过网关 HTTP 处理器对外提供。

package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func newGatewayHTTPHandler() http.Handler {
	var prometheusHandler http.Handler
	if adminStartup.PrometheusMode == adminconfig.PrometheusModeShared {
		prometheusHandler = newPrometheusHandler(gatewayMetrics, adminStartup.PrometheusBearerToken)
	}
	return adminhttp.NewGatewayHandler(adminhttp.GatewayHandlerOptions{
		AdminPath:         adminStartup.AdminPath,
		AdminAPIPrefix:    adminStartup.AdminAPIPrefix,
		APIHandler:        newAdminAPIHandler(),
		WebSocketEnabled:  tcpWebPortReuseEnabled(),
		WebSocketPath:     normalizedWebSocketPath(),
		WebSocketHandler:  handleWebSocket,
		PrometheusHandler: prometheusHandler,
	})
}
