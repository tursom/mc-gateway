// internal/gatewayconfig/config.go 定义运行态管理数据库可用前使用的静态 TOML 配置。

package gatewayconfig

type Config struct {
	Tcp       ProtocolConfig            `toml:"tcp"`
	Quic      QuicConfig                `toml:"quic"`
	Kcp       KcpConfig                 `toml:"kcp"`
	WebSocket WebSocketConfig           `toml:"websocket"`
	Log       LogConfig                 `toml:"log"`
	PidFile   string                    `toml:"pid_file"`
	Plugin    map[string]map[string]any `toml:"plugin"`
}

type ProtocolConfig struct {
	Enable bool `toml:"enable"`
	Port   int  `toml:"port"`
}

type KcpConfig struct {
	Enable       bool `toml:"enable"`
	Port         int  `toml:"port"`
	DataShards   int  `toml:"data_shards"`
	ParityShards int  `toml:"parity_Shards"`
}

type QuicConfig struct {
	Enable               bool     `toml:"enable"`
	Port                 int      `toml:"port"`
	ApplicationProtocols []string `toml:"application_protocols"`
}

type WebSocketConfig struct {
	Enable bool   `toml:"enable"`
	Port   int    `toml:"port"`
	Path   string `toml:"path"`
}

type LogConfig struct {
	Level string `toml:"level"`
	File  string `toml:"file"`
}
