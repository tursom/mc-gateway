// internal/gatewayconfig/plugin_test.go 包含用于约束 plugin 行为的测试。

package gatewayconfig

import "testing"

func TestDecodePluginConfig(t *testing.T) {
	type pluginConfig struct {
		Enable bool   `toml:"enable"`
		Name   string `toml:"name"`
		Count  int    `toml:"count"`
	}

	var got pluginConfig
	err := DecodePluginConfig(map[string]any{
		"enable": true,
		"name":   "plugin-a",
		"count":  7,
	}, &got)
	if err != nil {
		t.Fatalf("DecodePluginConfig() error = %v", err)
	}

	want := pluginConfig{Enable: true, Name: "plugin-a", Count: 7}
	if got != want {
		t.Fatalf("DecodePluginConfig() = %+v, want %+v", got, want)
	}
}

func TestDecodePluginConfigReturnsDecodeError(t *testing.T) {
	type pluginConfig struct {
		Count int `toml:"count"`
	}

	var got pluginConfig
	if err := DecodePluginConfig(map[string]any{"count": "not-an-int"}, &got); err == nil {
		t.Fatal("DecodePluginConfig() error = nil, want error")
	}
}
