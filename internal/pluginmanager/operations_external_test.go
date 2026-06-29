package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestManagerExternalClientOperationsAcceptanceRedactsErrors(t *testing.T) {
	var gateway *Gateway
	manager := newManagerForTest(t, &fakeAdapter{init: func(g *Gateway) {
		gateway = g
	}})
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch r.URL.Path {
		case "/flaky":
			http.Error(w, "session response token=server-secret full packet payload", http.StatusBadGateway)
		case "/slow":
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte("late session response token=server-secret"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	artifact := uploadTestArtifactWithManifest(t, manager, "external-plugin", func(manifest *Manifest) {
		manifest.ExternalDeps = []ExternalSpec{{
			Name:       "session",
			Endpoint:   server.URL + "/flaky?token=server-secret",
			Purpose:    "auth",
			Required:   true,
			Timeout:    "50ms",
			Retry:      1,
			FailPolicy: ExternalFailPolicyClosed,
		}, {
			Name:       "slow",
			Endpoint:   server.URL + "/slow",
			Purpose:    "audit",
			Timeout:    "50ms",
			FailPolicy: ExternalFailPolicyOpen,
		}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "external-plugin", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "external-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	if _, err := gateway.ExternalClient("session").DoHTTP(context.Background(), api.ExternalRequest{}); err == nil {
		t.Fatal("ExternalClient.DoHTTP(session) error = nil, want upstream failure")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want one retry after initial failure", attempts)
	}
	if _, err := gateway.ExternalClient("slow").DoHTTP(context.Background(), api.ExternalRequest{}); err == nil {
		t.Fatal("ExternalClient.DoHTTP(slow) error = nil, want timeout")
	}
	for i := 0; i < 2; i++ {
		_, _ = gateway.ExternalClient("session").DoHTTP(context.Background(), api.ExternalRequest{})
	}
	if _, err := gateway.ExternalClient("session").DoHTTP(context.Background(), api.ExternalRequest{}); err == nil || !strings.Contains(err.Error(), "circuit is open") {
		t.Fatalf("ExternalClient.DoHTTP(session after failures) error = %v, want open circuit", err)
	}

	health, err := manager.HealthCheckExternalDependency(context.Background(), "admin", "external-plugin", "session")
	if err != nil {
		t.Fatalf("HealthCheckExternalDependency() error = %v", err)
	}
	if health.OK || health.Error == "" {
		t.Fatalf("health = %+v, want failed health with summarized error", health)
	}
	snap, err := manager.OperationsSnapshot(context.Background(), "external-plugin")
	if err != nil {
		t.Fatalf("OperationsSnapshot() error = %v", err)
	}
	if len(snap.ExternalDependencies) != 2 {
		t.Fatalf("external dependencies = %+v, want two summaries", snap.ExternalDependencies)
	}
	byName := make(map[string]ExternalDependencySummary)
	for _, summary := range snap.ExternalDependencies {
		byName[summary.Name] = summary
	}
	session := byName["session"]
	if session.FailPolicy != ExternalFailPolicyClosed || session.CircuitState != circuitOpen || session.Errors == 0 || session.RecentError == "" {
		t.Fatalf("session summary = %+v, want fail-closed open circuit with recent error", session)
	}
	slow := byName["slow"]
	if slow.FailPolicy != ExternalFailPolicyOpen || slow.LastStatus == "" || slow.Errors == 0 {
		t.Fatalf("slow summary = %+v, want fail-open timeout summary", slow)
	}
	payload, _ := json.Marshal(map[string]any{
		"health":  health,
		"session": session,
		"slow":    slow,
	})
	for _, forbidden := range []string{"server-secret", "token=server-secret", "session response", "full packet payload"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("external operation summary leaked %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(fmt.Sprint(session.RecentError, health.Error), "[REDACTED]") {
		t.Fatalf("session recent error = %q health error = %q, want redaction marker", session.RecentError, health.Error)
	}
	if session.NetworkEnforced || !strings.Contains(session.NetworkBoundary, "not provide strong network isolation") {
		t.Fatalf("session network boundary = %+v, want native plugin observation boundary", session)
	}
}

func TestOperationsExporterBoundaryFailsOpenAndValidatesOutput(t *testing.T) {
	var gateway *Gateway
	manager := newManagerForTest(t, &fakeAdapter{
		init: func(g *Gateway) {
			gateway = g
		},
		handlers: map[string]api.UpstreamConnectHandler{
			"plugin-a": func(req api.UpstreamConnectRequest) (net.Conn, error) {
				_ = gateway.EmitEvent(req.Context, "auth.success", map[string]string{"result": "ok"})
				_ = gateway.ObserveMetric(req.Context, "auth.attempts", 1, map[string]string{"result": "ok"})
				return newMemoryConn(), nil
			},
		},
	})
	artifact := uploadTestArtifactWithManifest(t, manager, "plugin-a", func(manifest *Manifest) {
		manifest.Events = []EventSpec{{Name: "auth.success", Fields: []string{"result"}}}
		manifest.CustomMetrics = []MetricSpec{{Name: "auth.attempts", Type: "counter", Labels: []string{"result"}}}
	})
	if _, err := manager.SetDesired(context.Background(), "admin", "plugin-a", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := manager.Enable(context.Background(), "admin", "plugin-a"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	failing := &recordingOperationsExporterSink{err: errors.New("prometheus push failed token=secret")}
	manager.operations.ConfigureExporter(OperationsExporterConfig{Type: OperationsExporterPrometheus, Enabled: true, Endpoint: "http://user:pass@example.test/metrics?token=secret"}, failing)
	if _, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"}); err != nil {
		t.Fatalf("ConnectUpstream() with failing exporter error = %v", err)
	}
	waitForPluginManagerTest(t, func() bool {
		snap, err := manager.OperationsSnapshot(context.Background(), "plugin-a")
		if err != nil {
			return false
		}
		for _, exporter := range snap.Exporters {
			if exporter.Type == OperationsExporterPrometheus {
				return exporter.Degraded && exporter.FailureCount > 0 && strings.Contains(exporter.LastError, "[REDACTED]") && !strings.Contains(exporter.Endpoint, "secret")
			}
		}
		return false
	})
	if _, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example", Upstream: "backend"}); err != nil {
		t.Fatalf("ConnectUpstream() after degraded exporter error = %v", err)
	}

	success := &recordingOperationsExporterSink{}
	manager.operations.ConfigureExporter(OperationsExporterConfig{Type: OperationsExporterOTel, Enabled: true}, success)
	snap, err := manager.OperationsSnapshot(context.Background(), "plugin-a")
	if err != nil {
		t.Fatalf("OperationsSnapshot() error = %v", err)
	}
	if len(success.batches()) == 0 {
		t.Fatal("otel exporter received no batches")
	}
	for _, exporter := range snap.Exporters {
		if exporter.Type == OperationsExporterOTel && (exporter.Status != "enabled" || !exporter.LowCardinalityGate || !exporter.SensitiveFieldGate || !exporter.FailOpen) {
			t.Fatalf("otel exporter status = %+v, want enabled fail-open boundary with gates", exporter)
		}
	}

	badSnapshot := OperationsSnapshot{
		Events: []EventSummary{{
			PluginID: "plugin-a",
			Name:     "auth.bad",
			Count:    1,
			Fields:   map[string]string{"token": "secret"},
		}},
	}
	if _, err := operationsExportBatch(OperationsExporterPrometheus, badSnapshot); err == nil {
		t.Fatal("operationsExportBatch(sensitive label) error = nil")
	}
	badSnapshot.Events[0].Fields = map[string]string{"result": strings.Repeat("x", DefaultLabelValueMaxBytes+1)}
	if _, err := operationsExportBatch(OperationsExporterPrometheus, badSnapshot); err == nil {
		t.Fatal("operationsExportBatch(high-cardinality value) error = nil")
	}
	tooManyLabels := make(map[string]string)
	for i := 0; i < 13; i++ {
		tooManyLabels[fmt.Sprintf("label_%02d", i)] = "ok"
	}
	badSnapshot.Events[0].Fields = tooManyLabels
	if _, err := operationsExportBatch(OperationsExporterPrometheus, badSnapshot); err == nil {
		t.Fatal("operationsExportBatch(too many labels) error = nil")
	}

	rolledBack := manager.operations.RollbackExporter(OperationsExporterPrometheus)
	if rolledBack.Enabled || rolledBack.Status != "disabled" || rolledBack.RollbackCount == 0 {
		t.Fatalf("RollbackExporter() = %+v, want disabled rollback state", rolledBack)
	}
}

type recordingOperationsExporterSink struct {
	mu      sync.Mutex
	err     error
	records []OperationsExportBatch
}

func (s *recordingOperationsExporterSink) ExportOperations(_ context.Context, batch OperationsExportBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, batch)
	return s.err
}

func (s *recordingOperationsExporterSink) batches() []OperationsExportBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]OperationsExportBatch(nil), s.records...)
}
