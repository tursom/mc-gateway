package smoke

import (
	"bytes"
	"io"
	"net"
	"time"
)

type Result struct {
	Request  []byte
	Response []byte
}

func RunProtocolProxyFixture(initial []byte, handler func(net.Conn)) (Result, error) {
	gatewayEnd, pluginEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler(pluginEnd)
	}()
	defer gatewayEnd.Close()

	if err := gatewayEnd.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return Result{}, err
	}
	if _, err := gatewayEnd.Write(initial); err != nil {
		return Result{}, err
	}

	var response bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := gatewayEnd.Read(buf)
		if n > 0 {
			_, _ = response.Write(buf[:n])
		}
		if err == io.EOF {
			err = nil
		}
		readDone <- err
	}()
	<-done
	_ = gatewayEnd.Close()
	if err := <-readDone; err != nil {
		return Result{}, err
	}
	return Result{
		Request:  append([]byte(nil), initial...),
		Response: response.Bytes(),
	}, nil
}
