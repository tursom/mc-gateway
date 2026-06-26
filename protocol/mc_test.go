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
