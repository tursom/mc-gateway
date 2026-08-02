package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

func TestHandleRequestDispatchesTakeoverBeforeMinecraftRead(t *testing.T) {
	defer saveGatewayState(t)()

	source := &countingGatewayConn{gatewayTestConn: newGatewayTestConn(gatewayTestPacket("play.example"))}
	installGatewayTakeoverPlugin(t, func(req api.UpstreamConnectRequestV2) error {
		if got := source.reads.Load(); got != 0 {
			t.Fatalf("client reads before takeover = %d, want 0", got)
		}
		if req.PeerAddr != source.RemoteAddr().String() || req.LocalAddr != source.LocalAddr().String() {
			t.Fatalf("original addresses = peer %q local %q", req.PeerAddr, req.LocalAddr)
		}
		return nil
	})

	handleRequest(source)
	if got := source.reads.Load(); got != 0 {
		t.Fatalf("fully handled connection reads = %d, want 0", got)
	}
	if !source.isClosed() {
		t.Fatal("root connection was not closed after handler returned")
	}
}

func TestHandleRequestReplacementStreamReplaysConsumedMinecraftBytes(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	source := newGatewayTestConn(packet)
	upstreamAddress, upstreamDone := startGatewayTestUpstream(t, len(packet), nil)
	setGatewayTestRoutes(map[string]string{"play.example": upstreamAddress})
	installGatewayTakeoverPlugin(t, func(req api.UpstreamConnectRequestV2) error {
		prefix := make([]byte, 4)
		if _, err := io.ReadFull(req.Connection.Stream, prefix); err != nil {
			return err
		}
		state := req.Connection
		state.Stream = &replayGatewayConn{Conn: req.Connection.Stream, prefix: prefix}
		state.Metadata = map[string]string{"inspected": "true"}
		return req.Flow.Next(state)
	})

	handleRequest(source)
	if got := waitGatewayTestUpstream(t, upstreamDone); !bytes.Equal(got, packet) {
		t.Fatalf("upstream packet = %v, want %v", got, packet)
	}
}

func TestHandleRequestTakeoverFailureDoesNotEnterCore(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler api.UpstreamConnectHandlerV2
	}{
		{name: "error", handler: func(api.UpstreamConnectRequestV2) error { return errors.New("rejected") }},
		{name: "panic", handler: func(api.UpstreamConnectRequestV2) error { panic("rejected") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer saveGatewayState(t)()
			source := &countingGatewayConn{gatewayTestConn: newGatewayTestConn(gatewayTestPacket("play.example"))}
			installGatewayTakeoverPlugin(t, test.handler)
			handleRequest(source)
			if got := source.reads.Load(); got != 0 {
				t.Fatalf("failed handler entered core and read %d times", got)
			}
			if !source.isClosed() {
				t.Fatal("failed takeover did not close root connection")
			}
		})
	}
}

func TestConnectionIngressContextPreservesTransportFacts(t *testing.T) {
	base := newGatewayTestConn(nil)
	httpHeaders := http.Header{"X-Test": []string{"one", "two"}}
	httpConn := &testIngressConn{
		gatewayTestConn: base,
		transport:       "websocket",
		service:         serviceNameWebSocket,
		http:            &api.HTTPIngressContext{Method: "GET", Host: "mc.example", Path: "/mc?token=x", Headers: httpHeaders},
	}
	httpIngress := connectionIngressContext(httpConn)
	if httpIngress.Transport != "websocket" || httpIngress.ServiceName != serviceNameWebSocket || httpIngress.ListenerPort != 25565 {
		t.Fatalf("websocket ingress = %+v", httpIngress)
	}
	if httpIngress.HTTP == nil || !equalHeaderValues(httpIngress.HTTP.Headers.Values("X-Test"), []string{"one", "two"}) {
		t.Fatalf("HTTP ingress = %+v", httpIngress.HTTP)
	}
	httpHeaders.Set("X-Test", "mutated")
	if got := httpIngress.HTTP.Headers.Values("X-Test"); !equalHeaderValues(got, []string{"one", "two"}) {
		t.Fatalf("HTTP snapshot changed with source headers: %v", got)
	}

	quicConn := &testIngressConn{
		gatewayTestConn: newGatewayTestConn(nil), transport: "quic", service: serviceNameQUIC,
		quic: &api.QUICIngressContext{ApplicationProtocol: "minecraft"},
	}
	quicConn.local = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 24444}
	quicIngress := connectionIngressContext(quicConn)
	if quicIngress.Transport != "quic" || quicIngress.ListenerPort != 24444 || quicIngress.HTTP != nil || quicIngress.QUIC == nil || quicIngress.QUIC.ApplicationProtocol != "minecraft" {
		t.Fatalf("QUIC ingress = %+v", quicIngress)
	}

	kcpConn := &testIngressConn{gatewayTestConn: newGatewayTestConn(nil), transport: "kcp", service: serviceNameKCP}
	kcpConn.local = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 23333}
	kcpIngress := connectionIngressContext(kcpConn)
	if kcpIngress.Transport != "kcp" || kcpIngress.ListenerPort != 23333 || kcpIngress.HTTP != nil || kcpIngress.QUIC != nil {
		t.Fatalf("KCP ingress = %+v", kcpIngress)
	}
	rawIngress := connectionIngressContext(newGatewayTestConn(nil))
	if rawIngress.Transport != "tcp" || rawIngress.HTTP != nil || rawIngress.QUIC != nil {
		t.Fatalf("TCP ingress = %+v", rawIngress)
	}
}

