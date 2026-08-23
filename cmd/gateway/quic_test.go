// cmd/gateway/quic_test.go 包含用于约束 quic 行为的测试。

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestQUICIngressRoutesMinecraftTrafficAndStops(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("quic.example")
	upstreamAddress, upstreamDone := startGatewayTestUpstream(t, len(packet), []byte("reply"))
	setGatewayTestRoutes(map[string]string{"quic.example": upstreamAddress})

	config.Quic.Port = reserveGatewayTestUDPPort(t)
	config.Quic.ApplicationProtocols = []string{"minecraft"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runQuic(ctx) }()

	dialCtx, stopDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopDial()
	client, err := quic.DialAddr(
		dialCtx,
		net.JoinHostPort("127.0.0.1", fmt.Sprint(config.Quic.Port)),
		&tls.Config{InsecureSkipVerify: true, NextProtos: config.Quic.ApplicationProtocols},
		nil,
	)
	if err != nil {
		cancel()
		t.Fatalf("DialAddr() error = %v", err)
	}
	defer client.CloseWithError(0, "test complete")
	stream, err := client.OpenStreamSync(dialCtx)
	if err != nil {
		cancel()
		t.Fatalf("OpenStreamSync() error = %v", err)
	}
	if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := stream.Write(packet); err != nil {
		cancel()
		t.Fatalf("Write(handshake) error = %v", err)
	}
	reply := make([]byte, len("reply"))
	if _, err := io.ReadFull(stream, reply); err != nil {
		cancel()
		t.Fatalf("ReadFull(reply) error = %v", err)
	}
	if !bytes.Equal(reply, []byte("reply")) {
		t.Fatalf("QUIC reply = %q, want reply", reply)
	}
	if got := waitGatewayTestUpstream(t, upstreamDone); !bytes.Equal(got, packet) {
		t.Fatalf("upstream packet = %v, want %v", got, packet)
	}
	if err := client.CloseWithError(0, "test complete"); err != nil {
		t.Fatalf("CloseWithError(QUIC client) error = %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runQuic() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runQuic() did not stop after context cancellation")
	}
	waitForGatewayActiveConnections(t, 0)
}

func TestGetQuicNextProtos(t *testing.T) {
	defer saveGatewayState(t)()

	wantDefault := []string{"minecraft", "quic", "raw", "h3"}
	if got := getQuicNextProtos(); !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("getQuicNextProtos() = %v, want %v", got, wantDefault)
	}

	config.Quic.ApplicationProtocols = []string{"minecraft", "custom"}
	if got := getQuicNextProtos(); !reflect.DeepEqual(got, config.Quic.ApplicationProtocols) {
		t.Fatalf("getQuicNextProtos() = %v, want %v", got, config.Quic.ApplicationProtocols)
	}
}

func TestGenerateTLSConfig(t *testing.T) {
	defer saveGatewayState(t)()

	config.Quic.ApplicationProtocols = []string{"minecraft", "custom"}
	tlsConfig, err := generateTLSConfig()
	if err != nil {
		t.Fatalf("generateTLSConfig() error = %v", err)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("certificates len = %d, want 1", len(tlsConfig.Certificates))
	}
	if !reflect.DeepEqual(tlsConfig.NextProtos, config.Quic.ApplicationProtocols) {
		t.Fatalf("NextProtos = %v, want %v", tlsConfig.NextProtos, config.Quic.ApplicationProtocols)
	}

	cert, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	if !cert.IsCA && !cert.BasicConstraintsValid {
		t.Fatal("generated certificate has invalid basic constraints")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("ExtKeyUsage = %v, want server auth", cert.ExtKeyUsage)
	}
	if !cert.NotAfter.After(cert.NotBefore) {
		t.Fatalf("certificate validity range is invalid: %v - %v", cert.NotBefore, cert.NotAfter)
	}
}
