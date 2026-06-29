// examples/plugins/mc-auth-proxy/main_test.go 包含用于约束 mc auth proxy 行为的测试。

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol/smoke"
)

func TestFixtureRejectsLoginStart(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{DisconnectMessage: "fixture rejected"}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	gateway := &recordingGateway{}
	plugin.gateway = gateway
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		plugin.handleConn(upstreamRequestForTest(), server)
		close(done)
	}()

	if _, err := client.Write(smoke.MinecraftHandshakePacket("play.example")); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	if _, err := client.Write(smoke.MinecraftLoginStartPacket("Steve")); err != nil {
		t.Fatalf("write login start error = %v", err)
	}
	response, err := readPacketFromConn(client)
	if err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if !bytes.Contains(response, []byte("fixture rejected")) {
		t.Fatalf("response = %q, want fixture message", response)
	}
	_ = client.Close()
	<-done
	assertAuthSignal(t, gateway, "auth.failure", "fixture_reject")
}

func TestFixtureAcceptBackendUnavailable(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{FixtureAccept: true, Backend: "backend:25565"}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	gateway := &recordingGateway{externalClient: testExternalClient{dialErr: errors.New("backend down")}}
	plugin.gateway = gateway
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		plugin.handleConn(upstreamRequestForTest(), server)
		close(done)
	}()

	if _, err := client.Write(smoke.MinecraftHandshakePacket("play.example")); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	if _, err := client.Write(smoke.MinecraftLoginStartPacket("Steve")); err != nil {
		t.Fatalf("write login start error = %v", err)
	}
	response, err := readPacketFromConn(client)
	if err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if !bytes.Contains(response, []byte("Backend unavailable")) {
		t.Fatalf("response = %q, want backend unavailable message", response)
	}
	_ = client.Close()
	<-done
	assertAuthSignal(t, gateway, "auth.failure", "backend_unavailable")
}

func TestParseLoginStart(t *testing.T) {
	login, err := parseLoginStart(smoke.MinecraftLoginStartPacket("Alex"))
	if err != nil {
		t.Fatalf("parseLoginStart() error = %v", err)
	}
	if login.Username != "Alex" {
		t.Fatalf("username = %q, want Alex", login.Username)
	}
}

func TestFixtureAcceptRelaysMinecraftBackpressureToBackend(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{FixtureAccept: true, Backend: "backend:25565"}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	backendForPlugin, backendPeer, err := smoke.NewLoopbackTCPPair()
	if err != nil {
		t.Fatalf("NewLoopbackTCPPair(backend) error = %v", err)
	}
	defer backendForPlugin.Close()
	defer backendPeer.Close()
	clientGateway, client, err := smoke.NewLoopbackTCPPair()
	if err != nil {
		t.Fatalf("NewLoopbackTCPPair(client) error = %v", err)
	}
	defer clientGateway.Close()
	defer client.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = backendForPlugin.SetDeadline(deadline)
	_ = backendPeer.SetDeadline(deadline)
	_ = clientGateway.SetDeadline(deadline)
	_ = client.SetDeadline(deadline)

	gateway := &recordingGateway{externalClient: testExternalClient{conn: backendForPlugin}}
	plugin.gateway = gateway
	done := make(chan struct{})
	go func() {
		plugin.handleConn(upstreamRequestForTest(), clientGateway)
		close(done)
	}()

	handshake := smoke.MinecraftHandshakePacket("play.example")
	login := smoke.MinecraftLoginStartPacket("Steve")
	payload := smoke.MinecraftPayloadPacket(1, bytes.Repeat([]byte("x"), 128*1024))
	backendDone := make(chan error, 1)
	go func() {
		gotHandshake, err := smoke.ReadPacketFromConn(backendPeer)
		if err != nil {
			backendDone <- err
			return
		}
		gotLogin, err := smoke.ReadPacketFromConn(backendPeer)
		if err != nil {
			backendDone <- err
			return
		}
		gotPayload, err := smoke.ReadPacketFromConn(backendPeer)
		if err != nil {
			backendDone <- err
			return
		}
		if !bytes.Equal(gotHandshake, handshake) || !bytes.Equal(gotLogin, login) || !bytes.Equal(gotPayload, payload) {
			backendDone <- errors.New("backend received unexpected Minecraft packet sequence")
			return
		}
		if _, err := backendPeer.Write(smoke.MinecraftPayloadPacket(2, []byte("ack"))); err != nil {
			backendDone <- err
			return
		}
		backendDone <- backendPeer.CloseWrite()
	}()

	if _, err := client.Write(handshake); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	if _, err := client.Write(login); err != nil {
		t.Fatalf("write login error = %v", err)
	}
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write payload error = %v", err)
	}
	ack, err := smoke.ReadPacketFromConn(client)
	if err != nil {
		t.Fatalf("read backend ack error = %v", err)
	}
	if !bytes.Equal(ack, smoke.MinecraftPayloadPacket(2, []byte("ack"))) {
		t.Fatalf("ack = %v, want backend ack packet", ack)
	}
	_ = backendForPlugin.Close()
	_ = client.Close()
	<-done
	if err := <-backendDone; err != nil {
		t.Fatalf("backend fixture error = %v", err)
	}
	assertAuthSignal(t, gateway, "auth.success", "fixture_accept")
}

