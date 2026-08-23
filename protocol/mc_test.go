// protocol/mc_test.go 包含用于约束 mc 行为的测试。

package protocol

import (
	"bytes"
	"testing"
)

func TestGetMcHost(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want string
	}{
		{
			name: "short packet",
			buf:  []byte{0x01, 0x02, 0x03, 0x04},
			want: "",
		},
		{
			name: "host length exceeds packet",
			buf:  []byte{0x01, 0x02, 0x03, 0x04, 0x05, 'a'},
			want: "",
		},
		{
			name: "plain host",
			buf:  mcTestPacket("play.example", 0x63, 0x00),
			want: "play.example",
		},
		{
			name: "host with null suffix",
			buf:  mcTestPacket("play.example\x00FML\x00", 0x63, 0x00),
			want: "play.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetMcHost(tt.buf); got != tt.want {
				t.Fatalf("GetMcHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplaceMcHost(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		host string
		want []byte
	}{
		{
			name: "short packet",
			buf:  []byte{0x01, 0x02, 0x03, 0x04},
			host: "new.example",
			want: nil,
		},
		{
			name: "host length exceeds packet",
			buf:  []byte{0x01, 0x02, 0x03, 0x04, 0x05, 'a'},
			host: "new.example",
			want: nil,
		},
		{
			name: "replace plain host",
			buf:  mcTestPacket("old.example", 0x63, 0x00),
			host: "new.example",
			want: mcTestPacket("new.example", 0x63, 0x00),
		},
		{
			name: "preserve null suffix",
			buf:  mcTestPacket("old.example\x00FML\x00", 0x63, 0x00),
			host: "new.example",
			want: mcTestPacket("new.example\x00FML\x00", 0x63, 0x00),
		},
		{
			name: "replace with shorter host",
			buf:  mcTestPacket("long.example", 0x01, 0x02, 0x03),
			host: "x",
			want: mcTestPacket("x", 0x01, 0x02, 0x03),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReplaceMcHost(append([]byte(nil), tt.buf...), tt.host)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("ReplaceMcHost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadVarIntUsesMinecraftSignedInt32Semantics(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want int
	}{
		{name: "zero", buf: []byte{0x00}, want: 0},
		{name: "one byte maximum", buf: []byte{0x7f}, want: 127},
		{name: "two bytes", buf: []byte{0x80, 0x01}, want: 128},
		{name: "positive maximum", buf: []byte{0xff, 0xff, 0xff, 0xff, 0x07}, want: 2147483647},
		{name: "negative one", buf: []byte{0xff, 0xff, 0xff, 0xff, 0x0f}, want: -1},
		{name: "negative minimum", buf: []byte{0x80, 0x80, 0x80, 0x80, 0x08}, want: -2147483648},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, consumed, err := ReadVarInt(tt.buf)
			if err != nil {
				t.Fatalf("ReadVarInt(%v) error = %v", tt.buf, err)
			}
			if got != tt.want || consumed != len(tt.buf) {
				t.Fatalf("ReadVarInt(%v) = %d, %d; want %d, %d", tt.buf, got, consumed, tt.want, len(tt.buf))
			}
		})
	}
}

func TestProtocolPublicPacketHelpers(t *testing.T) {
	packet := []byte{0x03, 0x00, 0x01, 'x', 0x7f}
	payload, consumed, err := ReadPacket(packet)
	if err != nil {
		t.Fatalf("ReadPacket() error = %v", err)
	}
	if !bytes.Equal(payload, []byte{0x00, 0x01, 'x'}) || consumed != 4 {
		t.Fatalf("ReadPacket() = %v, %d; want payload and four consumed bytes", payload, consumed)
	}
	if _, _, err := ReadPacket([]byte{0x03, 0x00}); err == nil {
		t.Fatal("ReadPacket(incomplete) error = nil, want error")
	}

	value, consumed, err := ReadString([]byte{0x03, 'c', 'a', 't', 0x01})
	if err != nil {
		t.Fatalf("ReadString() error = %v", err)
	}
	if value != "cat" || consumed != 4 {
		t.Fatalf("ReadString() = %q, %d; want cat, 4", value, consumed)
	}
	if _, _, err := ReadString([]byte{0x03, 'c'}); err == nil {
		t.Fatal("ReadString(incomplete) error = nil, want error")
	}

	status, err := StatusResponsePacket("ok")
	if err != nil {
		t.Fatalf("StatusResponsePacket() error = %v", err)
	}
	wantStatus := []byte{0x06, 0x00, 0x04, '"', 'o', 'k', '"'}
	if !bytes.Equal(status, wantStatus) {
		t.Fatalf("StatusResponsePacket(ok) = %v, want %v", status, wantStatus)
	}
	if _, err := StatusResponsePacket(make(chan int)); err == nil {
		t.Fatal("StatusResponsePacket(channel) error = nil, want JSON error")
	}
}

func mcTestPacket(host string, tail ...byte) []byte {
	protocolVersion := byte(0x63)
	nextState := byte(0x02)
	extra := []byte(nil)
	if len(tail) > 0 {
		protocolVersion = tail[0]
	}
	if len(tail) > 1 {
		nextState = tail[1]
	}
	if len(tail) > 2 {
		extra = tail[2:]
	}
	payload := []byte{0x00, protocolVersion, byte(len(host))}
	payload = append(payload, host...)
	payload = append(payload, 0x63, 0xdd, nextState)
	payload = append(payload, extra...)
	packet := []byte{byte(len(payload))}
	return append(packet, payload...)
}
