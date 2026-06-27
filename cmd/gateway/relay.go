// cmd/gateway/relay.go 实现客户端与上游之间的双向复制循环和转发缓冲池。

package main

import (
	"errors"
	"io"
	"sync"

	"github.com/rs/zerolog/log"
)

const proxyBufferSize = 64 * 1024

// proxyBufferPool 为普通 io.CopyBuffer 路径复用 64KiB 缓冲区，降低长连接转发时的分配压力。
var proxyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, proxyBufferSize)
		return &buf
	},
}

type (
	closeWriter interface {
		CloseWrite() error
	}

	closeReader interface {
		CloseRead() error
	}
)

func proxyConnections(a, b io.ReadWriter) {
	var wg sync.WaitGroup

	// 两个方向独立复制，任意一侧读到 EOF 后通过半关闭通知对端。
	wg.Add(2)
	go func() {
		defer wg.Done()
		proxyCopy(b, a)
	}()
	go func() {
		defer wg.Done()
		proxyCopy(a, b)
	}()

	wg.Wait()
}

func proxyCopy(dst io.Writer, src io.Reader) {
	// 转发协程不能把 panic 带出到连接处理主协程；记录后关闭对应方向即可。
	defer recoverProxyCopy()
	defer closeRead(src)
	defer closeWrite(dst)

	_, err := copyForward(dst, src)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Debug().Err(err).Msg("proxy copy stopped")
	}
}

func copyForward(dst io.Writer, src io.Reader) (int64, error) {
	// 优先使用标准库为具体类型提供的零拷贝/优化路径，只有普通 reader/writer
	// 才落到共享缓冲区。
	if _, ok := src.(io.WriterTo); ok {
		return io.Copy(dst, src)
	}
	if _, ok := dst.(io.ReaderFrom); ok {
		return io.Copy(dst, src)
	}

	buf := getProxyBuffer()
	defer putProxyBuffer(buf)

	return io.CopyBuffer(dst, src, buf)
}

func getProxyBuffer() []byte {
	return *proxyBufferPool.Get().(*[]byte)
}

func putProxyBuffer(buf []byte) {
	if cap(buf) != proxyBufferSize {
		return
	}
	buf = buf[:proxyBufferSize]
	proxyBufferPool.Put(&buf)
}

func writeAll(w io.Writer, buf []byte) error {
	// net.Conn.Write 允许短写；首包回放和 PROXY 头写入必须循环直到写完。
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}

	return nil
}

func closeWrite(conn any) {
	// TCP 支持半关闭时只关闭写方向，让反向复制还有机会读完剩余数据。
	if closer, ok := conn.(closeWriter); ok {
		if err := closer.CloseWrite(); err != nil {
			log.Debug().Err(err).Msg("failed to close write side")
		}
		return
	}

	if closer, ok := conn.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close connection")
		}
	}
}

func closeRead(conn any) {
	// 支持 CloseRead 的连接可以显式停止读方向，帮助对端更快感知转发结束。
	if closer, ok := conn.(closeReader); ok {
		if err := closer.CloseRead(); err != nil {
			log.Debug().Err(err).Msg("failed to close read side")
		}
	}
}

func recoverProxyCopy() {
	if rec := recover(); rec != nil {
		log.Error().Any("panic", rec).Msg("panic in proxy copy")
	}
}
