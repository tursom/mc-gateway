package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/protocol"
)

type PluginImpl struct {
	api.AbstractPlugin
	config Config
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

func MCGatewayPluginMetadata() string {
	return manifestJSON
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
		_ = writeLoginDisconnect(conn, p.config.DisconnectMessage)
		return
	}

	if !p.config.FixtureAccept {
		_ = writeLoginDisconnect(conn, p.config.DisconnectMessage)
		return
	}
	if p.config.Backend == "" {
		_ = writeLoginDisconnect(conn, "Fixture accepted but no backend is configured")
		return
	}

	backend, err := net.Dial("tcp", p.config.Backend)
	if err != nil {
		_ = writeLoginDisconnect(conn, "Backend unavailable")
		return
	}
	defer backend.Close()
	_, _ = backend.Write(handshakePacket)
	_, _ = backend.Write(loginPacket)
	copyBoth(conn, backend)
	_ = req
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

var manifestJSON = compactJSON(map[string]any{
	"schema_version": "mc-gateway.plugin/v1",
	"id":             "mc-auth-proxy",
	"name":           "Minecraft Auth Proxy",
	"version":        "0.1.0",
	"description":    "Protocol-proxy example that reads handshake/login start and returns a login disconnect fixture.",
	"artifact_type":  "binary",
	"runtime": map[string]any{
		"type":            "go-plugin",
		"entry":           "plugin.so",
		"entry_symbol":    "Plugin",
		"metadata_symbol": "MCGatewayPluginMetadata",
	},
	"api_version": "plugin-api/v1",
	"sdk_module":  "github.com/tursom/mc-gateway/plugin/api",
	"go_version":  runtime.Version(),
	"go_os":       runtime.GOOS,
	"go_arch":     runtime.GOARCH,
	"extension_points": []map[string]any{
		{"type": "hook", "key": "upstream.connect/v1"},
	},
	"capabilities": map[string]any{
		"extension_points": []string{"upstream.connect/v1"},
		"upstream_connect": map[string]any{"mode": "protocol-proxy"},
		"minecraft": map[string]any{
			"protocol_versions":  map[string]any{"min": 760, "max": 767, "tested": []int{760, 763, 765, 767}, "unsupported_policy": "kick"},
			"states":             map[string]any{"status": "transparent", "login": "handled", "configuration": "transparent", "play": "transparent"},
			"auth_modes":         []string{"fixture"},
			"forwarding":         map[string]any{"supported": []string{"none", "velocity-modern"}, "default": "none", "requires_secret": false},
			"unsupported_policy": "kick",
			"modded":             map[string]any{"forge": "transparent", "fabric": "transparent", "unknown": "pass"},
		},
		"network": map[string]any{"outbound": []string{"tcp:*:*"}},
	},
	"runtime_limits": map[string]any{
		"handler_timeout_ms":       3000,
		"initial_write_timeout_ms": 1000,
	},
})

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
