// cmd/gateway/prometheus.go 提供独立 registry 和 Prometheus 抓取 HTTP 处理器。

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/gatewaymetrics"
)

const prometheusMetricsPath = adminconfig.PrometheusMetricsPath

func runPrometheus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	address := adminStartup.PrometheusListenAddr
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for Prometheus metrics on %s: %w", address, err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.Handle(prometheusMetricsPath, newPrometheusHandler(gatewayMetrics, adminStartup.PrometheusBearerToken))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()

	log.Info().
		Str("mode", string(prometheusModeDedicated)).
		Str("address", address).
		Str("path", prometheusMetricsPath).
		Bool("authentication_enabled", adminStartup.PrometheusBearerToken != "").
		Msg("Listening for Prometheus metrics")
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve Prometheus metrics on %s: %w", address, err)
	}
	return nil
}

func newPrometheusHandler(metrics *gatewaymetrics.Counters, bearerToken string) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		metrics,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	scrapeHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if bearerToken != "" && !validPrometheusBearerToken(r.Header.Get("Authorization"), bearerToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		scrapeHandler.ServeHTTP(w, r)
	})
}

func validPrometheusBearerToken(authorization, expected string) bool {
	actualHash := sha256.Sum256([]byte(authorization))
	expectedHash := sha256.Sum256([]byte("Bearer " + expected))
	return subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) == 1
}
