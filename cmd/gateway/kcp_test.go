// cmd/gateway/kcp_test.go 包含用于约束 KCP 原生上游分发的测试。

package main

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/xtaci/kcp-go"
)

func TestMapToHostManagedPassUsesKCPUpstream(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("kcp.example")
	listener, err := kcp.ListenWithOptions("127.0.0.1:0", nil, config.Kcp.DataShards, config.Kcp.ParityShards)
	if err != nil {
		t.Fatalf("ListenWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan gatewayTestUpstreamResult, 1)
	go func() {
		conn, err := listener.AcceptKCP()
		if err != nil {
			done <- gatewayTestUpstreamResult{err: err}
			return
		}
		defer conn.Close()
		tuneKcpConn(conn)
		done <- readGatewayTestPacketOnce(conn, conn, len(packet), func(err error) bool {
			// kcp-go wraps an unexported errTimeout that does not implement net.Error.
			return strings.Contains(err.Error(), "timeout")
		})
	}()

	managedCalls := 0
	enableGatewayTestUpstreamPlugin(t, "kcp-pass", func(api.UpstreamConnectRequest) (net.Conn, error) {
		managedCalls++
		return nil, api.ErrPass
	})
	setGatewayTestRoutes(map[string]string{"kcp.example": "kcp://" + listener.Addr().String()})

	client := mapToHost(newGatewayTestConn(packet))
	if client == nil {
		t.Fatal("mapToHost() = nil, want KCP upstream")
	}
	if got := waitGatewayTestUpstream(t, done); !bytes.Equal(got, packet) {
		t.Fatalf("KCP upstream packet = %v, want %v", got, packet)
	}
	_ = client.Close()
	if managedCalls != 1 {
		t.Fatalf("managed plugin calls = %d, want 1 pass", managedCalls)
	}
}
