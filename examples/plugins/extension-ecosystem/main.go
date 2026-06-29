// examples/plugins/extension-ecosystem/main.go demonstrates the M6 extension ecosystem fixtures.

package main

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type PluginImpl struct {
	api.AbstractPlugin
	mu     sync.Mutex
	config Config
	bucket map[string]rateBucket
}

type Config struct {
	OverrideHost      string `json:"override_host"`
	OverrideUpstream  string `json:"override_upstream"`
	RejectHost        string `json:"reject_host"`
	DenySourceCIDR    string `json:"deny_source_cidr"`
	AllowSourceCIDR   string `json:"allow_source_cidr"`
	RewriteHost       string `json:"rewrite_host"`
	RewriteTarget     string `json:"rewrite_target"`
	UpstreamRewrite   string `json:"upstream_rewrite"`
	RateLimit         int    `json:"rate_limit"`
	RateWindow        string `json:"rate_window"`
	Maintenance       bool   `json:"maintenance"`
	MaintenanceWindow string `json:"maintenance_window"`
	Favicon           string `json:"favicon"`
	VersionText       string `json:"version_text"`
	OnlinePlayers     int    `json:"online_players"`
	MaxPlayers        int    `json:"max_players"`
}

type rateBucket struct {
	windowStart time.Time
	count       int
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
	if p.config.OverrideHost == "" {
		p.config.OverrideHost = "blue.example"
	}
	if p.config.OverrideUpstream == "" {
		p.config.OverrideUpstream = "127.0.0.1:25566"
	}
	if p.config.RejectHost == "" {
		p.config.RejectHost = "blocked.example"
	}
	if p.config.DenySourceCIDR == "" {
		p.config.DenySourceCIDR = "203.0.113.0/24"
	}
	if p.config.AllowSourceCIDR == "" {
		p.config.AllowSourceCIDR = "198.51.100.0/24"
	}
	if p.config.RewriteHost == "" {
		p.config.RewriteHost = "legacy.example"
	}
	if p.config.RewriteTarget == "" {
		p.config.RewriteTarget = p.config.OverrideHost
	}
	if p.config.UpstreamRewrite == "" {
		p.config.UpstreamRewrite = "127.0.0.1:25567"
	}
	if p.config.RateLimit == 0 {
		p.config.RateLimit = 2
	}
	if p.config.RateWindow == "" {
		p.config.RateWindow = "1m"
	}
	if p.config.MaintenanceWindow == "" {
		p.config.MaintenanceWindow = "02:00-03:00 UTC"
	}
	if p.config.Favicon == "" {
		p.config.Favicon = "data:image/png;base64,fixture"
	}
	if p.config.VersionText == "" {
		p.config.VersionText = "mc-gateway"
	}
	if p.config.OnlinePlayers == 0 {
		p.config.OnlinePlayers = 1
	}
	if p.config.MaxPlayers == 0 {
		p.config.MaxPlayers = 20
	}
	p.mu.Lock()
	if p.bucket == nil {
		p.bucket = make(map[string]rateBucket)
	}
	p.mu.Unlock()
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	if err := api.RegisterHookHandler(gateway, api.HookConnectionFilter, p.acceptConnection, p.filterConnection); err != nil {
		return err
	}
	if err := api.RegisterHookHandler(gateway, api.HookHandshakeFilter, p.acceptHandshake, p.filterHandshake); err != nil {
		return err
	}
	if err := api.RegisterHookHandler(gateway, api.HookRouteResolve, p.acceptRoute, p.resolveRoute); err != nil {
		return err
	}
	if err := api.RegisterHookHandler(gateway, api.HookRuleEvaluate, p.acceptRule, p.evaluateRule); err != nil {
		return err
	}
	if err := api.RegisterHookHandler(gateway, api.HookStatusPing, p.acceptStatus, p.statusPing); err != nil {
		return err
	}
	if err := api.RegisterHookHandler(gateway, api.HookEventSubscriber, p.acceptEvent, p.handleEvent); err != nil {
		return err
	}
	return api.RegisterHookHandler(gateway, api.HookAdminAuthProvider, p.acceptProvider, p.adminAuthProvider)
}

func (p *PluginImpl) acceptConnection(req api.ConnectionFilterRequest) bool {
	return req.SourceAddr != ""
}

func (p *PluginImpl) filterConnection(req api.ConnectionFilterRequest) (api.FilterDecision, error) {
	_, network, err := net.ParseCIDR(p.config.DenySourceCIDR)
	host, _, splitErr := net.SplitHostPort(req.SourceAddr)
	if splitErr != nil {
		host = req.SourceAddr
	}
	ip := net.ParseIP(host)
	if err == nil && ip != nil && network.Contains(ip) {
		return api.FilterDecision{Allow: false, Reject: true, Reason: "source denied by extension ecosystem fixture"}, nil
	}
	if p.config.AllowSourceCIDR != "" {
		_, allowNetwork, allowErr := net.ParseCIDR(p.config.AllowSourceCIDR)
		if allowErr == nil && ip != nil && !allowNetwork.Contains(ip) {
			return api.FilterDecision{Allow: false, Reject: true, Reason: "source outside extension ecosystem allow CIDR"}, nil
		}
	}
	if p.config.RateLimit > 0 && p.rateLimited(req.SourceAddr) {
		return api.FilterDecision{Allow: false, Reject: true, Reason: "source rate limited by extension ecosystem fixture"}, nil
	}
	return api.FilterDecision{Allow: true}, nil
}

