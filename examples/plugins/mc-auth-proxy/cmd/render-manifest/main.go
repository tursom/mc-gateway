package main

import (
	"encoding/json"
	"os"
	"runtime"
)

func main() {
	artifactType := os.Getenv("ARTIFACT_TYPE")
	if artifactType == "" {
		artifactType = "binary"
	}
	manifest := map[string]any{
		"schema_version": "mc-gateway.plugin/v1",
		"id":             "mc-auth-proxy",
		"name":           "Minecraft Auth Proxy",
		"version":        "0.1.0",
		"description":    "Protocol-proxy example that reads handshake/login start and returns a login disconnect fixture.",
		"artifact_type":  artifactType,
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
			"upstream_connect": map[string]any{"mode": "protocol-proxy"},
			"minecraft": map[string]any{
				"protocol_versions": map[string]any{
					"min":                760,
					"max":                767,
					"tested":             []int{760, 763, 765, 767},
					"unsupported_policy": "kick",
				},
				"states": map[string]any{
					"status":        "transparent",
					"login":         "handled",
					"configuration": "transparent",
					"play":          "transparent",
				},
				"auth_modes": []string{"fixture"},
				"forwarding": map[string]any{
					"supported":       []string{"none", "velocity-modern"},
					"default":         "none",
					"requires_secret": false,
				},
				"unsupported_policy": "kick",
				"modded": map[string]any{
					"forge":   "transparent",
					"fabric":  "transparent",
					"unknown": "pass",
				},
			},
			"network":    map[string]any{"outbound": []string{"tcp:*:*"}},
			"filesystem": map[string]any{"read": []string{}, "write": []string{}},
			"env":        []string{},
		},
		"runtime_limits": map[string]any{
			"handler_timeout_ms":       3000,
			"initial_write_timeout_ms": 1000,
		},
		"config_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"match_host":         map[string]any{"type": "string"},
				"fixture_accept":     map[string]any{"type": "boolean"},
				"disconnect_message": map[string]any{"type": "string"},
				"backend":            map[string]any{"type": "string"},
			},
		},
	}
	if artifactType == "source" {
		manifest["build"] = map[string]any{
			"type":            "go",
			"entry":           ".",
			"go_version":      runtime.Version(),
			"cgo_enabled":     true,
			"tags":            []string{},
			"vendor_required": false,
			"output":          "plugin.so",
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		panic(err)
	}
}
