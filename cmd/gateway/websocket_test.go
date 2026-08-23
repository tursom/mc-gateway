// cmd/gateway/websocket_test.go 包含用于约束 websocket 行为的测试。

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketIngressRoutesMinecraftTrafficAndStops(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("websocket.example")
	upstreamAddress, upstreamDone := startGatewayTestUpstream(t, len(packet), []byte("reply"))
	setGatewayTestRoutes(map[string]string{"websocket.example": upstreamAddress})

	config.WebSocket.Port = reserveGatewayTestTCPPort(t)
	config.WebSocket.Path = "/minecraft"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWebSocket(ctx) }()

	url := fmt.Sprintf("ws://127.0.0.1:%d/minecraft", config.WebSocket.Port)
	var client *websocket.Conn
	deadline := time.Now().Add(2 * time.Second)
	for client == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("runWebSocket() stopped before accepting connections: %v", err)
		default:
		}
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			client = conn
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		cancel()
		t.Fatalf("timed out connecting to %s", url)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if err := client.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatalf("SetWriteDeadline() error = %v", err)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, packet); err != nil {
		cancel()
		t.Fatalf("WriteMessage(handshake) error = %v", err)
	}
	messageType, reply, err := client.ReadMessage()
	if err != nil {
		cancel()
		t.Fatalf("ReadMessage(reply) error = %v", err)
	}
	if messageType != websocket.BinaryMessage || !bytes.Equal(reply, []byte("reply")) {
		t.Fatalf("WebSocket reply type=%d payload=%q, want binary reply", messageType, reply)
	}
	if got := waitGatewayTestUpstream(t, upstreamDone); !bytes.Equal(got, packet) {
		t.Fatalf("upstream packet = %v, want %v", got, packet)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(WebSocket client) error = %v", err)
	}
	waitForGatewayActiveConnections(t, 0)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWebSocket() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWebSocket() did not stop after context cancellation")
	}
}

func waitForGatewayActiveConnections(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := gatewayMetrics.Snapshot()["active_connections"]; got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active_connections = %v, want %d", gatewayMetrics.Snapshot()["active_connections"], want)
}

func TestWebSocketConnReadWriteAndDeadline(t *testing.T) {
	serverConn, clientConn, cleanup := newGatewayWebSocketPair(t)
	defer cleanup()

	conn := &webSocketConn{Conn: serverConn}

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("client WriteMessage() error = %v", err)
	}

	buf := make([]byte, len("hello"))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got := string(buf[:n]); got != "hello" {
		t.Fatalf("Read() = %q, want hello", got)
	}

	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	payload := []byte("response")
	n, err = conn.Write(payload)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write() n = %d, want %d", n, len(payload))
	}

	messageType, got, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("client ReadMessage() error = %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want %d", messageType, websocket.BinaryMessage)
	}
	if string(got) != string(payload) {
		t.Fatalf("message payload = %q, want %q", got, payload)
	}
}

func TestWebSocketConnReadContinuesAcrossFrames(t *testing.T) {
	serverConn, clientConn, cleanup := newGatewayWebSocketPair(t)
	defer cleanup()

	conn := &webSocketConn{Conn: serverConn}

	if err := clientConn.WriteMessage(websocket.BinaryMessage, []byte("abc")); err != nil {
		t.Fatalf("client WriteMessage(first) error = %v", err)
	}
	if err := clientConn.WriteMessage(websocket.BinaryMessage, []byte("def")); err != nil {
		t.Fatalf("client WriteMessage(second) error = %v", err)
	}

	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read(first) error = %v", err)
	}
	if got := string(buf[:n]); got != "ab" {
		t.Fatalf("Read(first) = %q, want ab", got)
	}
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("Read(second) error = %v", err)
	}
	if got := string(buf[:n]); got != "c" {
		t.Fatalf("Read(second) = %q, want c", got)
	}
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("Read(third) error = %v", err)
	}
	if got := string(buf[:n]); got != "de" {
		t.Fatalf("Read(third) = %q, want de", got)
	}
}

func newGatewayWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn, func()) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		serverConnCh <- conn
	}))

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		server.Close()
		t.Fatalf("Dial() error = %v", err)
	}

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverConnCh:
	case err := <-errCh:
		clientConn.Close()
		server.Close()
		t.Fatalf("Upgrade() error = %v", err)
	case <-time.After(2 * time.Second):
		clientConn.Close()
		server.Close()
		t.Fatal("timed out waiting for websocket upgrade")
	}

	cleanup := func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		server.Close()
	}

	return serverConn, clientConn, cleanup
}
