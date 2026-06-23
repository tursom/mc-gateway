package main

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

func TestMapToHostRoutesThroughHookAndForwardsInitialPacket(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example", 0x63, 0x00)
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn(nil)
	config.Hosts = map[string]string{
		"play.example": "backend.example:25565",
	}

	var gotSource net.Conn
	var gotHost string
	registerGatewayUpstreamHook(
		t,
		func(source net.Conn, host string) bool {
			gotSource = source
			gotHost = host
			return host == "backend.example:25565"
		},
		func(source net.Conn, host string) (net.Conn, error) {
			return upstream, nil
		},
	)

	got := mapToHost(source)
	if got != upstream {
		t.Fatalf("mapToHost() = %v, want upstream conn", got)
	}
	if gotSource != source {
		t.Fatalf("hook source = %v, want original source", gotSource)
	}
	if gotHost != "backend.example:25565" {
		t.Fatalf("hook host = %q, want backend.example:25565", gotHost)
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
}

func TestMapToHostUsesDefaultRoute(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("unknown.example")
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn(nil)
	config.Hosts = map[string]string{
		"default": "fallback.example:25565",
	}

	registerGatewayUpstreamHook(
		t,
		func(_ net.Conn, host string) bool {
			return host == "fallback.example:25565"
		},
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	if got := mapToHost(source); got != upstream {
		t.Fatalf("mapToHost() = %v, want fallback upstream", got)
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
}

func TestMapToHostRejectsInvalidOrUnroutedPackets(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		hosts  map[string]string
	}{
		{
			name:   "read error",
			packet: nil,
			hosts:  map[string]string{"default": "fallback.example:25565"},
		},
		{
			name:   "malformed packet",
			packet: []byte{0x01, 0x02, 0x03, 0x04, 0x08, 'a'},
			hosts:  map[string]string{"default": "fallback.example:25565"},
		},
		{
			name:   "missing route",
			packet: gatewayTestPacket("unknown.example"),
			hosts:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer saveGatewayState(t)()

			source := newGatewayTestConn(tt.packet)
			if tt.packet == nil {
				source.readErr = errors.New("read failed")
			}
			config.Hosts = tt.hosts

			if got := mapToHost(source); got != nil {
				t.Fatalf("mapToHost() = %v, want nil", got)
			}
		})
	}
}

func TestMapToHostReturnsNilWhenHookFails(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(gatewayTestPacket("play.example"))
	config.Hosts = map[string]string{
		"play.example": "backend.example:25565",
	}
	wantErr := errors.New("hook failed")

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) {
			return nil, wantErr
		},
	)

	if got := mapToHost(source); got != nil {
		t.Fatalf("mapToHost() = %v, want nil", got)
	}
}

func TestMapToHostClosesUpstreamWhenInitialWriteFails(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(gatewayTestPacket("play.example"))
	upstream := newGatewayTestConn(nil)
	upstream.writeErr = errors.New("write failed")
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

	if got := mapToHost(source); got != nil {
		t.Fatalf("mapToHost() = %v, want nil", got)
	}
	if !upstream.closed {
		t.Fatal("upstream was not closed after write failure")
	}
}