func TestTrustedRealIPEndToEndWritesHAProxyHeaderThenMinecraftHandshake(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("trusted.example")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	type result struct {
		header string
		packet []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- result{err: acceptErr}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		header, readErr := reader.ReadString('\n')
		if readErr != nil {
			done <- result{err: readErr}
			return
		}
		got := make([]byte, len(packet))
		_, readErr = io.ReadFull(reader, got)
		done <- result{header: header, packet: got, err: readErr}
	}()

	pluginsManager = pluginmanager.New(pluginmanager.Options{DB: newGatewayTestPluginDB(t), ArtifactRoot: t.TempDir()})
	const artifactID = "builtin-official-trusted-real-ip-0.1.0"
	configJSON := `{"header":"X-Real-IP","trusted_peers":["127.0.0.0/8"]}`
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "official.trusted-real-ip", artifactID, pluginmanager.DesiredEnabled, configJSON, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "official.trusted-real-ip"); err != nil {
		t.Fatal(err)
	}
	setGatewayTestRoutes(map[string]string{"trusted.example": "haproxy://" + listener.Addr().String()})

	source := &testIngressConn{
		gatewayTestConn: newGatewayTestConn(packet), transport: "websocket", service: serviceNameWebSocket,
		http: &api.HTTPIngressContext{Method: "GET", Host: "gateway.example", Path: "/mc", Headers: http.Header{"X-Real-Ip": []string{"198.51.100.7"}}},
	}
	handleRequest(source)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !strings.HasPrefix(got.header, "PROXY TCP4 198.51.100.7 127.0.0.1 0 ") {
			t.Fatalf("PROXY header = %q", got.header)
		}
		if !bytes.Equal(got.packet, packet) {
			t.Fatalf("Minecraft packet = %v, want %v", got.packet, packet)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for HAProxy upstream")
	}
}

func installGatewayTakeoverPlugin(t *testing.T, handler api.UpstreamConnectHandlerV2) {
	t.Helper()
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB: newGatewayTestPluginDB(t), ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{initHook: func(gateway *pluginmanager.Gateway) error {
			return api.RegisterUpstreamConnectHandlerV2(gateway, handler)
		}},
	})
	artifact := uploadGatewayTestArtifact(t, pluginsManager, "takeover-test")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "takeover-test", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "takeover-test"); err != nil {
		t.Fatal(err)
	}
}

type countingGatewayConn struct {
	*gatewayTestConn
	reads atomic.Int64
}

func (c *countingGatewayConn) Read(data []byte) (int, error) {
	c.reads.Add(1)
	return c.gatewayTestConn.Read(data)
}

type replayGatewayConn struct {
	net.Conn
	prefix []byte
}

func (c *replayGatewayConn) Read(data []byte) (int, error) {
	n := copy(data, c.prefix)
	c.prefix = c.prefix[n:]
	if n > 0 {
		return n, nil
	}
	read, err := c.Conn.Read(data[n:])
	if n+read > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n + read, err
}

type testIngressConn struct {
	*gatewayTestConn
	transport string
	service   string
	http      *api.HTTPIngressContext
	quic      *api.QUICIngressContext
}

func (c *testIngressConn) IngressTransport() (string, string) { return c.transport, c.service }

func (c *testIngressConn) HTTPIngressContext() *api.HTTPIngressContext {
	if c.http == nil {
		return nil
	}
	out := *c.http
	out.Headers = c.http.Headers.Clone()
	return &out
}

func (c *testIngressConn) QUICIngressContext() *api.QUICIngressContext {
	if c.quic == nil {
		return nil
	}
	out := *c.quic
	return &out
}

func equalHeaderValues(got, want []string) bool {
	return len(got) == len(want) && strings.Join(got, "\x00") == strings.Join(want, "\x00")
}
