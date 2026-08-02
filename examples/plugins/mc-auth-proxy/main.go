// examples/plugins/mc-auth-proxy/main.go 演示托管插件如何拦截登录流量、发出认证事件并按条件拒绝客户端。

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
)

type PluginImpl struct {
	api.AbstractPlugin
	config  Config
	gateway api.Gateway
}

type Config struct {
	MatchHost         string `json:"match_host"`
	FixtureAccept     bool   `json:"fixture_accept"`
	DisconnectMessage string `json:"disconnect_message"`
	Backend           string `json:"backend"`
}

type loginStart struct {
	Username string
}

func Plugin() api.Plugin {
	return &PluginImpl{}
}

func (p *PluginImpl) NewConfigObj() any {
	return &Config{}
}

func (p *PluginImpl) ReloadConfig(config any) error {
	if cfg, ok := config.(*Config); ok {
		p.config = *cfg
	}
	if p.config.DisconnectMessage == "" {
		p.config.DisconnectMessage = "Authentication fixture rejected the login"
	}
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	p.gateway = gateway
	return api.RegisterUpstreamConnectHandlerV2(gateway, func(req api.UpstreamConnectRequestV2) error {
		p.handleConn(req, req.Connection.Stream)
		return nil
	})
}

func (p *PluginImpl) handleConn(req api.UpstreamConnectRequestV2, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	handshakePacket, err := readPacketFromConn(conn)
	if err != nil {
		return
	}
	handshake := protocol.ParseHandshake(handshakePacket)
	if p.config.MatchHost != "" && handshake.ServerHost != p.config.MatchHost {
		state := req.Connection
		state.Stream = &authReplayConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(handshakePacket), conn)}
		_ = req.Flow.Next(state)
		return
	}
	if handshake.ServerHost == "" || handshake.NextState != 2 {
		p.emitAuthEvent(req.Context, "auth.failure", "bad_handshake")
		_ = writeLoginDisconnect(conn, "Unsupported Minecraft handshake")
		return
	}

	loginPacket, err := readPacketFromConn(conn)
	if err != nil {
		_ = writeLoginDisconnect(conn, p.config.DisconnectMessage)
		return
	}
	login, err := parseLoginStart(loginPacket)
	if err != nil || login.Username == "" {
		p.emitAuthEvent(req.Context, "auth.failure", "bad_login_start")
		_ = writeLoginDisconnect(conn, p.config.DisconnectMessage)
		return
	}

	if !p.config.FixtureAccept {
		p.emitAuthEvent(req.Context, "auth.failure", "fixture_reject")
		_ = writeLoginDisconnect(conn, p.config.DisconnectMessage)
		return
	}
	if p.config.Backend == "" {
		p.emitAuthEvent(req.Context, "auth.failure", "backend_missing")
		_ = writeLoginDisconnect(conn, "Fixture accepted but no backend is configured")
		return
	}

	backend, err := p.gateway.ExternalClient("backend").DialTCP(req.Context, p.config.Backend, 3*time.Second)
	if err != nil {
		p.emitAuthEvent(req.Context, "auth.failure", "backend_unavailable")
		_ = writeLoginDisconnect(conn, "Backend unavailable")
		return
	}
	p.emitAuthEvent(req.Context, "auth.success", "fixture_accept")
	defer backend.Close()
	_, _ = backend.Write(handshakePacket)
	_, _ = backend.Write(loginPacket)
	copyBoth(conn, backend)
	_ = req
}

type authReplayConn struct {
	net.Conn
	reader io.Reader
}

func (c *authReplayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *authReplayConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *authReplayConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
}

func (p *PluginImpl) emitAuthEvent(ctx context.Context, name, result string) {
	if p.gateway == nil {
		return
	}
	_ = p.gateway.EmitEvent(ctx, name, map[string]string{
		"result": result,
		"mode":   "fixture",
	})
	_ = p.gateway.ObserveMetric(ctx, "auth.attempts", 1, map[string]string{
		"result": result,
		"mode":   "fixture",
	})
	p.gateway.Logger().Info(ctx, name, map[string]string{"result": result})
}

func readPacketFromConn(conn net.Conn) ([]byte, error) {
	length, err := readVarIntFromConn(conn)
	if err != nil {
		return nil, err
	}
	if length <= 0 || length > 2*1024*1024 {
		return nil, fmt.Errorf("invalid packet length %d", length)
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(conn, packet); err != nil {
		return nil, err
	}
	out := append(encodeVarInt(length), packet...)
	return out, nil
}

func parseLoginStart(packet []byte) (loginStart, error) {
	payload, _, err := protocol.ReadPacket(packet)
	if err != nil {
		return loginStart{}, err
	}
	packetID, n, err := protocol.ReadVarInt(payload)
	if err != nil {
		return loginStart{}, err
	}
	if packetID != 0 {
		return loginStart{}, errors.New("not a login start packet")
	}
	username, _, err := protocol.ReadString(payload[n:])
	if err != nil {
		return loginStart{}, err
	}
	return loginStart{Username: username}, nil
}

func writeLoginDisconnect(conn net.Conn, message string) error {
	payload := []byte{0x00}
	text, _ := json.Marshal(map[string]any{"text": message})
	payload = append(payload, encodeVarInt(len(text))...)
	payload = append(payload, text...)
	packet := append(encodeVarInt(len(payload)), payload...)
	_, err := conn.Write(packet)
	return err
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
	return 0, errors.New("varint too long")
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

func copyBoth(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
		return
	}
	_ = conn.Close()
}
