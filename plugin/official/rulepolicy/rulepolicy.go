// plugin/official/rulepolicy/rulepolicy.go 实现内置 rule-policy 插件，用于 IP 允许/拒绝、维护响应和限流策略。

package rulepolicy

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type Plugin struct {
	api.AbstractPlugin

	mu     sync.Mutex
	config Config
	bucket map[string]rateBucket
}

type Config struct {
	HostRewrite     map[string]string `json:"host_rewrite,omitempty"`
	UpstreamRewrite map[string]string `json:"upstream_rewrite,omitempty"`
	SourceAllowCIDR []string          `json:"source_allow_cidr,omitempty"`
	SourceDenyCIDR  []string          `json:"source_deny_cidr,omitempty"`
	RateLimit       RateLimitConfig   `json:"rate_limit,omitempty"`
	Maintenance     MaintenanceConfig `json:"maintenance,omitempty"`
}

type RateLimitConfig struct {
	Requests int    `json:"requests,omitempty"`
	Window   string `json:"window,omitempty"`
}

type MaintenanceConfig struct {
	Enabled       bool              `json:"enabled,omitempty"`
	Hosts         []string          `json:"hosts,omitempty"`
	MOTD          string            `json:"motd,omitempty"`
	Favicon       string            `json:"favicon,omitempty"`
	OnlinePlayers int               `json:"online_players,omitempty"`
	MaxPlayers    int               `json:"max_players,omitempty"`
	Version       string            `json:"version,omitempty"`
	Window        string            `json:"window,omitempty"`
	StatusByHost  map[string]string `json:"status_by_host,omitempty"`
}

type rateBucket struct {
	windowStart time.Time
	count       int
}

func New() *Plugin {
	return &Plugin{bucket: make(map[string]rateBucket)}
}

func (p *Plugin) NewConfigObj() any {
	return &Config{}
}

func (p *Plugin) ReloadConfig(config any) error {
	next, _ := config.(*Config)
	if next == nil {
		next = &Config{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = *next
	if p.bucket == nil {
		p.bucket = make(map[string]rateBucket)
	}
	return nil
}

func (p *Plugin) Init(gateway api.Gateway) error {
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
	return api.RegisterHookHandler(gateway, api.HookStatusPing, p.acceptStatus, p.statusPing)
}

func (p *Plugin) acceptConnection(api.ConnectionFilterRequest) bool { return true }

func (p *Plugin) filterConnection(req api.ConnectionFilterRequest) (api.FilterDecision, error) {
	cfg := p.snapshot()
	ip := sourceIP(req.SourceAddr)
	if ip != nil {
		if cidrMatches(cfg.SourceDenyCIDR, ip) {
			return api.FilterDecision{Allow: false, Reject: true, Reason: "source denied by CIDR policy"}, nil
		}
		if len(cfg.SourceAllowCIDR) > 0 && !cidrMatches(cfg.SourceAllowCIDR, ip) {
			return api.FilterDecision{Allow: false, Reject: true, Reason: "source not allowed by CIDR policy"}, nil
		}
	}
	if cfg.RateLimit.Requests > 0 && p.rateLimited(req.SourceAddr, cfg.RateLimit) {
		return api.FilterDecision{Allow: false, Reject: true, Reason: "source rate limited"}, nil
	}
	return api.FilterDecision{Allow: true}, nil
}

func (p *Plugin) acceptHandshake(api.HandshakeFilterRequest) bool { return true }

func (p *Plugin) filterHandshake(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
	cfg := p.snapshot()
	if rewritten := cfg.HostRewrite[strings.ToLower(req.ServerHost)]; rewritten != "" {
		return api.HandshakeFilterDecision{
			FilterDecision: api.FilterDecision{Allow: true, Reason: "host rewrite"},
			RewriteHost:    rewritten,
		}, nil
	}
	return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Allow: true}}, nil
}

func (p *Plugin) acceptRoute(api.RouteResolveRequest) bool { return true }

func (p *Plugin) resolveRoute(req api.RouteResolveRequest) (api.RouteDecision, error) {
	cfg := p.snapshot()
	if upstream := cfg.UpstreamRewrite[strings.ToLower(req.Host)]; upstream != "" {
		return api.RouteDecision{
			Action:     api.RouteDecisionOverride,
			Upstream:   upstream,
			ProviderID: "official.rule-policy",
			Reason:     "upstream rewrite rule",
			CacheTTL:   time.Minute,
		}, nil
	}
	return api.RouteDecision{Action: api.RouteDecisionPass}, nil
}

func (p *Plugin) acceptRule(api.RuleEvaluateRequest) bool { return true }

func (p *Plugin) evaluateRule(req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
	cfg := p.snapshot()
	ip := sourceIP(req.SourceAddr)
	if ip != nil {
		if cidrMatches(cfg.SourceDenyCIDR, ip) {
			return api.RuleEvaluateDecision{Deny: true, Reason: "source denied by CIDR policy", ProviderID: "official.rule-policy"}, nil
		}
		if len(cfg.SourceAllowCIDR) > 0 && !cidrMatches(cfg.SourceAllowCIDR, ip) {
			return api.RuleEvaluateDecision{Deny: true, Reason: "source not allowed by CIDR policy", ProviderID: "official.rule-policy"}, nil
		}
	}
	if cfg.RateLimit.Requests > 0 && p.rateLimited(req.SourceAddr, cfg.RateLimit) {
		return api.RuleEvaluateDecision{Deny: true, Reason: "source rate limited", ProviderID: "official.rule-policy"}, nil
	}
	return api.RuleEvaluateDecision{Allow: true, Reason: "rule policy allowed", ProviderID: "official.rule-policy"}, nil
}

func (p *Plugin) acceptStatus(req api.StatusPingRequest) bool {
	cfg := p.snapshot()
	if !cfg.Maintenance.Enabled && len(cfg.Maintenance.StatusByHost) == 0 {
		return false
	}
	if len(cfg.Maintenance.Hosts) == 0 {
		return true
	}
	host := strings.ToLower(req.Host)
	for _, item := range cfg.Maintenance.Hosts {
		if strings.ToLower(item) == host {
			return true
		}
	}
	return false
}

func (p *Plugin) statusPing(req api.StatusPingRequest) (api.StatusPingResponse, error) {
	cfg := p.snapshot()
	motd := cfg.Maintenance.MOTD
	if hostMOTD := cfg.Maintenance.StatusByHost[strings.ToLower(req.Host)]; hostMOTD != "" {
		motd = hostMOTD
	}
	if motd == "" {
		motd = "Maintenance"
	}
	version := cfg.Maintenance.Version
	if version == "" {
		version = "Maintenance"
	}
	return api.StatusPingResponse{
		MOTD:              motd,
		Favicon:           cfg.Maintenance.Favicon,
		OnlinePlayers:     cfg.Maintenance.OnlinePlayers,
		MaxPlayers:        cfg.Maintenance.MaxPlayers,
		VersionText:       version,
		ProtocolVersion:   req.ProtocolVersion,
		Maintenance:       cfg.Maintenance.Enabled,
		MaintenanceWindow: cfg.Maintenance.Window,
	}, nil
}

func (p *Plugin) snapshot() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config
}

func (p *Plugin) rateLimited(key string, cfg RateLimitConfig) bool {
	window := time.Second
	if cfg.Window != "" {
		if parsed, err := time.ParseDuration(cfg.Window); err == nil && parsed > 0 {
			window = parsed
		}
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
	return bucket.count > cfg.Requests
}

func cidrMatches(ranges []string, ip net.IP) bool {
	for _, raw := range ranges {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func sourceIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(host)
}
