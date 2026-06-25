package tcphttpmux

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	DefaultInitialPacketTimeout = time.Second
	DefaultHTTPConnBacklog      = 128
	DefaultReadBufferSize       = 64 * 1024
	maxHTTPMethodPrefixLen      = len("OPTIONS ")
)

var httpMethodPrefixes = [][]byte{
	[]byte("GET "),
	[]byte("POST "),
	[]byte("HEAD "),
	[]byte("PUT "),
	[]byte("PATCH "),
	[]byte("DELETE "),
	[]byte("OPTIONS "),
	[]byte("CONNECT "),
	[]byte("TRACE "),
}

type Options struct {
	InitialPacketTimeout time.Duration
	HTTPConnBacklog      int
	SetSocketOptions     func(net.Conn)
	OnTCPConnection      func()
	OnAcceptError        func(error)
	OnInitialPacketError func(net.Conn, error)
	OnEmptyInitialPacket func(net.Conn)
	OnHTTPDeliveryFailed func(net.Conn)
}

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type ChanListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      net.Addr
}

var readBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, DefaultReadBufferSize)
		return &buf
	},
}

func Serve(listener net.Listener, handler http.Handler, tcpHandler func(net.Conn), opts Options) error {
	defer listener.Close()

	webListener := NewChanListener(listener.Addr(), opts.normalizedHTTPConnBacklog())
	webServer := &http.Server{Handler: handler}
	webServerDone := make(chan error, 1)

	go func() {
		err := webServer.Serve(webListener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			webServerDone <- err
			return
		}
		webServerDone <- nil
	}()

	defer func() {
		_ = webListener.Close()
		_ = webServer.Close()
		<-webServerDone
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if opts.OnAcceptError != nil {
				opts.OnAcceptError(err)
			}
			continue
		}

		if opts.SetSocketOptions != nil {
			opts.SetSocketOptions(conn)
		}
		go HandleConn(conn, webListener, tcpHandler, opts)
	}
}

func HandleConn(conn net.Conn, webListener *ChanListener, tcpHandler func(net.Conn), opts Options) {
	peeked, err := ReadInitialPacket(conn, opts.normalizedInitialPacketTimeout())
	if err != nil {
		if opts.OnInitialPacketError != nil {
			opts.OnInitialPacketError(conn, err)
		}
		conn.Close()
		return
	}
	if len(peeked) == 0 {
		if opts.OnEmptyInitialPacket != nil {
			opts.OnEmptyInitialPacket(conn)
		}
		conn.Close()
		return
	}

	replayed := NewReplayConn(conn, peeked)
	if IsHTTPInitialPacket(peeked) {
		if !webListener.Deliver(replayed) {
			if opts.OnHTTPDeliveryFailed != nil {
				opts.OnHTTPDeliveryFailed(conn)
			}
			conn.Close()
		}
		return
	}

	if opts.OnTCPConnection != nil {
		opts.OnTCPConnection()
	}
	tcpHandler(replayed)
}

func ReadInitialPacket(conn net.Conn, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}

	buf := getReadBuffer()
	defer putReadBuffer(buf)

	var peeked []byte
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			peeked = append(peeked, buf[:n]...)
			if IsHTTPInitialPacket(peeked) ||
				!IsPotentialHTTPInitialPacket(peeked) ||
				len(peeked) >= maxHTTPMethodPrefixLen {
				return peeked, nil
			}
		}
		if err != nil {
			if len(peeked) > 0 && errors.Is(err, io.EOF) {
				return peeked, nil
			}
			return peeked, err
		}
		if n == 0 {
			return peeked, io.ErrNoProgress
		}
	}
}

func NewReplayConn(conn net.Conn, peeked []byte) net.Conn {
	return &replayConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(peeked), conn),
	}
}

func IsHTTPInitialPacket(buf []byte) bool {
	for _, prefix := range httpMethodPrefixes {
		if bytes.HasPrefix(buf, prefix) {
			return true
		}
	}
	return false
}

func IsPotentialHTTPInitialPacket(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}

	for _, prefix := range httpMethodPrefixes {
		if len(buf) <= len(prefix) && bytes.HasPrefix(prefix, buf) {
			return true
		}
	}
	return false
}

func getReadBuffer() []byte {
	return *readBufferPool.Get().(*[]byte)
}

func putReadBuffer(buf []byte) {
	if cap(buf) != DefaultReadBufferSize {
		return
	}
	buf = buf[:DefaultReadBufferSize]
	readBufferPool.Put(&buf)
}

func NewChanListener(addr net.Addr, backlog int) *ChanListener {
	if backlog <= 0 {
		backlog = DefaultHTTPConnBacklog
	}
	return &ChanListener{
		conns:  make(chan net.Conn, backlog),
		closed: make(chan struct{}),
		addr:   addr,
	}
}

func (l *ChanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *ChanListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *ChanListener) Addr() net.Addr {
	return l.addr
}

func (l *ChanListener) Deliver(conn net.Conn) bool {
	select {
	case <-l.closed:
		return false
	default:
	}

	select {
	case l.conns <- conn:
		return true
	case <-l.closed:
		return false
	default:
		return false
	}
}

func (opts Options) normalizedInitialPacketTimeout() time.Duration {
	if opts.InitialPacketTimeout == 0 {
		return DefaultInitialPacketTimeout
	}
	return opts.InitialPacketTimeout
}

func (opts Options) normalizedHTTPConnBacklog() int {
	if opts.HTTPConnBacklog <= 0 {
		return DefaultHTTPConnBacklog
	}
	return opts.HTTPConnBacklog
}
