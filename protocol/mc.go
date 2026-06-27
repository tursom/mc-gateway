// protocol/mc.go 解析和改写用于主机路由与状态响应的 Minecraft 握手数据包。

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

type Handshake struct {
	// RawServerHost 保留客户端原始主机字段。部分代理协议会在主机名后附加
	// NUL 分隔的扩展数据，路由时要剥离，转发或改写时仍要保留。
	RawServerHost   string
	ServerHost      string
	ProtocolVersion int
	NextState       int
}

// ReplaceMcHost 替换 Minecraft 主机名
// 必须是连接的第一个数据包
func ReplaceMcHost(buf []byte, host string) []byte {
	// 这里只处理连接的第一个 Minecraft packet。若首包不完整或不是握手包，
	// 返回 nil 让调用方按解析失败处理。
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
		// 保留 Forge/Bungee 等协议可能附带的 NUL 后缀，只改写真正用于路由的主机名。
		host = host + rawHost[spliterIndex:]
	}

	// 主机名长度变化会影响 packet 长度，因此需要重建 payload 和外层 packet 长度。
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
	// Minecraft 握手包格式为：
	// packet length、packet id、protocol version、server address、server port、next state。
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
		// NUL 前的部分是网关路由使用的主机名，NUL 后扩展数据只保留在 RawServerHost。
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

func StatusResponsePacket(value any) ([]byte, error) {
	// 状态响应 packet id 为 0，body 是一个 JSON 字符串，外层仍使用 Minecraft
	// VarInt 长度前缀封包。
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	payload.Write(encodeVarInt(0))
	payload.Write(encodeVarInt(len(data)))
	payload.Write(data)
	var out bytes.Buffer
	out.Write(encodeVarInt(payload.Len()))
	out.Write(payload.Bytes())
	return out.Bytes(), nil
}

func readPacket(buf []byte) ([]byte, int, error) {
	// Minecraft packet 以 VarInt 表示 payload 长度；返回值 consumed 包含长度字段本身。
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
		// 每个字节低 7 位是数值，高位为 1 表示后面还有字节。
		value |= int(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1, nil
		}
	}
	return 0, 0, errors.New("varint too long")
}

func readString(buf []byte) (string, int, error) {
	// Minecraft 字符串同样使用 VarInt 长度前缀，长度按字节计算。
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
	// server port 是网络字节序的无符号短整型；当前只需要跳过并验证长度。
	if len(buf) < 2 {
		return 0, 0, errors.New("incomplete unsigned short")
	}
	return int(buf[0])<<8 | int(buf[1]), 2, nil
}

func encodeVarInt(value int) []byte {
	var out []byte
	for {
		// 与 readVarInt 对应，每轮写低 7 位，并用最高位标记是否还有后续字节。
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
