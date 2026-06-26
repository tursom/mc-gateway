package protocol

import (
	"bytes"
	"errors"
	"strings"
)

type Handshake struct {
	RawServerHost   string
	ServerHost      string
	ProtocolVersion int
	NextState       int
}

// ReplaceMcHost 替换 Minecraft 主机名
// 必须是连接的第一个数据包
func ReplaceMcHost(buf []byte, host string) []byte {
	packet, consumed, err := readPacket(buf)
	if err != nil || len(packet) == 0 {
		return nil
	}
	packetID, n, err := readVarInt(packet)
	if err != nil || packetID != 0 {
		return nil
	}
	prefixEnd := n
	if _, n, err = readVarInt(packet[prefixEnd:]); err != nil {
		return nil
	}
	prefixEnd += n
	rawHost, n, err := readString(packet[prefixEnd:])
	if err != nil {
		return nil
	}
	hostEnd := prefixEnd + n

	if spliterIndex := strings.IndexRune(rawHost, 0); spliterIndex != -1 {
		host = host + rawHost[spliterIndex:]
	}

	var payload bytes.Buffer
	payload.Write(packet[:prefixEnd])
	payload.Write(encodeVarInt(len(host)))
	payload.WriteString(host)
	payload.Write(packet[hostEnd:])

	var out bytes.Buffer
	out.Write(encodeVarInt(payload.Len()))
	out.Write(payload.Bytes())
	out.Write(buf[consumed:])

	return out.Bytes()
}

// GetMcHost 通过第一个数据包获取 Minecraft 主机名
func GetMcHost(buf []byte) string {
	return ParseHandshake(buf).ServerHost
}

func ParseHandshake(buf []byte) Handshake {
	packet, _, err := readPacket(buf)
	if err != nil || len(packet) == 0 {
		return Handshake{}
	}
	packetID, n, err := readVarInt(packet)
	if err != nil || packetID != 0 {
		return Handshake{}
	}
	packet = packet[n:]
	protocolVersion, n, err := readVarInt(packet)
	if err != nil {
		return Handshake{}
	}
	packet = packet[n:]
	host, n, err := readString(packet)
	if err != nil {
		return Handshake{}
	}
	packet = packet[n:]
	if _, n, err = readUnsignedShort(packet); err != nil {
		return Handshake{}
	}
	packet = packet[n:]
	nextState, _, err := readVarInt(packet)
	if err != nil {
		return Handshake{}
	}

	parsed := Handshake{
		RawServerHost:   host,
		ProtocolVersion: protocolVersion,
		NextState:       nextState,
	}

	if spliterIndex := strings.IndexRune(host, 0); spliterIndex != -1 {
		parsed.ServerHost = host[0:spliterIndex]
	} else {
		parsed.ServerHost = host
	}
	return parsed
}

func ReadPacket(buf []byte) ([]byte, int, error) {
	return readPacket(buf)
}

func ReadVarInt(buf []byte) (int, int, error) {
	return readVarInt(buf)
}

func ReadString(buf []byte) (string, int, error) {
	return readString(buf)
}

func readPacket(buf []byte) ([]byte, int, error) {
	length, n, err := readVarInt(buf)
	if err != nil {
		return nil, 0, err
	}
	if length < 0 || len(buf[n:]) < length {
		return nil, 0, errors.New("incomplete packet")
	}
	return buf[n : n+length], n + length, nil
}

func readVarInt(buf []byte) (int, int, error) {
	var value int
	for i := 0; i < 5; i++ {
		if i >= len(buf) {
			return 0, 0, errors.New("incomplete varint")
		}
		b := buf[i]
		value |= int(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1, nil
		}
	}
	return 0, 0, errors.New("varint too long")
}

func readString(buf []byte) (string, int, error) {
	length, n, err := readVarInt(buf)
	if err != nil {
		return "", 0, err
	}
	if length < 0 || len(buf[n:]) < length {
		return "", 0, errors.New("incomplete string")
	}
	return string(buf[n : n+length]), n + length, nil
}

func readUnsignedShort(buf []byte) (int, int, error) {
	if len(buf) < 2 {
		return 0, 0, errors.New("incomplete unsigned short")
	}
	return int(buf[0])<<8 | int(buf[1]), 2, nil
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
