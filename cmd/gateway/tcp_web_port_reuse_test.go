// cmd/gateway/tcp_web_port_reuse_test.go 包含用于约束 tcp web port reuse 行为的测试。

package main

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tursom/mc-gateway/internal/gatewayconfig"
	"github.com/tursom/mc-gateway/protocol"
)

func TestTCPWebPortReuseEnabledUsesNormalizedPorts(t *testing.T) {
	tests := []struct {
		name      string
		tcp       gatewayconfig.ProtocolConfig
		websocket gatewayconfig.WebSocketConfig
		want      bool
	}{
		{
			name:      "explicit same port",
			tcp:       gatewayconfig.ProtocolConfig{Enable: true, Port: 25565},
			websocket: gatewayconfig.WebSocketConfig{Enable: true, Port: 25565},
			want:      true,
		},
		{
			name:      "websocket default port",
			tcp:       gatewayconfig.ProtocolConfig{Enable: true, Port: defaultWebSocketPort},
			websocket: gatewayconfig.WebSocketConfig{Enable: true},
			want:      true,
		},
		{
			name:      "tcp default port",
			tcp:       gatewayconfig.ProtocolConfig{Enable: true},
			websocket: gatewayconfig.WebSocketConfig{Enable: true, Port: defaultTCPPort},
			want:      true,
		},
		{
			name:      "different ports",
			tcp:       gatewayconfig.ProtocolConfig{Enable: true, Port: 25565},
			websocket: gatewayconfig.WebSocketConfig{Enable: true, Port: 25566},
			want:      false,
		},
		{
			name:      "tcp disabled",
			tcp:       gatewayconfig.ProtocolConfig{Enable: false, Port: 25565},
			websocket: gatewayconfig.WebSocketConfig{Enable: true, Port: 25565},
			want:      false,
		},
		{
			name:      "websocket disabled",
			tcp:       gatewayconfig.ProtocolConfig{Enable: true, Port: 25565},
			websocket: gatewayconfig.WebSocketConfig{Enable: false, Port: 25565},
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
