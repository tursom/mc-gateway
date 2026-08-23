package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

func TestStreamProxyCancellationRemovesPendingSocket(t *testing.T) {
	service := newService()
	streamID := fmt.Sprintf("mc-gateway-stream-test-%d", time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	resp := streamProxyControl(t, service, ctx, sandboxsdk.CommandStreamOpen, sandboxsdk.StreamOpenRequest{
		ExtensionPoint: "upstream.connect/v2",
		HandlerID:      "stream-main",
		Protocol:       "mc-gateway-stream/v1",
		StreamID:       streamID,
	})
	if !resp.OK || resp.Stream == nil || !resp.Stream.Connected || resp.Stream.Endpoint == "" {
		cancel()
		t.Fatalf("stream open response = %+v, want connected Unix endpoint", resp)
	}
	endpoint := resp.Stream.Endpoint
	t.Cleanup(func() { _ = os.Remove(endpoint) })
	if _, err := os.Stat(endpoint); err != nil {
		cancel()
		t.Fatalf("Stat(open endpoint) error = %v", err)
	}

	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(endpoint); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stream endpoint %q remains after request cancellation", endpoint)
}

func TestStreamProxyEndpointEchoesBytes(t *testing.T) {
	service := newService()
	streamID := fmt.Sprintf("mc-gateway-stream-echo-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp := streamProxyControl(t, service, ctx, sandboxsdk.CommandStreamOpen, sandboxsdk.StreamOpenRequest{
		ExtensionPoint: "upstream.connect/v2",
		HandlerID:      "stream-main",
		Protocol:       "mc-gateway-stream/v1",
		StreamID:       streamID,
	})
	if !resp.OK || resp.Stream == nil || resp.Stream.Action != sandboxsdk.TakeoverActionHandled || resp.Stream.EndpointType != "unix" {
		t.Fatalf("stream open response = %+v, want handled Unix endpoint", resp)
	}
	endpoint := resp.Stream.Endpoint
	t.Cleanup(func() { _ = os.Remove(endpoint) })
	conn, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		t.Fatalf("Dial(unix endpoint) error = %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		_ = conn.Close()
		t.Fatalf("Write(ping) error = %v", err)
	}
	reply := make([]byte, len("ping"))
	if _, err := io.ReadFull(conn, reply); err != nil {
		_ = conn.Close()
		t.Fatalf("ReadFull(echo) error = %v", err)
	}
	if string(reply) != "ping" {
		_ = conn.Close()
		t.Fatalf("echo = %q, want ping", reply)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close(stream) error = %v", err)
	}

	closeResp := streamProxyControl(t, service, context.Background(), sandboxsdk.CommandStreamClose, sandboxsdk.StreamCloseRequest{
		Protocol: "mc-gateway-stream/v1",
		StreamID: streamID,
	})
	if !closeResp.OK {
		t.Fatalf("stream close response = %+v, want success", closeResp)
	}
}

func streamProxyControl(t *testing.T, service sandboxsdk.Service, ctx context.Context, command string, request any) sandboxsdk.ControlResponse {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal(request) error = %v", err)
	}
	return service.HandleControlRequest(ctx, sandboxsdk.ControlRequest{
		Command:  command,
		Protocol: sandboxsdk.Protocol,
		Payload:  payload,
	})
}
