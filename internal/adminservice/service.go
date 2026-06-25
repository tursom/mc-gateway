package adminservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	NameTCPAdmin  = "tcp_admin"
	NameKCP       = "kcp"
	NameQUIC      = "quic"
	NameWebSocket = "websocket"

	DefaultKCPPort         = 25566
	DefaultKCPDataShards   = 10
	DefaultKCPParityShards = 3
	DefaultQUICPort        = 25565
	DefaultWebSocketPort   = 25566
	DefaultWebSocketPath   = "/"
)

var defaultQUICApplicationProtocols = []string{"minecraft", "quic", "raw", "h3"}

type Record struct {
	Name            string         `json:"name"`
	Enabled         bool           `json:"enabled"`
	Port            int            `json:"port"`
	Options         map[string]any `json:"options"`
	RestartRequired bool           `json:"restart_required"`
	CreatedAt       int64          `json:"created_at"`
	UpdatedAt       int64          `json:"updated_at"`
	UpdatedBy       string         `json:"updated_by"`
	Running         bool           `json:"running"`
}

func DefaultRecords(tcpAdminPort int) []Record {
	return []Record{
		{Name: NameTCPAdmin, Enabled: true, Port: tcpAdminPort, Options: map[string]any{}},
		{Name: NameKCP, Enabled: false, Port: DefaultKCPPort, Options: map[string]any{
			"data_shards":   DefaultKCPDataShards,
			"parity_shards": DefaultKCPParityShards,
		}},
		{Name: NameQUIC, Enabled: false, Port: DefaultQUICPort, Options: map[string]any{
			"application_protocols": append([]string(nil), defaultQUICApplicationProtocols...),
		}},
		{Name: NameWebSocket, Enabled: false, Port: DefaultWebSocketPort, Options: map[string]any{
			"path": DefaultWebSocketPath,
		}},
	}
}

func DefaultPort(name string, tcpAdminPort int) int {
	switch name {
	case NameTCPAdmin:
		return tcpAdminPort
	case NameKCP:
		return DefaultKCPPort
	case NameQUIC:
		return DefaultQUICPort
	case NameWebSocket:
		return DefaultWebSocketPort
	default:
		return 0
	}
}

func ValidateUpdate(name string, enabled bool, port int) error {
	if !IsKnown(name) {
		return fmt.Errorf("unknown service %q", name)
	}
	if port < 1 || port > 65535 {
		return errors.New("port must be an integer from 1 to 65535")
	}
	if name == NameTCPAdmin && !enabled {
		return errors.New("tcp_admin cannot be disabled")
	}
	return nil
}

func IsKnown(name string) bool {
	switch name {
	case NameTCPAdmin, NameKCP, NameQUIC, NameWebSocket:
		return true
	default:
		return false
	}
}

func DecodeOptions(optionsJSON string) map[string]any {
	options := map[string]any{}
	if strings.TrimSpace(optionsJSON) == "" {
		return options
	}
	if err := json.Unmarshal([]byte(optionsJSON), &options); err != nil {
		return map[string]any{}
	}
	return options
}

func NormalizeOptions(name string, options map[string]any) map[string]any {
	if options == nil {
		options = map[string]any{}
	}
	normalized := map[string]any{}
	for key, value := range options {
		normalized[key] = value
	}

	switch name {
	case NameKCP:
		if IntOption(normalized, "data_shards", 0) <= 0 {
			normalized["data_shards"] = DefaultKCPDataShards
		}
		if IntOption(normalized, "parity_shards", 0) <= 0 {
			normalized["parity_shards"] = DefaultKCPParityShards
		}
	case NameQUIC:
		if len(StringSliceOption(normalized, "application_protocols")) == 0 {
			normalized["application_protocols"] = append([]string(nil), defaultQUICApplicationProtocols...)
		}
	case NameWebSocket:
		p := StringOption(normalized, "path", DefaultWebSocketPath)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		normalized["path"] = p
	}

	return normalized
}

func IntOption(options map[string]any, key string, fallback int) int {
	value, ok := options[key]
	if !ok {
		return fallback
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		i, err := strconv.Atoi(v)
		if err == nil {
			return i
		}
	}
	return fallback
}

func StringOption(options map[string]any, key, fallback string) string {
	value, ok := options[key]
	if !ok {
		return fallback
	}
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	return fallback
}

func StringSliceOption(options map[string]any, key string) []string {
	value, ok := options[key]
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
