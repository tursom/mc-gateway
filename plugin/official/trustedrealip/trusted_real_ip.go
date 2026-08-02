package trustedrealip

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/tursom/mc-gateway/plugin/api"
)

type Config struct {
	Header       string   `json:"header"`
	TrustedPeers []string `json:"trusted_peers"`
}

type Plugin struct {
	api.AbstractPlugin
	mu      sync.RWMutex
	header  string
	trusted []*net.IPNet
}

func New() api.Plugin { return &Plugin{} }

func (p *Plugin) NewConfigObj() any {
	return &Config{Header: "X-Real-IP", TrustedPeers: []string{"127.0.0.1/32"}}
}

func (p *Plugin) ReloadConfig(value any) error {
	config, ok := value.(*Config)
	if !ok || config == nil {
		return errors.New("trusted real IP config has an invalid type")
	}
	header := strings.TrimSpace(config.Header)
	if header == "" {
		return errors.New("header is required")
	}
	if len(config.TrustedPeers) == 0 {
		return errors.New("trusted_peers must not be empty")
	}
	trusted := make([]*net.IPNet, 0, len(config.TrustedPeers))
	for _, raw := range config.TrustedPeers {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return errors.New("trusted_peers contains an invalid CIDR")
		}
		trusted = append(trusted, network)
	}
	p.mu.Lock()
	p.header = http.CanonicalHeaderKey(header)
	p.trusted = trusted
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Init(gateway api.Gateway) error {
	return api.RegisterUpstreamConnectHandlerV2(gateway, p.handle)
}

func (p *Plugin) handle(req api.UpstreamConnectRequestV2) error {
	if req.Ingress.Transport != "websocket" || req.Ingress.HTTP == nil {
		return req.Flow.Next(req.Connection)
	}
	peer := parsePeerIP(req.PeerAddr)
	if peer == nil {
		_ = req.Connection.Stream.Close()
		return nil
	}
	p.mu.RLock()
	header := p.header
	trustedPeer := false
	for _, network := range p.trusted {
		if network.Contains(peer) {
			trustedPeer = true
			break
		}
	}
	p.mu.RUnlock()
	if !trustedPeer {
		_ = req.Connection.Stream.Close()
		return nil
	}
	values := req.Ingress.HTTP.Headers.Values(header)
	if len(values) != 1 || strings.Contains(values[0], ",") {
		_ = req.Connection.Stream.Close()
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(values[0]))
	if ip == nil {
		_ = req.Connection.Stream.Close()
		return nil
	}
	state := req.Connection
	state.EffectiveSourceAddr = net.JoinHostPort(ip.String(), "0")
	return req.Flow.Next(state)
}

func parsePeerIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.TrimSpace(addr))
}
