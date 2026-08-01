// cmd/gateway/config_test.go 包含用于约束 config 行为的测试。

package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParseStartupConfigDefaultsAndEnv(t *testing.T) {
	cfg, err := parseStartupConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("parseStartupConfig() error = %v", err)
	}
	if cfg.DBPath != defaultAdminDBPath {
		t.Fatalf("DBPath = %q, want %q", cfg.DBPath, defaultAdminDBPath)
	}
	if cfg.TCPAdminPort != defaultTCPPort {
		t.Fatalf("TCPAdminPort = %d, want %d", cfg.TCPAdminPort, defaultTCPPort)
	}
	if cfg.AdminPath != defaultAdminPath {
		t.Fatalf("AdminPath = %q, want %q", cfg.AdminPath, defaultAdminPath)
	}
	if cfg.AdminAPIPrefix != defaultAdminAPIPrefix {
		t.Fatalf("AdminAPIPrefix = %q, want %q", cfg.AdminAPIPrefix, defaultAdminAPIPrefix)
	}

	env := map[string]string{
		adminEnvDB:                              "/tmp/mc.db",
		adminEnvTCPAdminPort:                    "25575",
		adminEnvPath:                            "/ops",
		adminEnvAPIPrefix:                       "/ops/api/",
		adminEnvPluginRequireConformanceFixture: "true",
	}
	cfg, err = parseStartupConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("parseStartupConfig(env) error = %v", err)
	}
	if cfg.DBPath != "/tmp/mc.db" || cfg.TCPAdminPort != 25575 || cfg.AdminPath != "/ops/" || cfg.AdminAPIPrefix != "/ops/api" || !cfg.PluginRequireConformanceFixture {
		t.Fatalf("startup config = %+v", cfg)
	}
}

func TestParseStartupConfigReturnsErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "invalid port",
			env:  map[string]string{adminEnvTCPAdminPort: "70000"},
		},
		{
			name: "invalid admin path",
			env:  map[string]string{adminEnvPath: "admin"},
		},
		{
			name: "invalid api prefix",
			env:  map[string]string{adminEnvAPIPrefix: "api"},
		},
		{
			name: "api prefix equals admin path",
			env: map[string]string{
				adminEnvPath:      "/admin",
				adminEnvAPIPrefix: "/admin",
			},
		},
		{
			name: "api prefix under js asset path",
			env:  map[string]string{adminEnvAPIPrefix: "/admin/js/api"},
		},
		{
			name: "api prefix under config asset path",
			env:  map[string]string{adminEnvAPIPrefix: "/admin/config.js/api"},
		},
		{
			name: "invalid plugin conformance gate",
			env:  map[string]string{adminEnvPluginRequireConformanceFixture: "maybe"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseStartupConfig(func(key string) string { return tt.env[key] })
			if err == nil {
				t.Fatal("parseStartupConfig() error = nil, want error")
			}
		})
	}
}

func TestLoadConfigInitializesSQLiteDefaults(t *testing.T) {
	defer saveGatewayState(t)()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "gateway.sqlite3")
	t.Setenv(adminEnvDB, dbPath)
	t.Setenv(adminEnvTCPAdminPort, "25575")
	t.Setenv(adminEnvPath, "/ops")
	t.Setenv(adminEnvAPIPrefix, "/ops/api")
	t.Setenv(adminEnvPluginRequireConformanceFixture, "true")

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if !config.Tcp.Enable || config.Tcp.Port != 25575 {
		t.Fatalf("tcp config = %+v", config.Tcp)
	}
	if config.Quic.Enable || config.Kcp.Enable || config.WebSocket.Enable {
		t.Fatalf("optional services should be disabled by default: quic=%+v kcp=%+v websocket=%+v", config.Quic, config.Kcp, config.WebSocket)
	}
	if config.Kcp.Port != defaultKCPPort || config.Kcp.DataShards != defaultKCPDataShards || config.Kcp.ParityShards != defaultKCPParityShards {
		t.Fatalf("kcp config = %+v", config.Kcp)
	}
	if adminStartup.DBPath != dbPath || adminStartup.AdminPath != "/ops/" || adminStartup.AdminAPIPrefix != "/ops/api" || !adminStartup.PluginRequireConformanceFixture {
		t.Fatalf("adminStartup = %+v", adminStartup)
	}
	if adminDB == nil {
		t.Fatal("adminDB = nil")
	}

	var enabled, port int
	if err := adminDB.QueryRow(`SELECT enabled, port FROM services WHERE name = ?`, serviceNameTCPAdmin).Scan(&enabled, &port); err != nil {
		t.Fatalf("query tcp_admin service: %v", err)
	}
	if enabled != 1 || port != 25575 {
		t.Fatalf("tcp_admin service enabled=%d port=%d, want enabled=1 port=25575", enabled, port)
	}
	listeners, err := pluginIngressReservedListeners(context.Background(), adminDB)
	if err != nil {
		t.Fatalf("pluginIngressReservedListeners() error = %v", err)
	}
	if len(listeners) != 1 || listeners[0].Name != serviceNameTCPAdmin || listeners[0].Network != "tcp" || listeners[0].Port != 25575 || !listeners[0].Enabled {
		t.Fatalf("reserved listeners = %+v, want tcp_admin tcp/25575", listeners)
	}
}

func TestLoadConfigCreatesInitialAdminFromEnv(t *testing.T) {
	defer saveGatewayState(t)()

	t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
	t.Setenv(adminEnvInitialPassword, "secret")

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	user, err := authenticateUser(t.Context(), "admin", "secret")
	if err != nil {
		t.Fatalf("authenticateUser() error = %v", err)
	}
	if user.Role != adminRoleAdmin {
		t.Fatalf("admin role = %q, want %q", user.Role, adminRoleAdmin)
	}
}

func TestLoadConfigReturnsEnvErrors(t *testing.T) {
	t.Run("invalid port", func(t *testing.T) {
		defer saveGatewayState(t)()

		t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
		t.Setenv(adminEnvTCPAdminPort, "0")
		if err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want error")
		}
	})

	t.Run("invalid admin path", func(t *testing.T) {
		defer saveGatewayState(t)()

		t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
		t.Setenv(adminEnvPath, "admin")
		if err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want error")
		}
	})
}
