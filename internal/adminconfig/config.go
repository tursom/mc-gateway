// internal/adminconfig/config.go 在 SQLite 中保存服务级运行态配置，并用类型化方法包装 JSON 选项。

package adminconfig

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDBPath         = "mc-gateway.sqlite3"
	DefaultTCPAdminPort   = 25565
	DefaultAdminPath      = "/admin/"
	DefaultAdminAPIPrefix = "/admin/api"
	DefaultSessionTTL     = 8 * time.Hour

	EnvDB              = "MC_GATEWAY_DB"
	EnvTCPAdminPort    = "MC_GATEWAY_TCP_ADMIN_PORT"
	EnvPath            = "MC_GATEWAY_ADMIN_PATH"
	EnvAPIPrefix       = "MC_GATEWAY_ADMIN_API_PREFIX"
	EnvInitialPassword = "MC_GATEWAY_ADMIN_PASSWORD"
)

type Config struct {
	DBPath         string
	TCPAdminPort   int
	AdminPath      string
	AdminAPIPrefix string
	SessionTTL     time.Duration
}

func Default() Config {
	return Config{
		DBPath:         DefaultDBPath,
		TCPAdminPort:   DefaultTCPAdminPort,
		AdminPath:      DefaultAdminPath,
		AdminAPIPrefix: DefaultAdminAPIPrefix,
		SessionTTL:     DefaultSessionTTL,
	}
}

func Parse(getenv func(string) string) (Config, error) {
	cfg := Default()

	if dbPath := strings.TrimSpace(getenv(EnvDB)); dbPath != "" {
		cfg.DBPath = dbPath
	}

	port, err := parseOptionalPort(getenv(EnvTCPAdminPort), DefaultTCPAdminPort, EnvTCPAdminPort)
	if err != nil {
		return Config{}, err
	}
	cfg.TCPAdminPort = port

	adminPath, err := normalizeAdminPath(getenv(EnvPath))
	if err != nil {
		return Config{}, err
	}
	cfg.AdminPath = adminPath

	apiPrefix, err := normalizeAdminAPIPrefix(getenv(EnvAPIPrefix))
	if err != nil {
		return Config{}, err
	}
	cfg.AdminAPIPrefix = apiPrefix

	if err := validateAdminPaths(cfg.AdminPath, cfg.AdminAPIPrefix); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func parseOptionalPort(value string, fallback int, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be an integer from 1 to 65535", name)
	}
	return port, nil
}

func normalizeAdminPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultAdminPath
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%s must start with /", EnvPath)
	}

	cleaned := path.Clean(value)
	if cleaned == "." {
		cleaned = "/"
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	if !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned, nil
}

func normalizeAdminAPIPrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultAdminAPIPrefix
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%s must start with /", EnvAPIPrefix)
	}

	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("%s must not be /", EnvAPIPrefix)
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return strings.TrimRight(cleaned, "/"), nil
}

func validateAdminPaths(adminPath, apiPrefix string) error {
	adminRoot := strings.TrimRight(adminPath, "/")
	if apiPrefix == adminRoot {
		return errors.New("admin API prefix cannot equal admin page path")
	}

	for _, asset := range []string{"app.css", "config.js", "js"} {
		assetPath := strings.TrimRight(adminPath, "/") + "/" + asset
		if apiPrefix == assetPath || strings.HasPrefix(apiPrefix+"/", assetPath+"/") {
			return errors.New("admin API prefix cannot be under static asset path")
		}
	}
	return nil
}
