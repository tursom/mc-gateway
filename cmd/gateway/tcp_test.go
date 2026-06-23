package main

import (
	"net"
	"testing"
	"time"
)

func TestSetSocketOptions(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
}

func TestUpstreamTcp(t *testing.T) {
	defer saveGatewayState(t)()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		accepted <- conn
	}()

	conn := upstreamTcp(listener.Addr().String())
	if conn == nil {
		t.Fatal("upstreamTcp() = nil, want connection")
	}
	defer conn.Close()

	select {
	case serverConn := <-accepted:
		serverConn.Close()
	case err := <-errCh:
		t.Fatalf("Accept() error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream TCP connection")
	}
}

func TestUpstreamTcpReturnsNilOnDialError(t *testing.T) {
	defer saveGatewayState(t)()

	if got := upstreamTcp("not a tcp address"); got != nil {
		t.Fatalf("upstreamTcp() = %v, want nil", got)
	}
}
