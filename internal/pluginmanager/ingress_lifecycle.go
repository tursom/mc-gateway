package pluginmanager

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

type IngressListener struct {
	PluginID        string         `json:"plugin_id"`
	ArtifactID      string         `json:"artifact_id"`
	Network         string         `json:"network"`
	Bind            string         `json:"bind"`
	Port            int            `json:"port"`
	State           string         `json:"state"`
	Health          string         `json:"health"`
	TLS             map[string]any `json:"tls,omitempty"`
	SecretRefs      []string       `json:"secret_refs,omitempty"`
	StartedAt       int64          `json:"started_at"`
	DisabledAt      int64          `json:"disabled_at,omitempty"`
	DrainingAt      int64          `json:"draining_at,omitempty"`
	ActiveConns     int64          `json:"active_connections"`
	LastError       string         `json:"last_error,omitempty"`
	RequiresRestart bool           `json:"requires_restart"`
	listener        net.Listener
}

func (m *IngressLifecycleManager) RefreshReservedListeners(listeners []IngressReservedListener) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved = append([]IngressReservedListener(nil), listeners...)
	sort.Slice(m.reserved, func(i, j int) bool { return m.reserved[i].Name < m.reserved[j].Name })
}

func (m *IngressLifecycleManager) Start(ctx context.Context, pluginID, artifactID string, ingress *IngressCapability) (IngressListener, error) {
	if m == nil {
		return IngressListener{}, errors.New("ingress lifecycle manager is nil")
	}
	if ingress == nil {
		return IngressListener{}, errors.New("ingress capability is required")
	}
	if err := ValidateIngressCapability(Manifest{Secrets: ingressSecretSpecs(ingress)}, ingress); err != nil && len(err) > 0 {
		return IngressListener{}, fmt.Errorf("ingress capability is invalid: %s", strings.Join(err, "; "))
	}
	m.mu.Lock()
	for _, reserved := range m.reserved {
		if ingressReservedListenerConflict(ingress, reserved) {
			m.mu.Unlock()
			return IngressListener{}, fmt.Errorf("ingress listener conflicts with reserved gateway listener %q on %s/%s:%d", reserved.Name, normalizedReservedListenerNetwork(reserved.Network), normalizedReservedListenerBind(reserved.Bind), reserved.Port)
		}
	}
	m.mu.Unlock()
	network := ingressListenerNetwork(ingress.Protocol)
	addr := net.JoinHostPort(strings.TrimSpace(ingress.Bind), fmt.Sprint(ingress.Port))
	listener, err := net.Listen(network, addr)
	if err != nil {
		return IngressListener{}, err
	}
	if ingress.TLS != nil && ingress.TLS.Enabled {
		listener = tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12})
	}
	actualPort := ingress.Port
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		actualPort = addr.Port
	}
	state := &IngressListener{
		PluginID:   pluginID,
		ArtifactID: artifactID,
		Network:    network,
		Bind:       strings.TrimSpace(ingress.Bind),
		Port:       actualPort,
		State:      RuntimeEnabled,
		Health:     "healthy",
		TLS:        redactedIngressTLS(ingress),
		SecretRefs: ingressSecretRefs(ingress),
		StartedAt:  time.Now().Unix(),
		listener:   listener,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := ingressKey(pluginID)
	if old := m.listeners[key]; old != nil && old.listener != nil {
		_ = old.listener.Close()
	}
	m.listeners[key] = state
	_ = ctx
	return *state, nil
}

func (m *IngressLifecycleManager) Health(pluginID string) IngressListener {
	if m == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeNotLoaded, Health: "missing"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if listener := m.listeners[ingressKey(pluginID)]; listener != nil {
		return ingressListenerSnapshot(listener)
	}
	return IngressListener{PluginID: pluginID, State: RuntimeNotLoaded, Health: "missing"}
}

