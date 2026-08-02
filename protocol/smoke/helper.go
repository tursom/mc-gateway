// protocol/smoke/helper.go 提供小型内存测试夹具，让协议代理测试可以使用真实 net.Conn 行为。

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

func RunTakeoverFixture(initial []byte, handler func(net.Conn)) (Result, error) {
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

func NewLoopbackTCPPair() (*net.TCPConn, *net.TCPConn, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	select {
	case server := <-accepted:
		return server, client, nil
	case err := <-acceptErr:
		_ = client.Close()
		return nil, nil, err
	case <-time.After(time.Second):
		_ = client.Close()
		return nil, nil, io.ErrNoProgress
	}
}

func MinecraftHandshakePacket(host string) []byte {
	return MinecraftHandshakePacketVersion(763, host)
}

func MinecraftHandshakePacketVersion(protocolVersion int, host string) []byte {
	payload := []byte{0x00}
	payload = append(payload, encodeVarInt(protocolVersion)...)
	payload = appendString(payload, host)
	payload = append(payload, 0x63, 0xdd)
	payload = append(payload, encodeVarInt(2)...)
	return packet(payload)
}

func MinecraftLoginStartPacket(username string) []byte {
	payload := []byte{0x00}
	payload = appendString(payload, username)
	return packet(payload)
}

func MinecraftPayloadPacket(packetID int, data []byte) []byte {
	payload := encodeVarInt(packetID)
	payload = append(payload, data...)
	return packet(payload)
}

func ReadPacketFromConn(r io.Reader) ([]byte, error) {
	length, err := readVarIntFromConn(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 || length > 2*1024*1024 {
		return nil, io.ErrUnexpectedEOF
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(r, packet); err != nil {
		return nil, err
	}
	out := append(encodeVarInt(length), packet...)
	return out, nil
}

func packet(payload []byte) []byte {
	out := encodeVarInt(len(payload))
	return append(out, payload...)
}

func appendString(dst []byte, value string) []byte {
	dst = append(dst, encodeVarInt(len(value))...)
	return append(dst, value...)
}

func readVarIntFromConn(r io.Reader) (int, error) {
	var value int
	var one [1]byte
	for i := 0; i < 5; i++ {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return 0, err
		}
		b := one[0]
		value |= int(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, nil
		}
	}
	return 0, io.ErrUnexpectedEOF
}

func encodeVarInt(value int) []byte {
	var out []byte
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}
