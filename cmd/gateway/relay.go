package main

import (
	"errors"
	"io"
	"sync"

	"github.com/rs/zerolog/log"
)

const proxyBufferSize = 64 * 1024

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
	defer recoverProxyCopy()
	defer closeRead(src)
	defer closeWrite(dst)

	_, err := copyForward(dst, src)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Debug().Err(err).Msg("proxy copy stopped")
	}
}

func copyForward(dst io.Writer, src io.Reader) (int64, error) {
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
