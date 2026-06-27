// cmd/gateway/admin_metric_handlers.go 返回管理面板状态卡片使用的轻量运行时计数器。

package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
	"github.com/tursom/mc-gateway/internal/gatewaymetrics"
)

var gatewayMetrics = gatewaymetrics.New()

func handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, gatewayMetrics.Snapshot())
}
