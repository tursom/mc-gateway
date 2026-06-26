package main

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
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

	if _, err := client.Write(mcAuthProxyHandshakePacket("play.example")); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	if _, err := client.Write(mcAuthProxyLoginStartPacket("Steve")); err != nil {
		t.Fatalf("write login start error = %v", err)
	}
	response, err := readPacketFromConn(client)
	if err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if !bytes.Contains(response, []byte("fixture rejected")) {
		t.Fatalf("response = %q, want fixture message", response)
	}
	if len(gateway.events) != 1 || gateway.events[0].name != "auth.failure" {
		t.Fatalf("events = %+v, want auth.failure", gateway.events)
	}
	_ = client.Close()
	<-done
}

func TestParseLoginStart(t *testing.T) {
	login, err := parseLoginStart(mcAuthProxyLoginStartPacket("Alex"))
	if err != nil {
		t.Fatalf("parseLoginStart() error = %v", err)
	}
	if login.Username != "Alex" {
		t.Fatalf("username = %q, want Alex", login.Username)
	}
}

func upstreamRequestForTest() api.UpstreamConnectRequest {
	return api.UpstreamConnectRequest{}
}

type recordedEvent struct {
	name   string
	fields map[string]string
}

type recordingGateway struct {
	events []recordedEvent
	wg     sync.WaitGroup
}

func (g *recordingGateway) HandleConn(net.Conn)            {}
func (g *recordingGateway) ExitWaitGroup() *sync.WaitGroup { return &g.wg }
func (g *recordingGateway) Hook(string, any) error         { return nil }
func (g *recordingGateway) EmitEvent(_ context.Context, name string, fields map[string]string) error {
	g.events = append(g.events, recordedEvent{name: name, fields: fields})
	return nil
}
func (g *recordingGateway) ObserveMetric(context.Context, string, float64, map[string]string) error {
	return nil
}
func (g *recordingGateway) Logger() api.Logger                              { return testLogger{} }
func (g *recordingGateway) DataStore() api.DataStore                        { return testDataStore{} }
func (g *recordingGateway) FileStore() api.FileStore                        { return testFileStore{} }
func (g *recordingGateway) ExternalClient(string) api.ExternalClient        { return testExternalClient{} }
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

type testExternalClient struct{}

func (testExternalClient) DoHTTP(context.Context, api.ExternalRequest) (api.ExternalResponse, error) {
	return api.ExternalResponse{}, nil
}
func (testExternalClient) DialTCP(context.Context, string, time.Duration) (net.Conn, error) {
	return nil, nil
}
func (testExternalClient) HealthCheck(context.Context) error { return nil }

func mcAuthProxyHandshakePacket(host string) []byte {
	payload := []byte{0x00, 0x63, byte(len(host))}
	payload = append(payload, host...)
	payload = append(payload, 0x63, 0xdd, 0x02)
	return append(encodeVarInt(len(payload)), payload...)
}

func mcAuthProxyLoginStartPacket(username string) []byte {
	payload := []byte{0x00}
	payload = append(payload, encodeVarInt(len(username))...)
	payload = append(payload, username...)
	return append(encodeVarInt(len(payload)), payload...)
}
