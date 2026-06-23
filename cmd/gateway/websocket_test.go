package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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
