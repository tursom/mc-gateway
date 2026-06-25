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
