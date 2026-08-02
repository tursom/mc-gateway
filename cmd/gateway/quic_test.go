// cmd/gateway/quic_test.go 包含用于约束 quic 行为的测试。

package main

import (
	"crypto/x509"
	"reflect"
	"testing"
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
