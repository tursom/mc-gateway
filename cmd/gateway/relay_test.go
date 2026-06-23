package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriteAll(t *testing.T) {
	t.Run("partial writes", func(t *testing.T) {
		writer := &partialGatewayWriter{chunkSize: 2}
		if err := writeAll(writer, []byte("abcdef")); err != nil {
			t.Fatalf("writeAll() error = %v", err)
		}
		if got := writer.buf.String(); got != "abcdef" {
			t.Fatalf("written data = %q, want abcdef", got)
		}
	})

	t.Run("short write", func(t *testing.T) {
		if err := writeAll(shortGatewayWriter{}, []byte("abcdef")); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("writeAll() error = %v, want %v", err, io.ErrShortWrite)
		}
	})

	t.Run("writer error", func(t *testing.T) {
		wantErr := errors.New("write failed")
		writer := &partialGatewayWriter{chunkSize: 2, err: wantErr}
		if err := writeAll(writer, []byte("abcdef")); !errors.Is(err, wantErr) {
			t.Fatalf("writeAll() error = %v, want %v", err, wantErr)
		}
		if got := writer.buf.String(); got != "ab" {
			t.Fatalf("written data = %q, want ab", got)
		}
	})
}

func TestCopyForwardCopiesData(t *testing.T) {
	var dst bytes.Buffer
	n, err := copyForward(&dst, strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("copyForward() error = %v", err)
	}
	if n != int64(len("payload")) {
		t.Fatalf("copyForward() n = %d, want %d", n, len("payload"))
	}
	if got := dst.String(); got != "payload" {
		t.Fatalf("copyForward() data = %q, want payload", got)
	}
}

func TestCopyForwardWithPlainReaderWriter(t *testing.T) {
	dst := &plainGatewayWriter{}
	n, err := copyForward(dst, &plainGatewayReader{data: []byte("plain")})
	if err != nil {
		t.Fatalf("copyForward() error = %v", err)
	}
	if n != int64(len("plain")) {
		t.Fatalf("copyForward() n = %d, want %d", n, len("plain"))
	}
	if got := dst.buf.String(); got != "plain" {
		t.Fatalf("copyForward() data = %q, want plain", got)
	}
}

func TestProxyCopyClosesSides(t *testing.T) {
	src := &closingGatewayReader{}
	dst := &closingGatewayWriter{}

	proxyCopy(dst, src)

	if !src.closeReadCalled {
		t.Fatal("CloseRead was not called")
	}
	if !dst.closeWriteCalled {
		t.Fatal("CloseWrite was not called")
	}
}

func TestProxyCopyRecoversAndClosesSides(t *testing.T) {
	src := &panicGatewayReader{}
	dst := &closingGatewayWriter{}

	proxyCopy(dst, src)

	if !src.closeReadCalled {
		t.Fatal("CloseRead was not called after panic")
	}
	if !dst.closeWriteCalled {
		t.Fatal("CloseWrite was not called after panic")
	}
}

func TestCloseWriteFallsBackToClose(t *testing.T) {
	closer := &gatewayCloser{}
	closeWrite(closer)
	if !closer.closed {
		t.Fatal("Close was not called")
	}
}

func TestGetAndPutProxyBuffer(t *testing.T) {
	buf := getProxyBuffer()
	if len(buf) != proxyBufferSize {
		t.Fatalf("buffer len = %d, want %d", len(buf), proxyBufferSize)
	}
	if cap(buf) != proxyBufferSize {
		t.Fatalf("buffer cap = %d, want %d", cap(buf), proxyBufferSize)
	}

	putProxyBuffer(buf[:1])
	putProxyBuffer(make([]byte, 1))
}

type partialGatewayWriter struct {
	chunkSize int
	err       error
	buf       bytes.Buffer
}

func (w *partialGatewayWriter) Write(p []byte) (int, error) {
	if len(p) > w.chunkSize {
		p = p[:w.chunkSize]
	}
	n, _ := w.buf.Write(p)
	if w.err != nil {
		return n, w.err
	}
	return n, nil
}

type shortGatewayWriter struct{}

func (shortGatewayWriter) Write([]byte) (int, error) {
	return 0, nil
}

type plainGatewayReader struct {
	data []byte
}

func (r *plainGatewayReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type plainGatewayWriter struct {
	buf bytes.Buffer
}

func (w *plainGatewayWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

type closingGatewayReader struct {
	closeReadCalled bool
}

func (r *closingGatewayReader) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (r *closingGatewayReader) CloseRead() error {
	r.closeReadCalled = true
	return nil
}

type panicGatewayReader struct {
	closeReadCalled bool
}

func (r *panicGatewayReader) Read([]byte) (int, error) {
	panic("read panic")
}

func (r *panicGatewayReader) CloseRead() error {
	r.closeReadCalled = true
	return nil
}

type closingGatewayWriter struct {
	closeWriteCalled bool
}

func (w *closingGatewayWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *closingGatewayWriter) CloseWrite() error {
	w.closeWriteCalled = true
	return nil
}

type gatewayCloser struct {
	closed bool
}

func (c *gatewayCloser) Close() error {
	c.closed = true
	return nil
}
