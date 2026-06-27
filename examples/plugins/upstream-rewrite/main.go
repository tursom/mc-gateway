// examples/plugins/upstream-rewrite/main.go 演示托管插件如何在网关拨号上游前改写路由决策。

package main

import (
	"net"

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
