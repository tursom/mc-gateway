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

	env := map[string]string{
		EnvDB:           "/tmp/mc.db",
		EnvTCPAdminPort: "25575",
		EnvPath:         "/ops",
		EnvAPIPrefix:    "/ops/api/",
	}
	cfg, err = Parse(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("Parse(env) error = %v", err)
	}
	if cfg.DBPath != "/tmp/mc.db" || cfg.TCPAdminPort != 25575 || cfg.AdminPath != "/ops/" || cfg.AdminAPIPrefix != "/ops/api" {
		t.Fatalf("config = %+v", cfg)
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