func (m *IngressLifecycleManager) Disable(pluginID string) IngressListener {
	if m == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDisabled}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	listener := m.listeners[ingressKey(pluginID)]
	if listener == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDisabled}
	}
	now := time.Now().Unix()
	listener.State = RuntimeDisabled
	listener.Health = "disabled"
	listener.DisabledAt = now
	listener.DrainingAt = now
	if listener.listener != nil {
		_ = listener.listener.Close()
		listener.listener = nil
	}
	return ingressListenerSnapshot(listener)
}

func (m *IngressLifecycleManager) Drain(pluginID string) IngressListener {
	if m == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDraining}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	listener := m.listeners[ingressKey(pluginID)]
	if listener == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDraining}
	}
	listener.State = RuntimeDraining
	listener.Health = "draining"
	listener.DrainingAt = time.Now().Unix()
	if listener.listener != nil {
		_ = listener.listener.Close()
		listener.listener = nil
	}
	return ingressListenerSnapshot(listener)
}

func (m *IngressLifecycleManager) ReservedListeners() []IngressReservedListener {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]IngressReservedListener(nil), m.reserved...)
}

func (m *Manager) RefreshIngressReservedListeners(listeners []IngressReservedListener) {
	m.ingressReservedListeners = append([]IngressReservedListener(nil), listeners...)
	if m.ingressLifecycle == nil {
		m.ingressLifecycle = NewIngressLifecycleManager(m.repo, listeners)
		return
	}
	m.ingressLifecycle.RefreshReservedListeners(listeners)
}

func (m *Manager) StartIngressListener(ctx context.Context, pluginID, artifactID string) (IngressListener, error) {
	if !m.futureGates.IngressEnabled() {
		return IngressListener{}, errors.New("ingress.service/v1 data plane is disabled by feature gate")
	}
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return IngressListener{}, err
	}
	ingress := ingressCapabilityFromArtifact(artifact, manifest)
	if ingress == nil {
		return IngressListener{}, errors.New("ingress capability is required")
	}
	if m.ingressLifecycle == nil {
		m.ingressLifecycle = NewIngressLifecycleManager(m.repo, m.ingressReservedListeners)
	}
	return m.ingressLifecycle.Start(ctx, pluginID, artifactID, ingress)
}

func (m *Manager) DisableIngressListener(pluginID string) IngressListener {
	if m.ingressLifecycle == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDisabled}
	}
	return m.ingressLifecycle.Disable(pluginID)
}

func (m *Manager) DrainIngressListener(pluginID string) IngressListener {
	if m.ingressLifecycle == nil {
		return IngressListener{PluginID: pluginID, State: RuntimeDraining}
	}
	return m.ingressLifecycle.Drain(pluginID)
}

func ingressListenerSnapshot(listener *IngressListener) IngressListener {
	out := *listener
	out.listener = nil
	out.SecretRefs = append([]string(nil), listener.SecretRefs...)
	if listener.TLS != nil {
		out.TLS = make(map[string]any, len(listener.TLS))
		for key, value := range listener.TLS {
			out.TLS[key] = value
		}
	}
	return out
}

func ingressKey(pluginID string) string {
	return strings.TrimSpace(pluginID)
}

func ingressSecretRefs(ingress *IngressCapability) []string {
	if ingress == nil {
		return nil
	}
	refs := append([]string(nil), ingress.SecretRefs...)
	if ingress.TLS != nil {
		refs = append(refs, ingress.TLS.CertSecret, ingress.TLS.KeySecret)
	}
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func ingressSecretSpecs(ingress *IngressCapability) []SecretSpec {
	refs := ingressSecretRefs(ingress)
	specs := make([]SecretSpec, 0, len(refs))
	for _, ref := range refs {
		specs = append(specs, SecretSpec{Name: ref})
	}
	return specs
}

func redactedIngressTLS(ingress *IngressCapability) map[string]any {
	if ingress == nil || ingress.TLS == nil {
		return nil
	}
	return map[string]any{
		"enabled":     ingress.TLS.Enabled,
		"cert_secret": redactIngressSecretRef(ingress.TLS.CertSecret),
		"key_secret":  redactIngressSecretRef(ingress.TLS.KeySecret),
	}
}

func redactIngressSecretRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	return "plugin-secret://" + ref + "#redacted"
}
