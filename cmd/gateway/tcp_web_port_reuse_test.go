package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tursom/mc-gateway/protocol"
)

func TestTCPWebPortReuseEnabledUsesNormalizedPorts(t *testing.T) {
	tests := []struct {
		name      string
		tcp       ProtocolConfig
		websocket WebSocketConfig
		want      bool
	}{
		{
			name:      "explicit same port",
			tcp:       ProtocolConfig{Enable: true, Port: 25565},
			websocket: WebSocketConfig{Enable: true, Port: 25565},
			want:      true,
		},
		{
			name:      "websocket default port",
			tcp:       ProtocolConfig{Enable: true, Port: defaultWebSocketPort},
			websocket: WebSocketConfig{Enable: true},
			want:      true,
		},
		{
			name:      "tcp default port",
			tcp:       ProtocolConfig{Enable: true},
			websocket: WebSocketConfig{Enable: true, Port: defaultTCPPort},
			want:      true,
		},
		{
			name:      "different ports",
			tcp:       ProtocolConfig{Enable: true, Port: 25565},
			websocket: WebSocketConfig{Enable: true, Port: 25566},
			want:      false,
		},
		{
			name:      "tcp disabled",
			tcp:       ProtocolConfig{Enable: false, Port: 25565},
			websocket: WebSocketConfig{Enable: true, Port: 25565},
			want:      false,
		},
		{
			name:      "websocket disabled",
			tcp:       ProtocolConfig{Enable: true, Port: 25565},
			websocket: WebSocketConfig{Enable: false, Port: 25565},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer saveGatewayState(t)()

			config.Tcp = tt.tcp
			config.WebSocket = tt.websocket

			if got := tcpWebPortReuseEnabled(); got != tt.want {
				t.Fatalf("tcpWebPortReuseEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHTTPInitialPacketRecognition(t *testing.T) {
	for _, method := range []string{
		"GET ",
		"POST ",
		"HEAD ",
		"PUT ",
		"PATCH ",
		"DELETE ",
		"OPTIONS ",
		"CONNECT ",
		"TRACE ",
	} {
		t.Run(strings.TrimSpace(method), func(t *testing.T) {
			if !isHTTPInitialPacket([]byte(method + "/gateway HTTP/1.1\r\n")) {
				t.Fatalf("isHTTPInitialPacket(%q) = false, want true", method)
			}
		})
	}

	if isHTTPInitialPacket(gatewayTestPacket("play.example")) {
		t.Fatal("Minecraft handshake was recognized as HTTP")
	}
	if isHTTPInitialPacket([]byte("GE")) {
		t.Fatal("partial HTTP method was recognized as complete HTTP")
	}
	if !isPotentialHTTPInitialPacket([]byte("GE")) {
		t.Fatal("partial HTTP method was not recognized as a possible HTTP prefix")
	}
	if isPotentialHTTPInitialPacket([]byte("GOT ")) {
		t.Fatal("invalid HTTP method was recognized as a possible HTTP prefix")
	}
}

func TestReplayConnReadsPeekedBytesBeforeUnderlyingConn(t *testing.T) {
	base := newGatewayTestConn([]byte("rest"))
	conn := newReplayConn(base, []byte("peek-"))

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "peek-rest" {
		t.Fatalf("replayed data = %q, want peek-rest", got)
	}
}

func TestChanListenerAcceptCloseAndDeliver(t *testing.T) {
	listener := newChanListener(benchmarkAddr("listener"))
	conn := newGatewayTestConn(nil)

	if !listener.deliver(conn) {
		t.Fatal("deliver() = false, want true")
	}

	got, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if got != conn {
		t.Fatalf("Accept() = %v, want delivered conn", got)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if listener.deliver(newGatewayTestConn(nil)) {
		t.Fatal("deliver() after Close = true, want false")
	}

	_, err = listener.Accept()
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() error = %v, want %v", err, net.ErrClosed)
	}
}

func TestHandleTcpWebPortReuseConnRoutesHTTP(t *testing.T) {
	defer saveGatewayState(t)()

	listener := newChanListener(benchmarkAddr("listener"))
	source := newGatewayTestConn([]byte("GET / HTTP/1.1\r\n\r\n"))
	tcpCalled := false

	handleTcpWebPortReuseConn(source, listener, func(net.Conn) {
		tcpCalled = true
	}, time.Second)

	if tcpCalled {
		t.Fatal("TCP handler was called for HTTP request")
	}

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("HTTP replay = %q", got)
	}
}

func TestHandleTcpWebPortReuseConnRoutesMinecraft(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	listener := newChanListener(benchmarkAddr("listener"))
	source := newGatewayTestConn(packet)

	var got []byte
	handleTcpWebPortReuseConn(source, listener, func(conn net.Conn) {
		var err error
		got, err = io.ReadAll(conn)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
	}, time.Second)

	if !bytes.Equal(got, packet) {
		t.Fatalf("Minecraft replay = %v, want %v", got, packet)
	}
}

func TestHandleTcpWebPortReuseConnTimeoutClosesConn(t *testing.T) {
	defer saveGatewayState(t)()

	client, server := net.Pipe()
	defer client.Close()

	listener := newChanListener(benchmarkAddr("listener"))
	done := make(chan struct{})
	go func() {
		handleTcpWebPortReuseConn(server, listener, func(net.Conn) {
			t.Error("TCP handler was called after timeout")
		}, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial packet timeout")
	}

	if _, err := client.Write([]byte("x")); err == nil {
		t.Fatal("client Write() error = nil, want closed connection error")
	}
}

func TestServeTcpWebPortReuseServesHTTPOnSharedPort(t *testing.T) {
	defer saveGatewayState(t)()

	requested := make(chan string, 1)
	addr, stop := startPortReuseTestServer(
		t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}),
		func(conn net.Conn) {
			conn.Close()
		},
	)
	defer stop()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTP status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	select {
	case got := <-requested:
		if got != "/status" {
			t.Fatalf("request path = %q, want /status", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP request")
	}
}

func TestServeTcpWebPortReuseUpgradesWebSocketOnSharedPort(t *testing.T) {
	defer saveGatewayState(t)()

	config.WebSocket.Path = "/gateway"
	webSocketHandler := newWebSocketHandler()
	handlerDone := make(chan struct{})
	addr, stop := startPortReuseTestServer(
		t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			webSocketHandler.ServeHTTP(w, r)
		}),
		func(conn net.Conn) {
			conn.Close()
		},
	)
	defer stop()

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/gateway", nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	conn.Close()

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WebSocket handler")
	}
}

func TestServeTcpWebPortReusePreservesMinecraftInitialPacket(t *testing.T) {
	defer saveGatewayState(t)()

	hostCh := make(chan string, 1)
	addr, stop := startPortReuseTestServer(
		t,
		http.NotFoundHandler(),
		func(conn net.Conn) {
			defer conn.Close()

			buf := getProxyBuffer()
			defer putProxyBuffer(buf)

			n, err := conn.Read(buf)
			if err != nil {
				hostCh <- ""
				return
			}
			hostCh <- protocol.GetMcHost(buf[:n])
		},
	)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(gatewayTestPacket("play.example")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	select {
	case got := <-hostCh:
		if got != "play.example" {
			t.Fatalf("Minecraft host = %q, want play.example", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Minecraft branch")
	}
}

func startPortReuseTestServer(t *testing.T, handler http.Handler, tcpHandler func(net.Conn)) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- serveTcpWebPortReuse(listener, handler, tcpHandler)
	}()

	stop := func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Close(listener) error = %v", err)
		}

		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("serveTcpWebPortReuse() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for port reuse server shutdown")
		}
	}

	return listener.Addr().String(), stop
}
