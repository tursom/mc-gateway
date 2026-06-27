// cmd/quic/main_test.go 包含用于约束 quic 行为的测试。

package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCopyData(t *testing.T) {
	var dst bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)

	copyData(strings.NewReader("payload"), &dst, &wg)
	wg.Wait()

	if got := dst.String(); got != "payload" {
		t.Fatalf("copyData() wrote %q, want payload", got)
	}
}

func TestCopyDataIgnoresEOFAndLogsOtherErrors(t *testing.T) {
	copyData(strings.NewReader(""), io.Discard, nil)
	copyData(errorReader{err: errors.New("read failed")}, io.Discard, nil)
}

func TestSetSocketOptions(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
}

func TestSetSocketOptionsTCPConn(t *testing.T) {
	client, server := newQuicTestTCPConnPair(t)
	defer client.Close()
	defer server.Close()

	setSocketOptions(client)
	setSocketOptions(server)
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func newQuicTestTCPConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenTCP() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan *net.TCPConn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			errCh <- err
			return
		}
		accepted <- conn
	}()

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatalf("DialTCP() error = %v", err)
	}

	select {
	case server := <-accepted:
		return client, server
	case err := <-errCh:
		client.Close()
		t.Fatalf("AcceptTCP() error = %v", err)
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("timed out waiting for TCP accept")
	}

	return nil, nil
}
