// internal/tcphttpmux/mux_test.go 包含用于约束 mux 行为的测试。

package tcphttpmux

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPInitialPacketRecognition(t *testing.T) {
	for _, method := range []string{
		"GET ",
		"POST ",
		"HEAD ",
		"PUT ",
		"PATCH ",
		"DELETE ",
		"OPTIONS ",
		"CONNECT ",
		"TRACE ",
	} {
		t.Run(strings.TrimSpace(method), func(t *testing.T) {
			if !IsHTTPInitialPacket([]byte(method + "/gateway HTTP/1.1\r\n")) {
				t.Fatalf("IsHTTPInitialPacket(%q) = false, want true", method)
			}
		})
	}

	if IsHTTPInitialPacket([]byte{0x00, 0x01, 0x02}) {
		t.Fatal("non-HTTP packet was recognized as HTTP")
	}
	if IsHTTPInitialPacket([]byte("GE")) {
		t.Fatal("partial HTTP method was recognized as complete HTTP")
	}
	if !IsPotentialHTTPInitialPacket([]byte("GE")) {
		t.Fatal("partial HTTP method was not recognized as a possible HTTP prefix")
	}
	if IsPotentialHTTPInitialPacket([]byte("GOT ")) {
		t.Fatal("invalid HTTP method was recognized as a possible HTTP prefix")
	}
}

func TestReplayConnReadsPeekedBytesBeforeUnderlyingConn(t *testing.T) {
	base := newMuxTestConn([]byte("rest"))
	conn := NewReplayConn(base, []byte("peek-"))

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "peek-rest" {
		t.Fatalf("replayed data = %q, want peek-rest", got)
	}
}

func TestReplayConnForwardsCloseWrite(t *testing.T) {
	base := &halfCloseMuxTestConn{muxTestConn: newMuxTestConn(nil)}
	conn := NewReplayConn(base, nil)
	closer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("replay connection does not expose CloseWrite")
	}
	if err := closer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if !base.writeClosed || base.closed {
		t.Fatalf("writeClosed=%v closed=%v, want half-close only", base.writeClosed, base.closed)
	}
}

func TestChanListenerAcceptCloseAndDeliver(t *testing.T) {
	listener := NewChanListener(muxTestAddr("listener"), DefaultHTTPConnBacklog)
	conn := newMuxTestConn(nil)

	if !listener.Deliver(conn) {
		t.Fatal("Deliver() = false, want true")
	}

	got, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if got != conn {
		t.Fatalf("Accept() = %v, want delivered conn", got)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if listener.Deliver(newMuxTestConn(nil)) {
		t.Fatal("Deliver() after Close = true, want false")
	}

	_, err = listener.Accept()
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() error = %v, want %v", err, net.ErrClosed)
	}
}

func TestServeWaitsForInFlightRoutingBeforeReturning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	routingStarted := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(listener, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(net.Conn) {}, Options{
			InitialPacketTimeout: time.Second,
			SetSocketOptions: func(net.Conn) {
				close(routingStarted)
			},
		})
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	select {
	case <-routingStarted:
	case <-time.After(time.Second):
		t.Fatal("routing did not start")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close(listener) error = %v", err)
	}
	select {
	case err := <-serveDone:
		t.Fatalf("Serve() returned before in-flight routing completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(client) error = %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not return after routing completed")
	}
}

func TestHandleConnRoutesHTTP(t *testing.T) {
	listener := NewChanListener(muxTestAddr("listener"), DefaultHTTPConnBacklog)
	source := newMuxTestConn([]byte("GET / HTTP/1.1\r\n\r\n"))
	tcpCalled := false

	HandleConn(source, listener, func(net.Conn) {
		tcpCalled = true
	}, Options{InitialPacketTimeout: time.Second})

	if tcpCalled {
		t.Fatal("TCP handler was called for HTTP request")
	}

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("HTTP replay = %q", got)
	}
}

func TestHandleConnRoutesTCP(t *testing.T) {
	packet := []byte{0x00, 0x01, 0x02, 'm', 'c'}
	listener := NewChanListener(muxTestAddr("listener"), DefaultHTTPConnBacklog)
	source := newMuxTestConn(packet)

	tcpStarted := false
	var got []byte
	HandleConn(source, listener, func(conn net.Conn) {
		var err error
		got, err = io.ReadAll(conn)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
	}, Options{
		InitialPacketTimeout: time.Second,
		OnTCPConnection: func() {
			tcpStarted = true
		},
	})

	if !tcpStarted {
		t.Fatal("OnTCPConnection was not called")
	}
	if !bytes.Equal(got, packet) {
		t.Fatalf("TCP replay = %v, want %v", got, packet)
	}
}

func TestHandleConnTimeoutClosesConn(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	listener := NewChanListener(muxTestAddr("listener"), DefaultHTTPConnBacklog)
	done := make(chan struct{})
	go func() {
		HandleConn(server, listener, func(net.Conn) {
			t.Error("TCP handler was called after timeout")
		}, Options{InitialPacketTimeout: 10 * time.Millisecond})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial packet timeout")
	}

	if _, err := client.Write([]byte("x")); err == nil {
		t.Fatal("client Write() error = nil, want closed connection error")
	}
}

type muxTestConn struct {
	reader *bytes.Reader
	closed bool
}

type halfCloseMuxTestConn struct {
	*muxTestConn
	writeClosed bool
}

func (c *halfCloseMuxTestConn) CloseWrite() error {
	c.writeClosed = true
	return nil
}

func newMuxTestConn(data []byte) *muxTestConn {
	return &muxTestConn{reader: bytes.NewReader(data)}
}

func (c *muxTestConn) Read(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.reader.Read(p)
}

func (c *muxTestConn) Write(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func (c *muxTestConn) Close() error {
	c.closed = true
	return nil
}

func (c *muxTestConn) LocalAddr() net.Addr {
	return muxTestAddr("local")
}

func (c *muxTestConn) RemoteAddr() net.Addr {
	return muxTestAddr("remote")
}

func (c *muxTestConn) SetDeadline(time.Time) error {
	return nil
}

func (c *muxTestConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *muxTestConn) SetWriteDeadline(time.Time) error {
	return nil
}

type muxTestAddr string

func (a muxTestAddr) Network() string {
	return "test"
}

func (a muxTestAddr) String() string {
	return string(a)
}
