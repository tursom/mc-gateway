// cmd/quic/main_test.go 包含用于约束 quic 行为的测试。

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/tursom/mc-gateway/protocol"
	"github.com/tursom/mc-gateway/protocol/smoke"
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
	client, server := newQuicTestTCPConnPair(t)
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
	setSocketOptions(server)
}

func TestHandlerConnReturnsWhenQUICDialFails(t *testing.T) {
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
		t.Fatal("handlerConn() did not return after QUIC dial failure")
	}
}

func TestHandlerConnProxiesMinecraftOverQUIC(t *testing.T) {
	listener, err := quic.ListenAddr("127.0.0.1:0", quicTestTLSConfig(t), nil)
	if err != nil {
		t.Fatalf("quic.ListenAddr() error = %v", err)
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
		t.Fatalf("parse QUIC port %q error = %v", portText, err)
	}
	mcHost = host

	upstreamPacket := make(chan []byte, 1)
	releaseUpstream := make(chan struct{})
	upstreamErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := listener.Accept(ctx)
		if err != nil {
			upstreamErr <- err
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			upstreamErr <- err
			return
		}
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		packet, err := smoke.ReadPacketFromConn(stream)
		if err != nil {
			upstreamErr <- err
			return
		}
		upstreamPacket <- packet
		if _, err := stream.Write([]byte("quic-reply")); err != nil {
			upstreamErr <- err
			return
		}
		if err := stream.Close(); err != nil {
			upstreamErr <- err
			return
		}
		<-releaseUpstream
		_ = conn.CloseWithError(0, "test complete")
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
	reply := make([]byte, len("quic-reply"))
	if _, err := io.ReadFull(caller, reply); err != nil {
		t.Fatalf("read QUIC reply error = %v", err)
	}
	if string(reply) != "quic-reply" {
		t.Fatalf("reply = %q, want quic-reply", reply)
	}
	packet := <-upstreamPacket
	if handshake := protocol.ParseHandshake(packet); handshake.ServerHost != mcHost {
		t.Fatalf("upstream host = %q, want %q", handshake.ServerHost, mcHost)
	}

	_ = caller.Close()
	close(releaseUpstream)
	if err := <-upstreamErr; err != nil {
		t.Fatalf("QUIC upstream error = %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handlerConn() did not stop after both peers closed")
	}
}

func quicTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: privateKey}},
		NextProtos:   []string{"minecraft"},
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func newQuicTestTCPConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
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
