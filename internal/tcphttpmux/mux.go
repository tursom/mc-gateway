// internal/tcphttpmux/mux.go 通过窥探首包并回放数据，把同一个监听器拆分给 HTTP 和原始 TCP 处理器。

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
	// 默认只等待一秒首包，避免慢连接长期占住共享监听器的分流协程。
	DefaultInitialPacketTimeout = time.Second
	DefaultHTTPConnBacklog      = 128
	DefaultReadBufferSize       = 64 * 1024
	maxHTTPMethodPrefixLen      = len("OPTIONS ")
)

// httpMethodPrefixes 是共享端口识别 HTTP 的方法前缀白名单。Minecraft
// 握手首字节是 VarInt 长度，不会以这些明文方法开头，因此首包前缀足够分流。
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

// Options 收集共享监听器的可调参数和观测回调。调用方通过回调接入日志、
// 指标和 socket 选项，避免 tcphttpmux 反向依赖网关主包。
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

// replayConn 先读已经窥探到的首包，再继续读底层连接。这样分流逻辑可以检查
// 首包，同时 HTTP 或 TCP 处理器仍能看到完整原始字节流。
type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// ChanListener 把已识别为 HTTP 的连接投递给 http.Server。它实现 net.Listener，
// 但没有真实 accept socket，只消费 Deliver 写入的连接。
type ChanListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      net.Addr
}

// readBufferPool 降低首包窥探时的临时分配；后续回放给处理器的数据来自 peeked 副本。
var readBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, DefaultReadBufferSize)
		return &buf
	},
}

// Serve 在同一个底层监听器上同时服务 Admin HTTP 和 Minecraft TCP。每条连接
// 会先读取少量字节判断协议，再被投递给 http.Server 或 tcpHandler。
func Serve(listener net.Listener, handler http.Handler, tcpHandler func(net.Conn), opts Options) error {
	defer listener.Close()

	// http.Server 仍使用标准库模型，只是它的 listener 是内存通道。
	// 这样管理端路由、中间件和超时语义都保持为普通 HTTP 服务。
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
		// 每条连接独立分流，避免慢客户端阻塞共享监听器继续 accept。
		go HandleConn(conn, webListener, tcpHandler, opts)
	}
}

// HandleConn 完成单连接分流。它只负责协议识别和投递，认证、路由和转发
// 仍由 HTTP handler 或 tcpHandler 里的业务层完成。
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
		// HTTP 连接投递失败通常意味着 HTTP server 已关闭或通道已满；
		// 此时不应降级为 Minecraft TCP，直接关闭更明确。
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

// ReadInitialPacket 读取足够判断协议的首包片段。对于可能是 HTTP 方法名的
// 短前缀会继续读取，直到确认是 HTTP、确认不是 HTTP 或达到最长方法名前缀。
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

// NewReplayConn 用已窥探字节包裹连接，让下游处理器无需知道首包曾被提前读取。
func NewReplayConn(conn net.Conn, peeked []byte) net.Conn {
	return &replayConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(peeked), conn),
	}
}

// IsHTTPInitialPacket 判断首包是否已经完整匹配某个 HTTP 方法前缀。
func IsHTTPInitialPacket(buf []byte) bool {
	for _, prefix := range httpMethodPrefixes {
		if bytes.HasPrefix(buf, prefix) {
			return true
		}
	}
	return false
}

// IsPotentialHTTPInitialPacket 判断当前字节是否仍可能发展成 HTTP 方法名。
// 例如只读到 "GE" 时还不能判定为 Minecraft，需要继续等 "GET " 或排除。
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
	// 只回收标准容量的缓冲区，避免外部错误切片把异常大小塞回池中。
	if cap(buf) != DefaultReadBufferSize {
		return
	}
	buf = buf[:DefaultReadBufferSize]
	readBufferPool.Put(&buf)
}

// NewChanListener 创建供 http.Server 消费的内存 listener。
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
	// Deliver 是非阻塞的：HTTP accept 队列满时返回 false，由调用方关闭连接。
	// 这可以保护共享监听器不被管理端慢请求拖住。
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
