// internal/gatewaymetrics/metrics.go 用原子计数器记录连接、路由和上游错误等管理状态指标。

package gatewaymetrics

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	totalConnectionsDesc = prometheus.NewDesc(
		"mc_gateway_connections_total",
		"Total number of connections handled by the gateway.",
		nil,
		nil,
	)
	activeConnectionsDesc = prometheus.NewDesc(
		"mc_gateway_active_connections",
		"Current number of active gateway connections.",
		nil,
		nil,
	)
	tcpConnectionsDesc = prometheus.NewDesc(
		"mc_gateway_tcp_connections_total",
		"Total number of Minecraft TCP connections accepted by the gateway.",
		nil,
		nil,
	)
	webSocketConnectionsDesc = prometheus.NewDesc(
		"mc_gateway_websocket_connections_total",
		"Total number of WebSocket connections accepted by the gateway.",
		nil,
		nil,
	)
	routeHitsDesc = prometheus.NewDesc(
		"mc_gateway_route_hits_total",
		"Total number of successful gateway route resolutions.",
		nil,
		nil,
	)
	routeMissesDesc = prometheus.NewDesc(
		"mc_gateway_route_misses_total",
		"Total number of failed gateway route resolutions.",
		nil,
		nil,
	)
	upstreamDialErrorsDesc = prometheus.NewDesc(
		"mc_gateway_upstream_dial_errors_total",
		"Total number of upstream dial errors.",
		nil,
		nil,
	)
)

type Counters struct {
	// 连接级计数使用原子值，避免转发热路径在每次连接开始/结束时争用锁。
	totalConnections  atomic.Uint64
	activeConnections atomic.Int64
	tcpConnections    atomic.Uint64
	webSocketConns    atomic.Uint64
	routeMisses       atomic.Uint64
	upstreamDialErrs  atomic.Uint64

	// routeHits 按 host 聚合，需要 map，因此用一把小锁保护。
	routeHitsMu sync.Mutex
	routeHits   map[string]uint64
}

func New() *Counters {
	return &Counters{
		routeHits: make(map[string]uint64),
	}
}

func (m *Counters) ConnectionStarted() {
	m.totalConnections.Add(1)
	m.activeConnections.Add(1)
}

func (m *Counters) ConnectionFinished() {
	m.activeConnections.Add(-1)
}

func (m *Counters) TCPConnectionStarted() {
	m.tcpConnections.Add(1)
}

func (m *Counters) WebSocketConnectionStarted() {
	m.webSocketConns.Add(1)
}

func (m *Counters) RouteHit(host string) {
	m.routeHitsMu.Lock()
	m.routeHits[host]++
	m.routeHitsMu.Unlock()
}

func (m *Counters) RouteMiss() {
	m.routeMisses.Add(1)
}

func (m *Counters) UpstreamDialError() {
	m.upstreamDialErrs.Add(1)
}

func (m *Counters) Snapshot() map[string]any {
	m.routeHitsMu.Lock()
	routeHits := make(map[string]uint64, len(m.routeHits))
	for host, count := range m.routeHits {
		routeHits[host] = count
	}
	m.routeHitsMu.Unlock()

	// 返回普通 map，方便 Admin API 直接 JSON 编码。
	return map[string]any{
		"total_connections":     m.totalConnections.Load(),
		"active_connections":    m.activeConnections.Load(),
		"tcp_connections":       m.tcpConnections.Load(),
		"websocket_connections": m.webSocketConns.Load(),
		"route_hits":            routeHits,
		"route_misses":          m.routeMisses.Load(),
		"upstream_dial_errors":  m.upstreamDialErrs.Load(),
	}
}

func (m *Counters) Describe(ch chan<- *prometheus.Desc) {
	ch <- totalConnectionsDesc
	ch <- activeConnectionsDesc
	ch <- tcpConnectionsDesc
	ch <- webSocketConnectionsDesc
	ch <- routeHitsDesc
	ch <- routeMissesDesc
	ch <- upstreamDialErrorsDesc
}

func (m *Counters) Collect(ch chan<- prometheus.Metric) {
	// Prometheus 只导出路由命中总数，避免把客户端可控的 host 变成高基数标签。
	m.routeHitsMu.Lock()
	var routeHits uint64
	for _, count := range m.routeHits {
		routeHits += count
	}
	m.routeHitsMu.Unlock()

	ch <- prometheus.MustNewConstMetric(totalConnectionsDesc, prometheus.CounterValue, float64(m.totalConnections.Load()))
	ch <- prometheus.MustNewConstMetric(activeConnectionsDesc, prometheus.GaugeValue, float64(m.activeConnections.Load()))
	ch <- prometheus.MustNewConstMetric(tcpConnectionsDesc, prometheus.CounterValue, float64(m.tcpConnections.Load()))
	ch <- prometheus.MustNewConstMetric(webSocketConnectionsDesc, prometheus.CounterValue, float64(m.webSocketConns.Load()))
	ch <- prometheus.MustNewConstMetric(routeHitsDesc, prometheus.CounterValue, float64(routeHits))
	ch <- prometheus.MustNewConstMetric(routeMissesDesc, prometheus.CounterValue, float64(m.routeMisses.Load()))
	ch <- prometheus.MustNewConstMetric(upstreamDialErrorsDesc, prometheus.CounterValue, float64(m.upstreamDialErrs.Load()))
}
