// cmd/gateway/prometheus_test.go 验证 Prometheus 抓取格式、鉴权和监听生命周期。

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/gatewaymetrics"
)

func TestPrometheusHandlerExportsGatewayRuntimeAndProcessMetrics(t *testing.T) {
	metrics := gatewaymetrics.New()
	metrics.ConnectionStarted()
	handler := newPrometheusHandler(metrics, "")

	req := httptest.NewRequest(http.MethodGet, prometheusMetricsPath, nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain;") {
		t.Fatalf("Content-Type = %q, want Prometheus text format", got)
	}
	body := resp.Body.String()
	for _, metric := range []string{
		"mc_gateway_connections_total 1",
		"go_gc_duration_seconds",
		"process_start_time_seconds",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics body missing %q", metric)
		}
	}

	openMetricsReq := httptest.NewRequest(http.MethodGet, prometheusMetricsPath, nil)
	openMetricsReq.Header.Set("Accept", "application/openmetrics-text")
	openMetricsResp := httptest.NewRecorder()
	handler.ServeHTTP(openMetricsResp, openMetricsReq)
	if got := openMetricsResp.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/openmetrics-text;") {
		t.Fatalf("OpenMetrics Content-Type = %q", got)
	}
	if !strings.HasSuffix(openMetricsResp.Body.String(), "# EOF\n") {
		t.Fatalf("OpenMetrics body does not end with EOF marker")
	}
}

func TestPrometheusHandlerMethodsAndBearerToken(t *testing.T) {
	handler := newPrometheusHandler(gatewaymetrics.New(), "metrics-secret")
	tests := []struct {
		name       string
		method     string
		token      string
		wantStatus int
	}{
		{name: "missing token", method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "wrong token", method: http.MethodGet, token: "wrong", wantStatus: http.StatusUnauthorized},
		{name: "valid token", method: http.MethodGet, token: "metrics-secret", wantStatus: http.StatusOK},
		{name: "head", method: http.MethodHead, token: "metrics-secret", wantStatus: http.StatusOK},
		{name: "method not allowed", method: http.MethodPost, token: "metrics-secret", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, prometheusMetricsPath, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized && resp.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", resp.Header().Get("WWW-Authenticate"))
			}
			if tt.wantStatus == http.StatusMethodNotAllowed && resp.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow = %q, want GET, HEAD", resp.Header().Get("Allow"))
			}
		})
	}
}

func TestGatewayHTTPHandlerRegistersPrometheusOnlyInSharedMode(t *testing.T) {
	tests := []struct {
		mode       adminconfig.PrometheusMode
		wantStatus int
	}{
		{mode: adminconfig.PrometheusModeShared, wantStatus: http.StatusOK},
		{mode: adminconfig.PrometheusModeDedicated, wantStatus: http.StatusNotFound},
		{mode: adminconfig.PrometheusModeDisabled, wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			defer saveGatewayState(t)()
			adminStartup.PrometheusMode = tt.mode
			handler := newGatewayHTTPHandler()
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, prometheusMetricsPath, nil))
			if resp.Code != tt.wantStatus {
				t.Fatalf("GET /metrics status = %d, want %d", resp.Code, tt.wantStatus)
			}
		})
	}
}

func TestGatewayHTTPHandlerReturnsNotFoundOutsidePrometheusPath(t *testing.T) {
	defer saveGatewayState(t)()
	adminStartup.PrometheusMode = adminconfig.PrometheusModeShared
	handler := newGatewayHTTPHandler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/not-metrics", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("GET /not-metrics status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestGatewayHTTPHandlerAllowsSeparatePortWebSocketMetricsPath(t *testing.T) {
	defer saveGatewayState(t)()
	adminStartup.PrometheusMode = adminconfig.PrometheusModeShared
	config.Tcp.Enable = true
	config.Tcp.Port = adminStartup.TCPAdminPort
	config.WebSocket.Enable = true
	config.WebSocket.Port = adminStartup.TCPAdminPort + 1
	config.WebSocket.Path = prometheusMetricsPath

	handler := newGatewayHTTPHandler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, prometheusMetricsPath, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", resp.Code, http.StatusOK)
	}
}

func TestSharedPrometheusEndpointServesThroughTCPHTTPMux(t *testing.T) {
	defer saveGatewayState(t)()
	port := reserveGatewayTestTCPPort(t)
	adminStartup.TCPAdminPort = port
	adminStartup.PrometheusMode = adminconfig.PrometheusModeShared
	config.Tcp.Enable = true
	config.Tcp.Port = port

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runTcpWebPortReuse(ctx) }()

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   200 * time.Millisecond,
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + prometheusMetricsPath
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET shared /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			break
		}
		select {
		case serviceErr := <-done:
			t.Fatalf("shared listener stopped before scrape: %v", serviceErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out scraping shared /metrics: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runTcpWebPortReuse() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared listener did not stop after context cancellation")
	}
}

func TestRunPrometheusDedicatedServesAndStopsWithContext(t *testing.T) {
	defer saveGatewayState(t)()
	port := reserveGatewayTestTCPPort(t)
	adminStartup.PrometheusListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runPrometheus(ctx) }()
	waitForGatewayTestTCPListener(t, port, done)

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + adminStartup.PrometheusListenAddr + prometheusMetricsPath)
	if err != nil {
		t.Fatalf("GET dedicated /metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET dedicated /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runPrometheus() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runPrometheus() did not stop after context cancellation")
	}
}

func TestRunPrometheusDedicatedReturnsListenerError(t *testing.T) {
	defer saveGatewayState(t)()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()
	adminStartup.PrometheusListenAddr = listener.Addr().String()

	if err := runPrometheus(context.Background()); err == nil || !strings.Contains(err.Error(), adminStartup.PrometheusListenAddr) {
		t.Fatalf("runPrometheus() error = %v, want address-specific listener error", err)
	}
}

func TestRunEnabledServicesPropagatesPrometheusListenerError(t *testing.T) {
	defer saveGatewayState(t)()
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer metricsListener.Close()

	port := reserveGatewayTestTCPPort(t)
	config.Tcp.Enable = true
	config.Tcp.Port = port
	adminStartup.TCPAdminPort = port
	adminStartup.PrometheusMode = adminconfig.PrometheusModeDedicated
	adminStartup.PrometheusListenAddr = metricsListener.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = runEnabledServices(ctx)
	if err == nil || !strings.Contains(err.Error(), adminStartup.PrometheusListenAddr) {
		t.Fatalf("runEnabledServices() error = %v, want Prometheus listener error", err)
	}
}
