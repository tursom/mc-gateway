// internal/adminservice/service.go 在原始服务仓库之上应用服务校验、默认值和更新语义。

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

// DefaultRecords 给新数据库写入可管理的监听服务。只有 TCP/Admin 默认启用，
// 其他传输保留配置但不自动开放端口。
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

// DefaultPort 返回服务的默认端口；TCP/Admin 使用启动配置传入的端口。
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

// ValidateUpdate 校验管理端提交的服务更新。TCP/Admin 是控制面入口，不能禁用。
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

// IsKnown 判断服务名是否属于当前网关支持的内置监听服务。
func IsKnown(name string) bool {
	switch name {
	case NameTCPAdmin, NameKCP, NameQUIC, NameWebSocket:
		return true
	default:
		return false
	}
}

// DecodeOptions 容错解析 JSON 选项；坏数据不会让整个服务列表不可读。
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

// NormalizeOptions 为不同服务补齐选项默认值，并修正 WebSocket path 这种可恢复输入。
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

// IntOption 从 JSON 解码后的 map 中读取整数，兼容 number 和字符串形式。
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

// StringOption 从 JSON 选项中读取非空字符串。
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

// StringSliceOption 从 JSON 选项中读取字符串数组，兼容 []any 的解码结果。
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
