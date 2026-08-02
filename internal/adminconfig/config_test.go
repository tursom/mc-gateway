// internal/adminconfig/config_test.go 包含用于约束 config 行为的测试。

package adminconfig

import "testing"

func TestParseDefaultsAndEnv(t *testing.T) {
	cfg, err := Parse(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.DBPath != DefaultDBPath {
		t.Fatalf("DBPath = %q, want %q", cfg.DBPath, DefaultDBPath)
	}
	if cfg.TCPAdminPort != DefaultTCPAdminPort {
		t.Fatalf("TCPAdminPort = %d, want %d", cfg.TCPAdminPort, DefaultTCPAdminPort)
	}
	if cfg.AdminPath != DefaultAdminPath {
		t.Fatalf("AdminPath = %q, want %q", cfg.AdminPath, DefaultAdminPath)
	}
	if cfg.AdminAPIPrefix != DefaultAdminAPIPrefix {
		t.Fatalf("AdminAPIPrefix = %q, want %q", cfg.AdminAPIPrefix, DefaultAdminAPIPrefix)
	}
	if cfg.PrometheusMode != PrometheusModeShared {
		t.Fatalf("PrometheusMode = %q, want %q", cfg.PrometheusMode, PrometheusModeShared)
	}
	if cfg.PrometheusListenAddr != DefaultPrometheusListenAddr {
		t.Fatalf("PrometheusListenAddr = %q, want %q", cfg.PrometheusListenAddr, DefaultPrometheusListenAddr)
	}
	if cfg.PrometheusBearerToken != "" {
		t.Fatalf("PrometheusBearerToken = %q, want empty", cfg.PrometheusBearerToken)
	}

	env := map[string]string{
		EnvDB:                              "/tmp/mc.db",
		EnvTCPAdminPort:                    "25575",
		EnvPath:                            "/ops",
		EnvAPIPrefix:                       "/ops/api/",
		EnvPluginRequireConformanceFixture: "true",
		EnvPrometheusMode:                  string(PrometheusModeDedicated),
		EnvPrometheusListenAddr:            "[::1]:9201",
		EnvPrometheusBearerToken:           "metrics-secret",
	}
	cfg, err = Parse(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("Parse(env) error = %v", err)
	}
	if cfg.DBPath != "/tmp/mc.db" || cfg.TCPAdminPort != 25575 || cfg.AdminPath != "/ops/" || cfg.AdminAPIPrefix != "/ops/api" || !cfg.PluginRequireConformanceFixture ||
		cfg.PrometheusMode != PrometheusModeDedicated || cfg.PrometheusListenAddr != "[::1]:9201" || cfg.PrometheusBearerToken != "metrics-secret" {
		t.Fatalf("config = %+v", cfg)
	}

	cfg, err = Parse(func(key string) string {
		if key == EnvPrometheusMode {
			return string(PrometheusModeDisabled)
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Parse(disabled) error = %v", err)
	}
	if cfg.PrometheusMode != PrometheusModeDisabled {
		t.Fatalf("PrometheusMode = %q, want %q", cfg.PrometheusMode, PrometheusModeDisabled)
	}
}

func TestParseReturnsErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "invalid port",
			env:  map[string]string{EnvTCPAdminPort: "70000"},
		},
		{
			name: "invalid admin path",
			env:  map[string]string{EnvPath: "admin"},
		},
		{
			name: "invalid api prefix",
			env:  map[string]string{EnvAPIPrefix: "api"},
		},
		{
			name: "api prefix equals admin path",
			env: map[string]string{
				EnvPath:      "/admin",
				EnvAPIPrefix: "/admin",
			},
		},
		{
			name: "api prefix under js asset path",
			env:  map[string]string{EnvAPIPrefix: "/admin/js/api"},
		},
		{
			name: "api prefix under config asset path",
			env:  map[string]string{EnvAPIPrefix: "/admin/config.js/api"},
		},
		{
			name: "invalid plugin conformance gate",
			env:  map[string]string{EnvPluginRequireConformanceFixture: "maybe"},
		},
		{
			name: "invalid prometheus mode",
			env:  map[string]string{EnvPrometheusMode: "public"},
		},
		{
			name: "invalid prometheus listen address",
			env: map[string]string{
				EnvPrometheusMode:       string(PrometheusModeDedicated),
				EnvPrometheusListenAddr: "not-an-address",
			},
		},
		{
			name: "shared prometheus conflicts with admin path",
			env:  map[string]string{EnvPath: "/metrics"},
		},
		{
			name: "shared prometheus conflicts with admin API prefix",
			env:  map[string]string{EnvAPIPrefix: "/metrics"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(func(key string) string { return tt.env[key] })
			if err == nil {
				t.Fatal("Parse() error = nil, want error")
			}
		})
	}
}
