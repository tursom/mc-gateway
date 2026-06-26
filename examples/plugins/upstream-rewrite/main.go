package main

import (
	"encoding/json"
	"net"
	"runtime"

	"github.com/tursom/mc-gateway/plugin/api"
)

type PluginImpl struct {
	api.AbstractPlugin
	config Config
}

type Config struct {
	MatchHost string `json:"match_host"`
	Upstream  string `json:"upstream"`
}

func Plugin() api.Plugin {
	return &PluginImpl{}
}

func MCGatewayPluginMetadata() string {
	return manifestJSON
}

func (p *PluginImpl) NewConfigObj() any {
	return &Config{}
}

func (p *PluginImpl) ReloadConfig(config any) error {
	if cfg, ok := config.(*Config); ok {
		p.config = *cfg
	}
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool {
			return p.config.MatchHost == "" || req.Host == p.config.MatchHost || req.Upstream == p.config.MatchHost
		},
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			if p.config.Upstream == "" {
				return nil, api.ErrPass
			}
			if p.config.MatchHost != "" && req.Host != p.config.MatchHost && req.Upstream != p.config.MatchHost {
				return nil, api.ErrPass
			}
			return net.Dial("tcp", p.config.Upstream)
		},
	)
}

var manifestJSON = compactJSON(map[string]any{
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
	"api_version": "plugin-api/v1",
	"sdk_module":  "github.com/tursom/mc-gateway/plugin/api",
	"go_version":  runtime.Version(),
	"go_os":       runtime.GOOS,
	"go_arch":     runtime.GOARCH,
	"extension_points": []map[string]any{
		{"type": "hook", "key": "upstream.connect/v1"},
	},
	"capabilities": map[string]any{
		"extension_points": []string{"upstream.connect/v1"},
		"network":          map[string]any{"outbound": []string{"tcp:*:*"}},
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
	},
})

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
