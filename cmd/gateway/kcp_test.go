// cmd/gateway/kcp_test.go 包含用于约束 KCP 原生上游分发的测试。

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/kcp-go"
)

func TestKCPIngressRoutesMinecraftTrafficAndStops(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("kcp.example")
	upstreamAddress, upstreamDone := startGatewayTestUpstream(t, len(packet), []byte("reply"))
	setGatewayTestRoutes(map[string]string{"kcp.example": upstreamAddress})

	config.Kcp.Port = reserveGatewayTestUDPPort(t)
	config.Kcp.DataShards = defaultKCPDataShards
	config.Kcp.ParityShards = defaultKCPParityShards
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runKcp(ctx) }()

	client, err := kcp.DialWithOptions(
		net.JoinHostPort("127.0.0.1", fmt.Sprint(config.Kcp.Port)),
		nil,
		config.Kcp.DataShards,
		config.Kcp.ParityShards,
	)
	if err != nil {
		cancel()
		t.Fatalf("DialWithOptions() error = %v", err)
	}
	tuneKcpConn(client)
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := client.Write(packet); err != nil {
		cancel()
		t.Fatalf("Write(handshake) error = %v", err)
	}
	reply := make([]byte, len("reply"))
	if _, err := io.ReadFull(client, reply); err != nil {
		cancel()
		t.Fatalf("ReadFull(reply) error = %v", err)
	}
	if !bytes.Equal(reply, []byte("reply")) {
		t.Fatalf("KCP reply = %q, want reply", reply)
	}
	if got := waitGatewayTestUpstream(t, upstreamDone); !bytes.Equal(got, packet) {
		t.Fatalf("upstream packet = %v, want %v", got, packet)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(KCP client) error = %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runKcp() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runKcp() did not stop after context cancellation")
	}
	waitForGatewayActiveConnections(t, 0)
}

func reserveGatewayTestUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatalf("Close(UDP listener) error = %v", err)
	}
	return port
}
