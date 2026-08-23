// cmd/kcp/main_test.go 包含用于约束 kcp 行为的测试。

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/protocol"
	"github.com/tursom/mc-gateway/protocol/smoke"
	"github.com/xtaci/kcp-go"
)

func TestCopyData(t *testing.T) {
	var dst bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)

	copyData(strings.NewReader("payload"), &dst, &wg)
	wg.Wait()

	if got := dst.String(); got != "payload" {
		t.Fatalf("copyData() wrote %q, want payload", got)
	}
}

func TestCopyDataIgnoresEOFAndLogsOtherErrors(t *testing.T) {
	copyData(strings.NewReader(""), io.Discard, nil)
	copyData(errorReader{err: errors.New("read failed")}, io.Discard, nil)
}

func TestSetSocketOptions(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
}

func TestSetSocketOptionsTCPConn(t *testing.T) {
	client, server := newKcpTestTCPConnPair(t)
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
	setSocketOptions(server)
}

func TestHandlerConnReturnsWhenKCPDialFails(t *testing.T) {
	oldHost, oldPort := mcHost, mcPort
	t.Cleanup(func() {
		mcHost = oldHost
		mcPort = oldPort
	})
	mcHost = "invalid\x00host"
	mcPort = 25565
	client, server := net.Pipe()
	defer server.Close()

	panicResult := make(chan any, 1)
	go func() {
		defer func() { panicResult <- recover() }()
		handlerConn(client)
	}()
	select {
	case recovered := <-panicResult:
		if recovered != nil {
			t.Fatalf("handlerConn() panic = %v, want clean dial failure", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("handlerConn() did not return after KCP dial failure")
	}
}

func TestHandlerConnProxiesMinecraftOverKCP(t *testing.T) {
	listener, err := kcp.ListenWithOptions("127.0.0.1:0", nil, 10, 5)
	if err != nil {
		t.Fatalf("ListenWithOptions() error = %v", err)
	}
	defer listener.Close()

	oldHost, oldPort := mcHost, mcPort
	t.Cleanup(func() {
		mcHost = oldHost
		mcPort = oldPort
	})
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", listener.Addr(), err)
	}
	if _, err := fmt.Sscanf(portText, "%d", &mcPort); err != nil {
		t.Fatalf("parse KCP port %q error = %v", portText, err)
	}
	mcHost = host

	upstreamPacket := make(chan []byte, 1)
	releaseUpstream := make(chan struct{})
	upstreamErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptKCP()
		if err != nil {
			upstreamErr <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		packet, err := smoke.ReadPacketFromConn(conn)
		if err != nil {
			upstreamErr <- err
			return
		}
		upstreamPacket <- packet
		if _, err := conn.Write([]byte("kcp-reply")); err != nil {
			upstreamErr <- err
			return
		}
		<-releaseUpstream
		upstreamErr <- nil
	}()

	client, caller := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	_ = caller.SetDeadline(deadline)
	handlerDone := make(chan struct{})
	go func() {
		handlerConn(client)
		close(handlerDone)
	}()

	initial := smoke.MinecraftHandshakePacket("original.example")
	if _, err := caller.Write(initial); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	reply := make([]byte, len("kcp-reply"))
	if _, err := io.ReadFull(caller, reply); err != nil {
		t.Fatalf("read KCP reply error = %v", err)
	}
	if string(reply) != "kcp-reply" {
		t.Fatalf("reply = %q, want kcp-reply", reply)
	}
	packet := <-upstreamPacket
	if handshake := protocol.ParseHandshake(packet); handshake.ServerHost != mcHost {
		t.Fatalf("upstream host = %q, want %q", handshake.ServerHost, mcHost)
	}

	_ = caller.Close()
	close(releaseUpstream)
	if err := <-upstreamErr; err != nil {
		t.Fatalf("KCP upstream error = %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handlerConn() did not stop after both peers closed")
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func newKcpTestTCPConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenTCP() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan *net.TCPConn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			errCh <- err
			return
		}
		accepted <- conn
	}()

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatalf("DialTCP() error = %v", err)
	}

	select {
	case server := <-accepted:
		return client, server
	case err := <-errCh:
		client.Close()
		t.Fatalf("AcceptTCP() error = %v", err)
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("timed out waiting for TCP accept")
	}

	return nil, nil
}
