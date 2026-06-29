// examples/plugins/mc-auth-proxy/main.go 演示托管插件如何拦截登录流量、发出认证事件并按条件拒绝客户端。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool {
			return p.config.MatchHost == "" || req.Host == p.config.MatchHost
		},
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			if p.config.MatchHost != "" && req.Host != p.config.MatchHost {
				return nil, api.ErrPass
			}
			gatewayEnd, pluginEnd := net.Pipe()
			go p.handleConn(req, pluginEnd)
			return gatewayEnd, nil
		},
	)
}

func (p *PluginImpl) handleConn(req api.UpstreamConnectRequest, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	handshakePacket, err := readPacketFromConn(conn)
	if err != nil {
		return
	}
	handshake := protocol.ParseHandshake(handshakePacket)
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
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	<-done
}
