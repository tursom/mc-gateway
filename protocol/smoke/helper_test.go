// protocol/smoke/helper_test.go 约束协议 smoke fixture 本身的 Minecraft packet 和 TCP 行为。

package smoke

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/protocol"
)

func TestMinecraftSmokePacketsAreParseable(t *testing.T) {
	for _, protocolVersion := range []int{47, 340, 498, 763, 767} {
		handshake := MinecraftHandshakePacketVersion(protocolVersion, "play.example")
		parsed := protocol.ParseHandshake(handshake)
		if parsed.ServerHost != "play.example" || parsed.ProtocolVersion != protocolVersion || parsed.NextState != 2 {
			t.Fatalf("handshake protocol %d = %+v, want login handshake for play.example", protocolVersion, parsed)
		}
	}

	payload, _, err := protocol.ReadPacket(MinecraftLoginStartPacket("Steve"))
	if err != nil {
		t.Fatalf("ReadPacket(login) error = %v", err)
	}
	packetID, n, err := protocol.ReadVarInt(payload)
	if err != nil {
		t.Fatalf("ReadVarInt(login id) error = %v", err)
	}
	username, _, err := protocol.ReadString(payload[n:])
	if err != nil {
		t.Fatalf("ReadString(login username) error = %v", err)
	}
	if packetID != 0 || username != "Steve" {
		t.Fatalf("login packet id=%d username=%q, want id=0 username=Steve", packetID, username)
	}
}

func TestLoopbackTCPPairSupportsHalfCloseAndBackpressure(t *testing.T) {
	server, client, err := NewLoopbackTCPPair()
	if err != nil {
		t.Fatalf("NewLoopbackTCPPair() error = %v", err)
	}
	defer server.Close()
	defer client.Close()
	deadline := time.Now().Add(2 * time.Second)
	_ = server.SetDeadline(deadline)
	_ = client.SetDeadline(deadline)

	payload := MinecraftPayloadPacket(1, bytes.Repeat([]byte("x"), 128*1024))
	readDone := make(chan []byte, 1)
	go func() {
		packet, err := ReadPacketFromConn(server)
		if err != nil {
			t.Errorf("ReadPacketFromConn(server) error = %v", err)
			readDone <- nil
			return
		}
		if _, err := server.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("server read after client half-close = %v, want EOF", err)
		}
		_, _ = server.Write(MinecraftPayloadPacket(2, []byte("ack")))
		_ = server.CloseWrite()
		readDone <- packet
	}()

	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write payload error = %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite() error = %v", err)
	}
	if got := <-readDone; !bytes.Equal(got, payload) {
		t.Fatalf("server packet length = %d, want %d", len(got), len(payload))
	}
	ack, err := ReadPacketFromConn(client)
	if err != nil {
		t.Fatalf("ReadPacketFromConn(client ack) error = %v", err)
	}
	if !bytes.Equal(ack, MinecraftPayloadPacket(2, []byte("ack"))) {
		t.Fatalf("ack = %v, want ack packet", ack)
	}
}
