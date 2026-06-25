package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/plugin/api"
)

const benchmarkPayloadSize = 4 << 20

func BenchmarkForwardCopy(b *testing.B) {
	disableBenchmarkLogs(b)

	b.Run("stdlib_io_copy", func(b *testing.B) {
		benchmarkCopy(b, func(dst io.Writer, src io.Reader) {
			if _, err := io.Copy(dst, src); err != nil {
				b.Fatal(err)
			}
		})
	})

	b.Run("pooled_copy_buffer", func(b *testing.B) {
		benchmarkCopy(b, func(dst io.Writer, src io.Reader) {
			proxyCopy(dst, src)
		})
	})
}

func BenchmarkTCPForwardCopy(b *testing.B) {
	disableBenchmarkLogs(b)

	b.Run("generic_user_buffer", func(b *testing.B) {
		benchmarkTCPForward(b, func(dst, src *net.TCPConn) error {
			defer closeRead(src)
			defer closeWrite(dst)

			buf := getProxyBuffer()
			defer putProxyBuffer(buf)

			_, err := io.CopyBuffer(
				benchmarkTCPWriterOnly{conn: dst},
				benchmarkTCPReaderOnly{conn: src},
				buf,
			)
			return err
		})
	})

	b.Run("tcp_fast_path", func(b *testing.B) {
		benchmarkTCPForward(b, func(dst, src *net.TCPConn) error {
			proxyCopy(dst, src)
			return nil
		})
	})
}

func BenchmarkMapToHostInitialPacket(b *testing.B) {
	disableBenchmarkLogs(b)

	const (
		hostName     = "dev.example"
		upstreamHost = "benchmark-upstream"
	)

	packet := benchmarkHandshakePacket(hostName)
	source := newBenchmarkConn(benchmarkAddr("client:25565"))
	upstream := newBenchmarkConn(benchmarkAddr("upstream:25565"))
	publishRouteSnapshot(map[string]string{hostName: upstreamHost})
	defer publishRouteSnapshot(nil)

	restore := installBenchmarkUpstreamHook(b, hostName, upstreamHost, upstream)
	defer restore()

	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		source.ResetReader(packet)
		upstream.ResetWriter()

		client := mapToHost(source)
		if client == nil {
			b.Fatal("mapToHost returned nil")
		}
		if upstream.Written() != int64(len(packet)) {
			b.Fatalf("upstream wrote %d bytes, want %d", upstream.Written(), len(packet))
		}
	}
}

func benchmarkCopy(b *testing.B, copyFunc func(io.Writer, io.Reader)) {
	reader := newBenchmarkReader(benchmarkPayloadSize)
	writer := &benchmarkWriter{}

	b.SetBytes(benchmarkPayloadSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader.Reset(benchmarkPayloadSize)
		writer.Reset()

		copyFunc(writer, reader)
		if writer.Written() != benchmarkPayloadSize {
			b.Fatalf("copied %d bytes, want %d", writer.Written(), benchmarkPayloadSize)
		}
	}
}

func benchmarkTCPForward(b *testing.B, copyFunc func(dst, src *net.TCPConn) error) {
	b.SetBytes(benchmarkPayloadSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		srcRelay, srcWriter := newBenchmarkTCPConnPair(b)
		dstReader, dstRelay := newBenchmarkTCPConnPair(b)
		setBenchmarkTCPDeadline(b, srcRelay, srcWriter, dstReader, dstRelay)

		start := make(chan struct{})
		writerDone := make(chan error, 1)
		readerDone := make(chan benchmarkTCPReadResult, 1)

		go benchmarkTCPWrite(start, srcWriter, writerDone)
		go benchmarkTCPRead(start, dstReader, readerDone)

		b.StartTimer()
		close(start)

		copyErr := copyFunc(dstRelay, srcRelay)
		writeErr := <-writerDone
		readResult := <-readerDone

		b.StopTimer()
		closeBenchmarkTCPConns(srcRelay, srcWriter, dstReader, dstRelay)

		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			b.Fatalf("copy failed: %v", copyErr)
		}
		if writeErr != nil {
			b.Fatalf("source write failed: %v", writeErr)
		}
		if readResult.err != nil {
			b.Fatalf("destination read failed: %v", readResult.err)
		}
		if readResult.n != benchmarkPayloadSize {
			b.Fatalf("forwarded %d bytes, want %d", readResult.n, benchmarkPayloadSize)
		}
	}
}

func benchmarkTCPWrite(start <-chan struct{}, conn *net.TCPConn, done chan<- error) {
	<-start

	reader := newBenchmarkReader(benchmarkPayloadSize)
	buf := getProxyBuffer()
	_, err := io.CopyBuffer(conn, reader, buf)
	putProxyBuffer(buf)

	if closeErr := conn.CloseWrite(); err == nil {
		err = closeErr
	}
	done <- err
}

