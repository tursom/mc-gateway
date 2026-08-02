// internal/gatewaymetrics/metrics_test.go 包含用于约束 metrics 行为的测试。

package gatewaymetrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCountersSnapshot(t *testing.T) {
	metrics := New()

	metrics.ConnectionStarted()
	metrics.ConnectionStarted()
	metrics.ConnectionFinished()
	metrics.TCPConnectionStarted()
	metrics.WebSocketConnectionStarted()
	metrics.RouteHit("play.example")
	metrics.RouteHit("play.example")
	metrics.RouteHit("dev.example")
	metrics.RouteMiss()
	metrics.UpstreamDialError()

	snapshot := metrics.Snapshot()
	if got := snapshot["total_connections"]; got != uint64(2) {
		t.Fatalf("total_connections = %#v, want 2", got)
	}
	if got := snapshot["active_connections"]; got != int64(1) {
		t.Fatalf("active_connections = %#v, want 1", got)
	}
	if got := snapshot["tcp_connections"]; got != uint64(1) {
		t.Fatalf("tcp_connections = %#v, want 1", got)
	}
	if got := snapshot["websocket_connections"]; got != uint64(1) {
		t.Fatalf("websocket_connections = %#v, want 1", got)
	}
	if got := snapshot["route_misses"]; got != uint64(1) {
		t.Fatalf("route_misses = %#v, want 1", got)
	}
	if got := snapshot["upstream_dial_errors"]; got != uint64(1) {
		t.Fatalf("upstream_dial_errors = %#v, want 1", got)
	}

	routeHits, ok := snapshot["route_hits"].(map[string]uint64)
	if !ok {
		t.Fatalf("route_hits = %#v, want map[string]uint64", snapshot["route_hits"])
	}
	if routeHits["play.example"] != 2 || routeHits["dev.example"] != 1 {
		t.Fatalf("route_hits = %#v", routeHits)
	}
}

func TestSnapshotCopiesRouteHits(t *testing.T) {
	metrics := New()
	metrics.RouteHit("play.example")

	snapshot := metrics.Snapshot()
	routeHits := snapshot["route_hits"].(map[string]uint64)
	routeHits["play.example"] = 100

	next := metrics.Snapshot()["route_hits"].(map[string]uint64)
	if next["play.example"] != 1 {
		t.Fatalf("route_hits was not copied, got %#v", next)
	}
}

func TestCountersCollectPrometheusMetrics(t *testing.T) {
	metrics := New()
	metrics.ConnectionStarted()
	metrics.ConnectionStarted()
	metrics.ConnectionFinished()
	metrics.TCPConnectionStarted()
	metrics.WebSocketConnectionStarted()
	metrics.RouteHit("play.example")
	metrics.RouteHit("attacker-controlled.example")
	metrics.RouteMiss()
	metrics.UpstreamDialError()

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	want := `
# HELP mc_gateway_active_connections Current number of active gateway connections.
# TYPE mc_gateway_active_connections gauge
mc_gateway_active_connections 1
# HELP mc_gateway_connections_total Total number of connections handled by the gateway.
# TYPE mc_gateway_connections_total counter
mc_gateway_connections_total 2
# HELP mc_gateway_route_hits_total Total number of successful gateway route resolutions.
# TYPE mc_gateway_route_hits_total counter
mc_gateway_route_hits_total 2
# HELP mc_gateway_route_misses_total Total number of failed gateway route resolutions.
# TYPE mc_gateway_route_misses_total counter
mc_gateway_route_misses_total 1
# HELP mc_gateway_tcp_connections_total Total number of Minecraft TCP connections accepted by the gateway.
# TYPE mc_gateway_tcp_connections_total counter
mc_gateway_tcp_connections_total 1
# HELP mc_gateway_upstream_dial_errors_total Total number of upstream dial errors.
# TYPE mc_gateway_upstream_dial_errors_total counter
mc_gateway_upstream_dial_errors_total 1
# HELP mc_gateway_websocket_connections_total Total number of WebSocket connections accepted by the gateway.
# TYPE mc_gateway_websocket_connections_total counter
mc_gateway_websocket_connections_total 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}