func (p *PluginImpl) acceptHandshake(req api.HandshakeFilterRequest) bool {
	return req.ServerHost != ""
}

func (p *PluginImpl) filterHandshake(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
	if strings.EqualFold(req.ServerHost, p.config.RewriteHost) {
		return api.HandshakeFilterDecision{
			FilterDecision: api.FilterDecision{Allow: true, Reason: "host rewrite by extension ecosystem fixture"},
			RewriteHost:    p.config.RewriteTarget,
		}, nil
	}
	return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Allow: true}}, nil
}

func (p *PluginImpl) acceptRoute(req api.RouteResolveRequest) bool {
	return req.Host != ""
}

func (p *PluginImpl) resolveRoute(req api.RouteResolveRequest) (api.RouteDecision, error) {
	host := strings.ToLower(req.Host)
	switch {
	case host == strings.ToLower(p.config.RejectHost):
		return api.RouteDecision{Action: api.RouteDecisionReject, Host: req.Host, Reason: "blocked by extension ecosystem fixture"}, nil
	case host == strings.ToLower(p.config.OverrideHost):
		return api.RouteDecision{
			Action:      api.RouteDecisionOverride,
			Host:        req.Host,
			Upstream:    p.config.OverrideUpstream,
			ProviderID:  "extension-ecosystem",
			Reason:      "fixture override",
			CacheTTL:    time.Minute,
			Metadata:    map[string]string{"explain": "external refresh returned override and cached with TTL"},
			Explanation: "external source refresh selected fixture override",
		}, nil
	case host == strings.ToLower(p.config.RewriteHost):
		return api.RouteDecision{
			Action:     api.RouteDecisionOverride,
			Host:       req.Host,
			Upstream:   p.config.UpstreamRewrite,
			ProviderID: "extension-ecosystem",
			Reason:     "upstream rewrite fixture",
			CacheTTL:   time.Minute,
			Metadata:   map[string]string{"explain": "upstream rewrite rule matched"},
		}, nil
	case req.FallbackHit:
		return api.RouteDecision{
			Action:     api.RouteDecisionFallback,
			Host:       req.Host,
			Upstream:   req.FallbackUpstream,
			ProviderID: "sqlite",
			Reason:     "sqlite fallback after provider pass",
			Metadata:   map[string]string{"explain": "external source miss used sqlite snapshot fallback"},
		}, nil
	default:
		return api.RouteDecision{Action: api.RouteDecisionPass}, nil
	}
}

func (p *PluginImpl) acceptRule(req api.RuleEvaluateRequest) bool {
	return req.SourceAddr != "" || req.Host != ""
}

func (p *PluginImpl) evaluateRule(req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
	filter, err := p.filterConnection(api.ConnectionFilterRequest{SourceAddr: req.SourceAddr})
	if err != nil {
		return api.RuleEvaluateDecision{}, err
	}
	if filter.Reject || !filter.Allow {
		return api.RuleEvaluateDecision{Deny: true, Reason: filter.Reason, ProviderID: "extension-ecosystem"}, nil
	}
	return api.RuleEvaluateDecision{Allow: true, Reason: "extension ecosystem rule allowed", ProviderID: "extension-ecosystem"}, nil
}

func (p *PluginImpl) acceptStatus(req api.StatusPingRequest) bool {
	return req.Host == "blue.example" || req.Host == "red.example"
}

func (p *PluginImpl) statusPing(req api.StatusPingRequest) (api.StatusPingResponse, error) {
	motd := "Extension ecosystem"
	if p.config.Maintenance {
		motd = "Maintenance"
	}
	return api.StatusPingResponse{
		MOTD:              motd + " " + req.Host,
		Favicon:           p.config.Favicon,
		OnlinePlayers:     p.config.OnlinePlayers,
		MaxPlayers:        p.config.MaxPlayers,
		VersionText:       p.config.VersionText,
		ProtocolVersion:   req.ProtocolVersion,
		Maintenance:       p.config.Maintenance,
		MaintenanceWindow: p.config.MaintenanceWindow,
	}, nil
}

func (p *PluginImpl) acceptEvent(req api.EventDeliveryRequest) bool {
	return req.Name != ""
}

func (p *PluginImpl) handleEvent(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
	if req.Name == "fixture.retry" && req.Attempt <= 1 {
		return api.EventDeliveryResult{OK: false, Retry: true, Reason: "fixture retry"}, nil
	}
	return api.EventDeliveryResult{OK: true}, nil
}

func (p *PluginImpl) acceptProvider(reg api.ProviderRegistration) bool {
	return reg.Type == "" || reg.Type == api.HookAdminAuthProvider.Key()
}

func (p *PluginImpl) adminAuthProvider() (api.ProviderRegistration, error) {
	return api.ProviderRegistration{
		Type:         api.HookAdminAuthProvider.Key(),
		Name:         "external-identity",
		Priority:     100,
		Fallback:     true,
		Dependencies: []string{"local-admin-break-glass"},
		Metadata: map[string]string{
			"break_glass": "local-admin",
		},
	}, nil
}

func (p *PluginImpl) rateLimited(key string) bool {
	window := time.Minute
	if parsed, err := time.ParseDuration(p.config.RateWindow); err == nil && parsed > 0 {
		window = parsed
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	bucket := p.bucket[key]
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= window {
		bucket = rateBucket{windowStart: now}
	}
	bucket.count++
	p.bucket[key] = bucket
	return bucket.count > p.config.RateLimit
}