func upstreamRequestForTest() api.UpstreamConnectRequest {
	return api.UpstreamConnectRequest{}
}

type recordedEvent struct {
	name   string
	fields map[string]string
}

type recordedMetric struct {
	name   string
	value  float64
	labels map[string]string
}

type recordingGateway struct {
	events         []recordedEvent
	metrics        []recordedMetric
	externalClient api.ExternalClient
	wg             sync.WaitGroup
}

func (g *recordingGateway) HandleConn(net.Conn)            {}
func (g *recordingGateway) ExitWaitGroup() *sync.WaitGroup { return &g.wg }
func (g *recordingGateway) Hook(string, any) error         { return nil }
func (g *recordingGateway) EmitEvent(_ context.Context, name string, fields map[string]string) error {
	g.events = append(g.events, recordedEvent{name: name, fields: fields})
	return nil
}
func (g *recordingGateway) ObserveMetric(_ context.Context, name string, value float64, labels map[string]string) error {
	g.metrics = append(g.metrics, recordedMetric{name: name, value: value, labels: labels})
	return nil
}
func (g *recordingGateway) Logger() api.Logger       { return testLogger{} }
func (g *recordingGateway) DataStore() api.DataStore { return testDataStore{} }
func (g *recordingGateway) FileStore() api.FileStore { return testFileStore{} }
func (g *recordingGateway) ExternalClient(string) api.ExternalClient {
	if g.externalClient != nil {
		return g.externalClient
	}
	return testExternalClient{}
}
func (g *recordingGateway) RegisterBackgroundTask(api.BackgroundTask) error { return nil }

type testLogger struct{}

func (testLogger) Debug(context.Context, string, map[string]string) {}
func (testLogger) Info(context.Context, string, map[string]string)  {}
func (testLogger) Warn(context.Context, string, map[string]string)  {}
func (testLogger) Error(context.Context, string, map[string]string) {}

type testDataStore struct{}

func (testDataStore) Put(context.Context, api.DataRecord) error { return nil }
func (testDataStore) Get(context.Context, string) (api.DataRecord, error) {
	return api.DataRecord{}, nil
}
func (testDataStore) Delete(context.Context, string) error { return nil }

type testFileStore struct{}

func (testFileStore) ResourcePath(string) (string, error) { return "", nil }
func (testFileStore) Write(context.Context, string, string, []byte, string, time.Duration) error {
	return nil
}
func (testFileStore) Read(context.Context, string, string, int64) ([]byte, error) { return nil, nil }
func (testFileStore) Delete(context.Context, string, string) error                { return nil }

type testExternalClient struct {
	dialErr error
	conn    net.Conn
}

func (testExternalClient) DoHTTP(context.Context, api.ExternalRequest) (api.ExternalResponse, error) {
	return api.ExternalResponse{}, nil
}
func (c testExternalClient) DialTCP(context.Context, string, time.Duration) (net.Conn, error) {
	if c.dialErr != nil {
		return nil, c.dialErr
	}
	if c.conn != nil {
		return c.conn, nil
	}
	client, server := net.Pipe()
	_ = client.Close()
	return server, nil
}
func (testExternalClient) HealthCheck(context.Context) error { return nil }

func assertAuthSignal(t *testing.T, gateway *recordingGateway, eventName, result string) {
	t.Helper()
	if len(gateway.events) != 1 {
		t.Fatalf("events = %+v, want one auth event", gateway.events)
	}
	event := gateway.events[0]
	if event.name != eventName || event.fields["result"] != result || event.fields["mode"] != "fixture" {
		t.Fatalf("event = %+v, want %s result=%s mode=fixture", event, eventName, result)
	}
	if len(gateway.metrics) != 1 {
		t.Fatalf("metrics = %+v, want one auth metric", gateway.metrics)
	}
	metric := gateway.metrics[0]
	if metric.name != "auth.attempts" || metric.value != 1 || metric.labels["result"] != result || metric.labels["mode"] != "fixture" {
		t.Fatalf("metric = %+v, want auth.attempts result=%s mode=fixture", metric, result)
	}
}
