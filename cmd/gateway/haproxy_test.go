// cmd/gateway/haproxy_test.go 包含用于约束 haproxy 行为的测试。

package main

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHAProxyUpstreamUsesEffectiveSourceAddressWithZeroPort(t *testing.T) {
	defer saveGatewayState(t)()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct {
		header string
		body   string
		err    error
	}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- struct {
				header string
				body   string
				err    error
			}{err: acceptErr}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReader(conn)
		header, readErr := reader.ReadString('\n')
		body := make([]byte, 4)
		if readErr == nil {
			_, readErr = io.ReadFull(reader, body)
		}
		done <- struct {
			header string
			body   string
			err    error
		}{header: header, body: string(body), err: readErr}
	}()

	conn := haProxyUpstream("198.51.100.1:0", listener.Addr().String())
	if conn == nil {
		t.Fatal("haProxyUpstream returned nil")
	}
	if _, err := conn.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !strings.HasPrefix(result.header, "PROXY TCP4 198.51.100.1 127.0.0.1 0 ") {
		t.Fatalf("PROXY header = %q", result.header)
	}
	if result.body != "data" {
		t.Fatalf("body = %q, want data", result.body)
	}
}
