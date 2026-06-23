package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPluginConfig(t *testing.T) {
	defer saveGatewayState(t)()

	type pluginConfig struct {
		Enable bool   `toml:"enable"`
		Name   string `toml:"name"`
		Count  int    `toml:"count"`
	}

	var got pluginConfig
	err := loadPluginConfig(map[string]any{
		"enable": true,
		"name":   "plugin-a",
		"count":  7,
	}, &got)
	if err != nil {
		t.Fatalf("loadPluginConfig() error = %v", err)
	}

	want := pluginConfig{Enable: true, Name: "plugin-a", Count: 7}
	if got != want {
		t.Fatalf("loadPluginConfig() = %+v, want %+v", got, want)
	}
}

func TestLoadPluginConfigReturnsDecodeError(t *testing.T) {
	defer saveGatewayState(t)()

	type pluginConfig struct {
		Count int `toml:"count"`
	}

	var got pluginConfig
	if err := loadPluginConfig(map[string]any{"count": "not-an-int"}, &got); err == nil {
		t.Fatal("loadPluginConfig() error = nil, want error")
	}
}

func TestLoadConfigReadsTomlAndAppliesSideEffects(t *testing.T) {
	defer saveGatewayState(t)()

	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "gateway.pid")
	logPath := filepath.Join(tmpDir, "logs", "gateway.log")
	configFile = filepath.Join(tmpDir, "config.toml")

	toml := fmt.Sprintf(`
pid_file = %q

[log]
level = "debug"
file = %q

[tcp]
enable = true
port = 25565

[quic]
enable = true
port = 25566
application_protocols = ["minecraft", "raw"]

[kcp]
enable = true
port = 25567
data_shards = 10
parity_Shards = 3

[websocket]
enable = true
port = 25568
path = "/gateway"

[hosts]
"play.example" = "backend.example:25565"
default = "fallback.example:25565"

[plugin.disabled]
enable = false
`, pidPath, logPath)

	if err := os.WriteFile(configFile, []byte(toml), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if !config.Tcp.Enable || config.Tcp.Port != 25565 {
		t.Fatalf("tcp config = %+v", config.Tcp)
	}
	if !config.Quic.Enable || config.Quic.Port != 25566 {
		t.Fatalf("quic config = %+v", config.Quic)
	}
	if got := strings.Join(config.Quic.ApplicationProtocols, ","); got != "minecraft,raw" {
		t.Fatalf("application protocols = %q, want minecraft,raw", got)
	}
	if !config.Kcp.Enable || config.Kcp.DataShards != 10 || config.Kcp.ParityShards != 3 {
		t.Fatalf("kcp config = %+v", config.Kcp)
	}
	if !config.WebSocket.Enable || config.WebSocket.Path != "/gateway" {
		t.Fatalf("websocket config = %+v", config.WebSocket)
	}
	if got := config.Hosts["play.example"]; got != "backend.example:25565" {
		t.Fatalf("host route = %q, want backend.example:25565", got)
	}
	if currentPidFile != pidPath {
		t.Fatalf("currentPidFile = %q, want %q", currentPidFile, pidPath)
	}
	if currentLogFile != logPath {
		t.Fatalf("currentLogFile = %q, want %q", currentLogFile, logPath)
	}

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("ReadFile(pid) error = %v", err)
	}
	if wantPID := fmt.Sprintf("%d\n", os.Getpid()); string(pidBytes) != wantPID {
		t.Fatalf("pid file = %q, want %q", string(pidBytes), wantPID)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("Stat(log file) error = %v", err)
	}
}

func TestLoadConfigReturnsErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		defer saveGatewayState(t)()

		configFile = filepath.Join(t.TempDir(), "missing.toml")
		if err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want error")
		}
	})

	t.Run("invalid toml", func(t *testing.T) {
		defer saveGatewayState(t)()

		configFile = filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(configFile, []byte("[tcp\n"), 0644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want error")
		}
	})
}
