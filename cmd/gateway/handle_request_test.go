package main

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

func TestHandleRequestProxiesAndClosesConnections(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn([]byte("reply"))
	config.Hosts = map[string]string{
		"play.example": "backend.example:25565",
	}

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed")
	}
	if !upstream.closed {
		t.Fatal("upstream connection was not closed")
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
	if got := source.writeBuf.String(); got != "reply" {
		t.Fatalf("proxied reply = %q, want reply", got)
	}
}

func TestHandleRequestRecoversAndClosesConnection(t *testing.T) {
	defer saveGatewayState(t)()

	source := &panicReadGatewayConn{gatewayTestConn: newGatewayTestConn(nil)}

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed after panic")
	}
}

func TestGatewayHandleConnStartsRequestGoroutine(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(nil)
	source.readErr = errors.New("read failed")

	(&Gateway{}).HandleConn(source)

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for HandleConn goroutine")
		case <-ticker.C:
			if source.isClosed() {
				return
			}
		}
	}
}

func TestGatewayTestOpPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("TestOp() did not panic")
		}
	}()

	(&Gateway{}).TestOp()
}

type panicReadGatewayConn struct {
	*gatewayTestConn
}

func (c *panicReadGatewayConn) Read([]byte) (int, error) {
	panic("read panic")
}