func benchmarkTCPRead(start <-chan struct{}, conn *net.TCPConn, done chan<- benchmarkTCPReadResult) {
	<-start

	n, err := io.Copy(io.Discard, conn)
	done <- benchmarkTCPReadResult{n: n, err: err}
}

func benchmarkHandshakePacket(host string) []byte {
	packet := make([]byte, 5+len(host)+2)
	packet[4] = byte(len(host))
	copy(packet[5:], host)
	return packet
}

func installBenchmarkUpstreamHook(b *testing.B, hostName, upstreamHost string, upstream net.Conn) func() {
	b.Helper()

	previousConfig := config
	previousHooks := hooks

	pluginLock.Lock()
	hooks = map[string]map[string]any{
		"benchmark": {},
	}
	pluginLock.Unlock()

	gateway := &Gateway{pluginId: "benchmark"}
	if err := api.RegisterHookHandler(
		gateway,
		api.HookUpstream,
		func(_ net.Conn, host string) bool {
			return host == upstreamHost
		},
		func(_ net.Conn, _ string) (net.Conn, error) {
			return upstream, nil
		},
	); err != nil {
		b.Fatal(err)
	}

	return func() {
		config = previousConfig

		pluginLock.Lock()
		hooks = previousHooks
		pluginLock.Unlock()
	}
}

func newBenchmarkTCPConnPair(b *testing.B) (*net.TCPConn, *net.TCPConn) {
	b.Helper()

	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		b.Fatal(err)
	}

	select {
	case err := <-acceptErr:
		client.Close()
		b.Fatal(err)
	case server := <-accepted:
		setSocketOptions(server)
		setSocketOptions(client)
		return server, client
	}

	return nil, nil
}

func setBenchmarkTCPDeadline(b *testing.B, conns ...*net.TCPConn) {
	b.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for _, conn := range conns {
		if err := conn.SetDeadline(deadline); err != nil {
			b.Fatal(err)
		}
	}
}

func closeBenchmarkTCPConns(conns ...*net.TCPConn) {
	for _, conn := range conns {
		conn.Close()
	}
}

func disableBenchmarkLogs(b *testing.B) {
	b.Helper()

	previousLogger := log.Logger
	log.Logger = zerolog.Nop()
	b.Cleanup(func() {
		log.Logger = previousLogger
	})
}

type benchmarkTCPReadResult struct {
	n   int64
	err error
}

type benchmarkTCPReaderOnly struct {
	conn *net.TCPConn
}

func (r benchmarkTCPReaderOnly) Read(p []byte) (int, error) {
	return r.conn.Read(p)
}

type benchmarkTCPWriterOnly struct {
	conn *net.TCPConn
}

func (w benchmarkTCPWriterOnly) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}

type benchmarkReader struct {
	chunk     []byte
	remaining int64
}

func newBenchmarkReader(size int64) *benchmarkReader {
	chunk := bytes.Repeat([]byte{0x5a}, 64*1024)
	return &benchmarkReader{
		chunk:     chunk,
		remaining: size,
	}
}

func (r *benchmarkReader) Reset(size int64) {
	r.remaining = size
}

func (r *benchmarkReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}

	n := len(p)
	if n > len(r.chunk) {
		n = len(r.chunk)
	}
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}

	copy(p, r.chunk[:n])
	r.remaining -= int64(n)
	return n, nil
}

type benchmarkWriter struct {
	written int64
}

func (w *benchmarkWriter) Reset() {
	w.written = 0
}

func (w *benchmarkWriter) Written() int64 {
	return w.written
}

func (w *benchmarkWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	return len(p), nil
}

type benchmarkConn struct {
	reader bytes.Reader
	writer benchmarkWriter
	addr   net.Addr
}

func newBenchmarkConn(addr net.Addr) *benchmarkConn {
	return &benchmarkConn{addr: addr}
}

func (c *benchmarkConn) ResetReader(buf []byte) {
	c.reader.Reset(buf)
}

func (c *benchmarkConn) ResetWriter() {
	c.writer.Reset()
}

func (c *benchmarkConn) Written() int64 {
	return c.writer.Written()
}

func (c *benchmarkConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *benchmarkConn) Write(p []byte) (int, error) {
	return c.writer.Write(p)
}

func (c *benchmarkConn) Close() error {
	return nil
}

func (c *benchmarkConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *benchmarkConn) RemoteAddr() net.Addr {
	return c.addr
}

func (c *benchmarkConn) SetDeadline(time.Time) error {
	return nil
}

func (c *benchmarkConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *benchmarkConn) SetWriteDeadline(time.Time) error {
	return nil
}

type benchmarkAddr string

func (a benchmarkAddr) Network() string {
	return "tcp"
}

func (a benchmarkAddr) String() string {
	return string(a)
}
