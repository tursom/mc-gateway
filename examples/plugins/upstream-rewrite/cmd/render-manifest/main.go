package main

import (
	"encoding/json"
	"os"
	"runtime"
)

func main() {
	manifest := map[string]any{
		"schema_version": "mc-gateway.plugin/v1",
		"id":             "upstream-rewrite",
		"name":           "Upstream Rewrite",
		"version":        "0.1.0",
		"description":    "Rewrite selected upstream targets before dialing.",
		"artifact_type":  "binary",
		"runtime": map[string]any{
			"type":            "go-plugin",
			"entry":           "plugin.so",
			"entry_symbol":    "Plugin",
			"metadata_symbol": "MCGatewayPluginMetadata",
		},
		"api_version":        "plugin-api/v1",
		"sdk_module":         "github.com/tursom/mc-gateway/plugin/api",
		"sdk_module_version": "v0.1.0",
		"go_version":         runtime.Version(),
		"go_os":              runtime.GOOS,
		"go_arch":            runtime.GOARCH,
		"extension_points": []map[string]any{
			{"type": "hook", "key": "upstream.connect/v1"},
		},
		"capabilities": map[string]any{
			"extension_points": []string{"upstream.connect/v1"},
			"network":          map[string]any{"outbound": []string{"tcp:*:*"}},
			"filesystem":       map[string]any{"read": []string{}, "write": []string{}},
			"env":              []string{},
		},
		"runtime_limits": map[string]any{
			"handler_timeout_ms": 3000,
		},
		"config_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"match_host": map[string]any{"type": "string"},
				"upstream":   map[string]any{"type": "string"},
			},
			"required": []string{"upstream"},
		},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		panic(err)
	}
}
