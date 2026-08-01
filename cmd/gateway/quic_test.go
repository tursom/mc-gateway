// cmd/gateway/quic_test.go 包含用于约束 quic 行为的测试。

package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"net"
	"reflect"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/tursom/mc-gateway/plugin/api"
)

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

func TestMapToHostManagedPassUsesQUICUpstream(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("quic.example")
	tlsConfig, err := generateTLSConfig()
	if err != nil {
		t.Fatalf("generateTLSConfig() error = %v", err)
	}
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	listener, err := quic.Listen(udpConn, tlsConfig, nil)
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("quic.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = udpConn.Close()
	})

	done := make(chan gatewayTestUpstreamResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := listener.Accept(ctx)
		if err != nil {
			done <- gatewayTestUpstreamResult{err: err}
			return
		}
		defer conn.CloseWithError(0, "test complete")
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			done <- gatewayTestUpstreamResult{err: err}
			return
		}
		done <- readGatewayTestPacketOnce(stream, quicConn{Connection: conn, Stream: stream}, len(packet), nil)
	}()

	managedCalls := 0
	enableGatewayTestUpstreamPlugin(t, "quic-pass", func(api.UpstreamConnectRequest) (net.Conn, error) {
		managedCalls++
		return nil, api.ErrPass
	})
	setGatewayTestRoutes(map[string]string{"quic.example": "quic://" + udpConn.LocalAddr().String()})

	client := mapToHost(newGatewayTestConn(packet))
	if client == nil {
		t.Fatal("mapToHost() = nil, want QUIC upstream")
	}
	if got := waitGatewayTestUpstream(t, done); !bytes.Equal(got, packet) {
		t.Fatalf("QUIC upstream packet = %v, want %v", got, packet)
	}
	_ = client.Close()
	if managedCalls != 1 {
		t.Fatalf("managed plugin calls = %d, want 1 pass", managedCalls)
	}
}
