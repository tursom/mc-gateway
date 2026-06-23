package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHaProxyUpstreamWritesProxyHeader(t *testing.T) {
	defer saveGatewayState(t)()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	headerCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()

		header, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		headerCh <- header
	}()

	source := newGatewayTestConn(nil)
	source.remote = &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 45678}

	conn := haProxyUpstream(source, listener.Addr().String())
	if conn == nil {
		t.Fatal("haProxyUpstream() = nil, want connection")
	}
	defer conn.Close()

	select {
	case header := <-headerCh:
		if !strings.HasPrefix(header, "PROXY TCP4 127.0.0.2 127.0.0.1 45678 ") {
			t.Fatalf("proxy header = %q", header)
		}
		if !strings.HasSuffix(header, "\r\n") {
			t.Fatalf("proxy header missing CRLF: %q", header)
		}
	case err := <-errCh:
		t.Fatalf("accept/read header error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy header")
	}
}

func TestHaProxyUpstreamReturnsNilForInvalidHost(t *testing.T) {
	defer saveGatewayState(t)()

	if got := haProxyUpstream(newGatewayTestConn(nil), "not a tcp address"); got != nil {
		t.Fatalf("haProxyUpstream() = %v, want nil", got)
	}
}
