package main

import (
	"bytes"
	"net"
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestFixtureRejectsLoginStart(t *testing.T) {
	plugin := &PluginImpl{}
	if err := plugin.ReloadConfig(&Config{DisconnectMessage: "fixture rejected"}); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		plugin.handleConn(upstreamRequestForTest(), server)
		close(done)
	}()

	if _, err := client.Write(mcAuthProxyHandshakePacket("play.example")); err != nil {
		t.Fatalf("write handshake error = %v", err)
	}
	if _, err := client.Write(mcAuthProxyLoginStartPacket("Steve")); err != nil {
		t.Fatalf("write login start error = %v", err)
	}
	response, err := readPacketFromConn(client)
	if err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if !bytes.Contains(response, []byte("fixture rejected")) {
		t.Fatalf("response = %q, want fixture message", response)
	}
	_ = client.Close()
	<-done
}

func TestParseLoginStart(t *testing.T) {
	login, err := parseLoginStart(mcAuthProxyLoginStartPacket("Alex"))
	if err != nil {
		t.Fatalf("parseLoginStart() error = %v", err)
	}
	if login.Username != "Alex" {
		t.Fatalf("username = %q, want Alex", login.Username)
	}
}

func upstreamRequestForTest() api.UpstreamConnectRequest {
	return api.UpstreamConnectRequest{}
}

func mcAuthProxyHandshakePacket(host string) []byte {
	payload := []byte{0x00, 0x63, byte(len(host))}
	payload = append(payload, host...)
	payload = append(payload, 0x63, 0xdd, 0x02)
	return append(encodeVarInt(len(payload)), payload...)
}

func mcAuthProxyLoginStartPacket(username string) []byte {
	payload := []byte{0x00}
	payload = append(payload, encodeVarInt(len(username))...)
	payload = append(payload, username...)
	return append(encodeVarInt(len(payload)), payload...)
}
