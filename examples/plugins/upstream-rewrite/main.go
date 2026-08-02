// examples/plugins/upstream-rewrite/main.go 演示托管插件如何在网关拨号上游前改写路由决策。

package main

import "github.com/tursom/mc-gateway/plugin/api"

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
		api.HookRouteResolve,
		func(req api.RouteResolveRequest) bool {
			return p.config.MatchHost == "" || req.Host == p.config.MatchHost
		},
		func(req api.RouteResolveRequest) (api.RouteDecision, error) {
			if p.config.Upstream == "" {
				return api.RouteDecision{Action: api.RouteDecisionPass}, nil
			}
			if p.config.MatchHost != "" && req.Host != p.config.MatchHost {
				return api.RouteDecision{Action: api.RouteDecisionPass}, nil
			}
			return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: p.config.Upstream}, nil
		},
	)
}
