// internal/pluginmanager/extensions.go 索引扩展点注册信息，并分发连接、路由、状态和提供方钩子。

package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type dispatchState struct {
	upstreams   []*upstreamHandler
	routes      []*routeHandler
	rules       []*ruleHandler
	statuses    []*statusHandler
	middleware  []*middlewareHandler
	subscribers []*subscriberHandler
	providers   []ProviderSummary
}

type routeHandler struct {
	pluginID       string
	artifactID     string
	priority       int
	handlerID      string
	extensionPoint string
	timeout        time.Duration
	accept         api.RouteResolveAcceptor
	handle         api.RouteResolveHandler
	calls          atomic.Uint64
	errors         atomic.Uint64
	panics         atomic.Uint64
	timeouts       atomic.Uint64
	blocked        atomic.Uint64
	durationCount  atomic.Uint64
	durationSumMS  atomic.Uint64
	durationMaxMS  atomic.Uint64
}

type ruleHandler struct {
	pluginID       string
	artifactID     string
	priority       int
	handlerID      string
	extensionPoint string
	timeout        time.Duration
	accept         api.RuleEvaluateAcceptor
	handle         api.RuleEvaluateHandler
	calls          atomic.Uint64
	errors         atomic.Uint64
	panics         atomic.Uint64
	timeouts       atomic.Uint64
	blocked        atomic.Uint64
	durationCount  atomic.Uint64
	durationSumMS  atomic.Uint64
	durationMaxMS  atomic.Uint64
}

type statusHandler struct {
	pluginID       string
	artifactID     string
	priority       int
	handlerID      string
	extensionPoint string
	timeout        time.Duration
	accept         api.StatusPingAcceptor
	handle         api.StatusPingHandler
	calls          atomic.Uint64
	errors         atomic.Uint64
	panics         atomic.Uint64
	timeouts       atomic.Uint64
	blocked        atomic.Uint64
	durationCount  atomic.Uint64
	durationSumMS  atomic.Uint64
	durationMaxMS  atomic.Uint64
}

type middlewareHandler struct {
	pluginID         string
	artifactID       string
	priority         int
	handlerID        string
	extensionPoint   string
	timeout          time.Duration
	failPolicy       string
	connectionAccept api.ConnectionFilterAcceptor
	connectionHandle api.ConnectionFilterHandler
	handshakeAccept  api.HandshakeFilterAcceptor
	handshakeHandle  api.HandshakeFilterHandler
	calls            atomic.Uint64
	errors           atomic.Uint64
	panics           atomic.Uint64
	timeouts         atomic.Uint64
	blocked          atomic.Uint64
	durationCount    atomic.Uint64
	durationSumMS    atomic.Uint64
	durationMaxMS    atomic.Uint64
}

type subscriberHandler struct {
	pluginID       string
	artifactID     string
	priority       int
	handlerID      string
	extensionPoint string
	timeout        time.Duration
	mode           string
	maxRetry       int
	accept         api.EventSubscriberAcceptor
	handle         api.EventSubscriberHandler
	calls          atomic.Uint64
	errors         atomic.Uint64
	panics         atomic.Uint64
	timeouts       atomic.Uint64
	blocked        atomic.Uint64
	durationCount  atomic.Uint64
	durationSumMS  atomic.Uint64
	durationMaxMS  atomic.Uint64
}

type routeCacheEntry struct {
	summary  RouteDecisionSummary
	decision api.RouteDecision
}

type RouteResolveResult struct {
	Decision api.RouteDecision    `json:"decision"`
	Source   string               `json:"source"`
	Summary  RouteDecisionSummary `json:"summary"`
}

type StatusPingResult struct {
	Handled  bool                   `json:"handled"`
	Response api.StatusPingResponse `json:"response"`
	PluginID string                 `json:"plugin_id,omitempty"`
	Source   string                 `json:"source,omitempty"`
}

type RuleEvaluateResult struct {
	Handled  bool                     `json:"handled"`
	Decision api.RuleEvaluateDecision `json:"decision"`
	PluginID string                   `json:"plugin_id,omitempty"`
	Source   string                   `json:"source,omitempty"`
}

type ConnectionFilterResult struct {
	Allowed  bool   `json:"allowed"`
	PluginID string `json:"plugin_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type HandshakeFilterResult struct {
	Allowed     bool   `json:"allowed"`
	PluginID    string `json:"plugin_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RewriteHost string `json:"rewrite_host,omitempty"`
}

func (e pluginExtensions) empty() bool {
	return e.count() == 0
}

func (e pluginExtensions) count() int {
	return len(e.routes) + len(e.rules) + len(e.statuses) + len(e.middleware) + len(e.subscribers) + len(e.providers)
}

func (loaded *loadedPlugin) dispatchSummaries() []DispatchHandlerSummary {
	out := handlerSummaries(loaded.handlers)
	out = append(out, routeHandlerSummaries(loaded.extensions.routes)...)
	out = append(out, ruleHandlerSummaries(loaded.extensions.rules)...)
	out = append(out, statusHandlerSummaries(loaded.extensions.statuses)...)
	out = append(out, middlewareHandlerSummaries(loaded.extensions.middleware)...)
	out = append(out, subscriberHandlerSummaries(loaded.extensions.subscribers)...)
	return out
}

func (m *Manager) EvaluateRule(ctx context.Context, req api.RuleEvaluateRequest) (RuleEvaluateResult, error) {
	if req.Context == nil {
		req.Context = ctx
	}
	for _, handler := range m.extensionState().rules {
		accepted, err := handler.accepts(req)
		if err != nil {
			return RuleEvaluateResult{}, err
		}
		if !accepted {
			continue
		}
		decision, err := handler.invoke(req)
		if err != nil {
			if errors.Is(err, api.ErrPass) {
				continue
			}
			return RuleEvaluateResult{}, err
		}
		decision = normalizeRuleDecision(decision, handler.pluginID)
		if !decision.Allow && !decision.Deny && !decision.Reject {
			continue
		}
		if decision.Deny || decision.Reject || !decision.Allow {
			handler.blocked.Add(1)
		}
		return RuleEvaluateResult{Handled: true, Decision: decision, PluginID: handler.pluginID, Source: "plugin"}, nil
	}
	return RuleEvaluateResult{Decision: api.RuleEvaluateDecision{Allow: true, Reason: "no rule evaluator handled request"}}, nil
}

func (m *Manager) ResolveRoute(ctx context.Context, req api.RouteResolveRequest, fallback func(api.RouteResolveRequest) (string, bool)) (RouteResolveResult, error) {
	if req.Context == nil {
		req.Context = ctx
	}
	if fallback != nil && req.FallbackUpstream == "" {
		upstream, ok := fallback(req)
		req.FallbackUpstream = upstream
		req.FallbackHit = ok
	}
	state := m.extensionState()
	for _, handler := range state.routes {
		accepted, err := handler.accepts(req)
		if err != nil {
			return RouteResolveResult{}, err
		}
		if !accepted {
			continue
		}
		decision, err := handler.invoke(req)
		if err != nil {
			if errors.Is(err, api.ErrPass) {
				continue
			}
			if cached, ok := m.cachedRoute(req.Host); ok {
				cached.Source = "cache"
				return RouteResolveResult{Decision: cachedDecision(cached), Source: "cache", Summary: cached}, nil
			}
			break
		}
		decision = normalizeRouteDecision(decision, handler.pluginID)
		if decision.Action == api.RouteDecisionPass || decision.Action == "" {
			continue
		}
		summary := routeDecisionSummary(req.Host, decision, "provider")
		m.cacheRoute(req.Host, decision, summary)
		return RouteResolveResult{Decision: decision, Source: "provider", Summary: summary}, nil
	}
	if cached, ok := m.cachedRoute(req.Host); ok && !req.Refresh {
		cached.Source = "cache"
		return RouteResolveResult{Decision: cachedDecision(cached), Source: "cache", Summary: cached}, nil
	}
	upstream := req.FallbackUpstream
	source := "sqlite_fallback"
	if upstream == "" {
		source = "fallback_miss"
	}
	decision := api.RouteDecision{
		Action:     api.RouteDecisionFallback,
		Upstream:   upstream,
		ProviderID: "sqlite",
		Reason:     "sqlite route snapshot fallback",
	}
	if upstream == "" {
		decision.Action = api.RouteDecisionReject
		decision.Reason = "no route provider decision and no sqlite fallback"
	}
	summary := routeDecisionSummary(req.Host, decision, source)
	return RouteResolveResult{Decision: decision, Source: source, Summary: summary}, nil
}

func (m *Manager) RefreshRouteProviders(ctx context.Context, actor string) []RouteDecisionSummary {
	hosts := make([]string, 0)
	m.routeCacheMu.Lock()
	for host := range m.routeCache {
		hosts = append(hosts, host)
	}
	m.routeCache = make(map[string]routeCacheEntry)
	m.routeCacheMu.Unlock()
	sort.Strings(hosts)
	_ = m.repo.RecordOperation(ctx, "", "", "route_provider_refresh", "succeeded", actor, "route provider cache refreshed", map[string]any{
		"cleared_hosts": hosts,
	})
	return m.RouteCacheSnapshot()
}

func (m *Manager) ReplaySubscriberDeadLetters(ctx context.Context, actor string) uint64 {
	count := m.operations.ReplaySubscriberDeadLetters()
	_ = m.repo.RecordOperation(ctx, "", "", "event_subscriber_replay", "succeeded", actor, "event subscriber dead letter replay requested", map[string]any{
		"dead_letters":           count,
		"remaining_dead_letters": m.operations.SubscriberDeadLetters(),
	})
	return count
}

func (m *Manager) DropSubscriberDeadLetters(ctx context.Context, actor string) uint64 {
	count := m.operations.DropSubscriberDeadLetters()
	_ = m.repo.RecordOperation(ctx, "", "", "event_subscriber_drop", "succeeded", actor, "event subscriber dead letters dropped", map[string]any{
		"dead_letters": count,
	})
	return count
}

func (m *Manager) SubscriberDeadLetters(ctx context.Context) uint64 {
	_ = ctx
	return m.operations.SubscriberDeadLetters()
}

func (m *Manager) EmitPluginEventForConformance(ctx context.Context, pluginID, artifactID string, manifest Manifest, name string, fields map[string]string) error {
	return m.operations.ForPlugin(pluginID, artifactID, manifest).EmitEvent(ctx, name, fields)
}

func (m *Manager) RouteCacheSnapshot() []RouteDecisionSummary {
	m.routeCacheMu.Lock()
	defer m.routeCacheMu.Unlock()
	out := make([]RouteDecisionSummary, 0, len(m.routeCache))
	now := time.Now().Unix()
	for _, entry := range m.routeCache {
		if entry.summary.ExpiresAt > 0 && entry.summary.ExpiresAt <= now {
			continue
		}
		out = append(out, entry.summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func (m *Manager) StatusPing(ctx context.Context, req api.StatusPingRequest) (StatusPingResult, error) {
	if req.Context == nil {
		req.Context = ctx
	}
	for _, handler := range m.extensionState().statuses {
		accepted, err := handler.accepts(req)
		if err != nil {
			return StatusPingResult{}, err
		}
		if !accepted {
			continue
		}
		resp, err := handler.invoke(req)
		if err != nil {
			if errors.Is(err, api.ErrPass) {
				continue
			}
			return StatusPingResult{}, err
		}
		return StatusPingResult{Handled: true, Response: resp, PluginID: handler.pluginID, Source: "plugin"}, nil
	}
	return StatusPingResult{}, nil
}

func (m *Manager) FilterConnection(ctx context.Context, req api.ConnectionFilterRequest) (ConnectionFilterResult, error) {
	if req.Context == nil {
		req.Context = ctx
	}
	for _, handler := range m.extensionState().middleware {
		if handler.connectionHandle == nil {
			continue
		}
		accepted, err := handler.acceptsConnection(req)
		if err != nil {
			if handler.failPolicy == api.FailPolicyClose {
				return ConnectionFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: err.Error()}, err
			}
			continue
		}
		if !accepted {
			continue
		}
		decision, err := handler.invokeConnection(req)
		if err != nil {
			if handler.failPolicy == api.FailPolicyClose {
				return ConnectionFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: err.Error()}, err
			}
			continue
		}
		if decision.Reject || !decision.Allow {
			handler.blocked.Add(1)
			return ConnectionFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: decision.Reason}, nil
		}
	}
	return ConnectionFilterResult{Allowed: true}, nil
}

func (m *Manager) FilterHandshake(ctx context.Context, req api.HandshakeFilterRequest) (HandshakeFilterResult, error) {
	if req.Context == nil {
		req.Context = ctx
	}
	result := HandshakeFilterResult{Allowed: true}
	for _, handler := range m.extensionState().middleware {
		if handler.handshakeHandle == nil {
			continue
		}
		accepted, err := handler.acceptsHandshake(req)
		if err != nil {
			if handler.failPolicy == api.FailPolicyClose {
				return HandshakeFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: err.Error()}, err
			}
			continue
		}
		if !accepted {
			continue
		}
		decision, err := handler.invokeHandshake(req)
		if err != nil {
			if handler.failPolicy == api.FailPolicyClose {
				return HandshakeFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: err.Error()}, err
			}
			continue
		}
		if decision.Reject || !decision.Allow {
			handler.blocked.Add(1)
			return HandshakeFilterResult{Allowed: false, PluginID: handler.pluginID, Reason: decision.Reason}, nil
		}
		if decision.RewriteHost != "" {
			result.RewriteHost = decision.RewriteHost
			result.PluginID = handler.pluginID
			req.ServerHost = decision.RewriteHost
		}
	}
	return result, nil
}

func (m *Manager) extensionState() dispatchState {
	value := m.extensionSnapshot.Load()
	if state, ok := value.(dispatchState); ok {
		return state
	}
	return dispatchState{}
}

func (m *Manager) currentExtensionsLocked() map[string]pluginExtensions {
	current := make(map[string]pluginExtensions)
	state := m.extensionState()
	for _, handler := range state.routes {
		ext := current[handler.pluginID]
		ext.routes = append(ext.routes, handler)
		current[handler.pluginID] = ext
	}
	for _, handler := range state.rules {
		ext := current[handler.pluginID]
		ext.rules = append(ext.rules, handler)
		current[handler.pluginID] = ext
	}
	for _, handler := range state.statuses {
		ext := current[handler.pluginID]
		ext.statuses = append(ext.statuses, handler)
		current[handler.pluginID] = ext
	}
	for _, handler := range state.middleware {
		ext := current[handler.pluginID]
		ext.middleware = append(ext.middleware, handler)
		current[handler.pluginID] = ext
	}
	for _, handler := range state.subscribers {
		ext := current[handler.pluginID]
		ext.subscribers = append(ext.subscribers, handler)
		current[handler.pluginID] = ext
	}
	for _, provider := range state.providers {
		ext := current[provider.PluginID]
		ext.providers = append(ext.providers, provider)
		current[provider.PluginID] = ext
	}
	return current
}

func (m *Manager) publishExtensionsLocked(byPlugin map[string]pluginExtensions) {
	var state dispatchState
	for _, ext := range byPlugin {
		state.routes = append(state.routes, ext.routes...)
		state.rules = append(state.rules, ext.rules...)
		state.statuses = append(state.statuses, ext.statuses...)
		state.middleware = append(state.middleware, ext.middleware...)
		state.subscribers = append(state.subscribers, ext.subscribers...)
		state.providers = append(state.providers, ext.providers...)
	}
	sortRouteHandlers(state.routes)
	sortRuleHandlers(state.rules)
	sortStatusHandlers(state.statuses)
	sortMiddlewareHandlers(state.middleware)
	sortSubscriberHandlers(state.subscribers)
	sort.SliceStable(state.providers, func(i, j int) bool {
		if state.providers[i].Type != state.providers[j].Type {
			return state.providers[i].Type < state.providers[j].Type
		}
		if state.providers[i].Priority != state.providers[j].Priority {
			return state.providers[i].Priority < state.providers[j].Priority
		}
		return state.providers[i].Name < state.providers[j].Name
	})
	m.extensionSnapshot.Store(state)
	m.operations.SetSubscribers(state.subscribers)
}

func (m *Manager) removeExtensionsLocked(pluginID string) {
	current := m.currentExtensionsLocked()
	delete(current, pluginID)
	m.publishExtensionsLocked(current)
	m.routeCacheMu.Lock()
	m.routeCache = make(map[string]routeCacheEntry)
	m.routeCacheMu.Unlock()
}

func buildExtensions(pluginRecord PluginRecord, artifact ArtifactRecord, gateway *Gateway) pluginExtensions {
	timeout := DefaultHandlerTimeout
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil && manifest.RuntimeLimits.HandlerTimeoutMS > 0 {
		timeout = time.Duration(manifest.RuntimeLimits.HandlerTimeoutMS) * time.Millisecond
	}
	failPolicy := api.FailPolicyOpen
	var summary CapabilitySummary
	if err := json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary); err == nil && summary.Middleware.FailPolicy != "" {
		failPolicy = summary.Middleware.FailPolicy
	}
	ext := pluginExtensions{}
	if hook, ok := gateway.RouteResolveHandler(); ok {
		ext.routes = append(ext.routes, &routeHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionRouteResolve, extensionPoint: ExtensionRouteResolve, timeout: timeout,
			accept: hook.Acceptor(), handle: hook.Handler(),
		})
	}
	if hook, ok := gateway.RouteResolverHandler(); ok {
		ext.routes = append(ext.routes, &routeHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionRouteResolver, extensionPoint: ExtensionRouteResolver, timeout: timeout,
			accept: hook.Acceptor(), handle: hook.Handler(),
		})
	}
	if hook, ok := gateway.RuleEvaluateHandler(); ok {
		ext.rules = append(ext.rules, &ruleHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionRuleEvaluate, extensionPoint: ExtensionRuleEvaluate, timeout: timeout,
			accept: hook.Acceptor(), handle: hook.Handler(),
		})
	}
	if hook, ok := gateway.StatusPingHandler(); ok {
		ext.statuses = append(ext.statuses, &statusHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionStatusPing, extensionPoint: ExtensionStatusPing, timeout: timeout,
			accept: hook.Acceptor(), handle: hook.Handler(),
		})
	}
	if hook, ok := gateway.ConnectionFilterHandler(); ok {
		ext.middleware = append(ext.middleware, &middlewareHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionConnectionFilter, extensionPoint: ExtensionConnectionFilter, timeout: timeout, failPolicy: failPolicy,
			connectionAccept: hook.Acceptor(), connectionHandle: hook.Handler(),
		})
	}
	if hook, ok := gateway.HandshakeFilterHandler(); ok {
		ext.middleware = append(ext.middleware, &middlewareHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionHandshakeFilter, extensionPoint: ExtensionHandshakeFilter, timeout: timeout, failPolicy: failPolicy,
			handshakeAccept: hook.Acceptor(), handshakeHandle: hook.Handler(),
		})
	}
	if hook, ok := gateway.EventSubscriberHandler(); ok {
		mode := summary.EventSubscriber.Mode
		if mode == "" {
			mode = api.DeliveryBestEffort
		}
		maxRetry := summary.EventSubscriber.MaxRetry
		if maxRetry <= 0 {
			maxRetry = DefaultSubscriberMaxRetry
		}
		ext.subscribers = append(ext.subscribers, &subscriberHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: ExtensionEventSubscriber, extensionPoint: ExtensionEventSubscriber, timeout: timeout,
			mode: mode, maxRetry: maxRetry, accept: hook.Acceptor(), handle: hook.Handler(),
		})
	}
	ext.providers = append(ext.providers, buildProviderSummaries(pluginRecord, artifact, gateway)...)
	return ext
}

func buildProviderSummaries(pluginRecord PluginRecord, artifact ArtifactRecord, gateway *Gateway) []ProviderSummary {
	var out []ProviderSummary
	providerHook, providerOK := gateway.ProviderHandler()
	authHook, authOK := gateway.AuthProviderHandler()
	adminAuthHook, adminAuthOK := gateway.AdminAuthProviderHandler()
	items := []struct {
		key  string
		hook api.HookHandler[api.ProviderAcceptor, api.ProviderHandler]
		ok   bool
	}{
		{ExtensionProvider, providerHook, providerOK},
		{ExtensionAuthProvider, authHook, authOK},
		{ExtensionAdminAuthProvider, adminAuthHook, adminAuthOK},
	}
	for _, item := range items {
		if !item.ok {
			continue
		}
		reg, err := item.hook.Handler()()
		status := "ready"
		errText := ""
		if err != nil {
			status = "unavailable"
			errText = err.Error()
		}
		if reg.Type == "" {
			reg.Type = item.key
		}
		if reg.Name == "" {
			reg.Name = pluginRecord.ID
		}
		if reg.Priority == 0 {
			reg.Priority = pluginRecord.Priority
		}
		out = append(out, ProviderSummary{
			PluginID: pluginRecord.ID, ArtifactID: artifact.ID, ExtensionPoint: item.key,
			Type: reg.Type, Name: reg.Name, Priority: reg.Priority, Fallback: reg.Fallback,
			Dependencies: append([]string(nil), reg.Dependencies...), Metadata: copyStringMap(reg.Metadata),
			Status: status, Error: errText,
		})
	}
	return out
}

func (h *routeHandler) accepts(req api.RouteResolveRequest) (accepted bool, err error) {
	if h.accept == nil {
		return true, nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			h.panics.Add(1)
			accepted = false
			err = fmt.Errorf("plugin %s route acceptor panic: %v", h.pluginID, rec)
		}
	}()
	return h.accept(req), nil
}

func (h *routeHandler) invoke(req api.RouteResolveRequest) (decision api.RouteDecision, err error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "route resolver", func(ctx context.Context) (api.RouteDecision, error) {
		req.Context = ctx
		return h.handle(req)
	})
}

func (h *ruleHandler) accepts(req api.RuleEvaluateRequest) (accepted bool, err error) {
	if h.accept == nil {
		return true, nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			h.panics.Add(1)
			accepted = false
			err = fmt.Errorf("plugin %s rule acceptor panic: %v", h.pluginID, rec)
		}
	}()
	return h.accept(req), nil
}

func (h *ruleHandler) invoke(req api.RuleEvaluateRequest) (decision api.RuleEvaluateDecision, err error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "rule evaluator", func(ctx context.Context) (api.RuleEvaluateDecision, error) {
		req.Context = ctx
		return h.handle(req)
	})
}

func (h *statusHandler) accepts(req api.StatusPingRequest) (accepted bool, err error) {
	if h.accept == nil {
		return true, nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			h.panics.Add(1)
			accepted = false
			err = fmt.Errorf("plugin %s status acceptor panic: %v", h.pluginID, rec)
		}
	}()
	return h.accept(req), nil
}

func (h *statusHandler) invoke(req api.StatusPingRequest) (response api.StatusPingResponse, err error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "status ping", func(ctx context.Context) (api.StatusPingResponse, error) {
		req.Context = ctx
		return h.handle(req)
	})
}

func (h *middlewareHandler) acceptsConnection(req api.ConnectionFilterRequest) (accepted bool, err error) {
	if h.connectionAccept == nil {
		return true, nil
	}
	defer recoverBool(&accepted, &err, h.pluginID, "connection filter acceptor", &h.panics)
	return h.connectionAccept(req), nil
}

func (h *middlewareHandler) acceptsHandshake(req api.HandshakeFilterRequest) (accepted bool, err error) {
	if h.handshakeAccept == nil {
		return true, nil
	}
	defer recoverBool(&accepted, &err, h.pluginID, "handshake filter acceptor", &h.panics)
	return h.handshakeAccept(req), nil
}

func (h *middlewareHandler) invokeConnection(req api.ConnectionFilterRequest) (api.FilterDecision, error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "connection filter", func(ctx context.Context) (api.FilterDecision, error) {
		req.Context = ctx
		return h.connectionHandle(req)
	})
}

func (h *middlewareHandler) invokeHandshake(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "handshake filter", func(ctx context.Context) (api.HandshakeFilterDecision, error) {
		req.Context = ctx
		return h.handshakeHandle(req)
	})
}

func (h *subscriberHandler) accepts(req api.EventDeliveryRequest) (accepted bool, err error) {
	if h.accept == nil {
		return true, nil
	}
	defer recoverBool(&accepted, &err, h.pluginID, "event subscriber acceptor", &h.panics)
	return h.accept(req), nil
}

func (h *subscriberHandler) invoke(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
	h.calls.Add(1)
	start := time.Now()
	defer recordExtensionDuration(&h.durationCount, &h.durationSumMS, &h.durationMaxMS, start)
	if req.Context == nil {
		req.Context = context.Background()
	}
	return invokeWithTimeout(req.Context, h.timeout, &h.timeouts, &h.panics, &h.errors, h.pluginID, "event subscriber", func(ctx context.Context) (api.EventDeliveryResult, error) {
		req.Context = ctx
		return h.handle(req)
	})
}

func invokeWithTimeout[T any](ctx context.Context, timeout time.Duration, timeouts, panics, errorsCounter *atomic.Uint64, pluginID, name string, fn func(context.Context) (T, error)) (zero T, err error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan struct {
		value T
		err   error
	}, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
				done <- struct {
					value T
					err   error
				}{err: fmt.Errorf("plugin %s %s panic: %v", pluginID, name, rec)}
			}
		}()
		value, err := fn(ctx)
		done <- struct {
			value T
			err   error
		}{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		timeouts.Add(1)
		return zero, ctx.Err()
	case result := <-done:
		if result.err != nil && !errors.Is(result.err, api.ErrPass) {
			errorsCounter.Add(1)
		}
		return result.value, result.err
	}
}

func recordExtensionDuration(count, sum, max *atomic.Uint64, start time.Time) {
	durationMS := uint64(time.Since(start).Milliseconds())
	count.Add(1)
	sum.Add(durationMS)
	for {
		current := max.Load()
		if durationMS <= current || max.CompareAndSwap(current, durationMS) {
			return
		}
	}
}

func recoverBool(accepted *bool, err *error, pluginID, name string, panics *atomic.Uint64) {
	if rec := recover(); rec != nil {
		panics.Add(1)
		*accepted = false
		*err = fmt.Errorf("plugin %s %s panic: %v", pluginID, name, rec)
	}
}

func sortRouteHandlers(handlers []*routeHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		return extensionLess(handlers[i].priority, handlers[i].pluginID, handlers[i].handlerID, handlers[j].priority, handlers[j].pluginID, handlers[j].handlerID)
	})
}

func sortRuleHandlers(handlers []*ruleHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		return extensionLess(handlers[i].priority, handlers[i].pluginID, handlers[i].handlerID, handlers[j].priority, handlers[j].pluginID, handlers[j].handlerID)
	})
}

func sortStatusHandlers(handlers []*statusHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		return extensionLess(handlers[i].priority, handlers[i].pluginID, handlers[i].handlerID, handlers[j].priority, handlers[j].pluginID, handlers[j].handlerID)
	})
}

func sortMiddlewareHandlers(handlers []*middlewareHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		return extensionLess(handlers[i].priority, handlers[i].pluginID, handlers[i].handlerID, handlers[j].priority, handlers[j].pluginID, handlers[j].handlerID)
	})
}

func sortSubscriberHandlers(handlers []*subscriberHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		return extensionLess(handlers[i].priority, handlers[i].pluginID, handlers[i].handlerID, handlers[j].priority, handlers[j].pluginID, handlers[j].handlerID)
	})
}

func extensionLess(aPriority int, aPlugin, aHandler string, bPriority int, bPlugin, bHandler string) bool {
	if aPriority != bPriority {
		return aPriority < bPriority
	}
	if aPlugin != bPlugin {
		return aPlugin < bPlugin
	}
	return aHandler < bHandler
}

func extensionSummary(pluginID, artifactID string, priority int, handlerID, extensionPoint, mode string, timeout time.Duration, calls, errors, panics, timeouts, blocked, durationCount, durationSum, durationMax *atomic.Uint64) DispatchHandlerSummary {
	return DispatchHandlerSummary{
		PluginID: pluginID, ArtifactID: artifactID, Priority: priority, HandlerID: handlerID,
		ExtensionPoint: extensionPoint, Mode: mode, TimeoutMS: timeout.Milliseconds(),
		Calls: calls.Load(), Errors: errors.Load(), Panics: panics.Load(), Timeouts: timeouts.Load(), Blocked: blocked.Load(),
		DurationCount: durationCount.Load(), DurationSumMS: durationSum.Load(), DurationMaxMS: durationMax.Load(),
	}
}

func routeHandlerSummaries(handlers []*routeHandler) []DispatchHandlerSummary {
	out := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, h := range handlers {
		out = append(out, extensionSummary(h.pluginID, h.artifactID, h.priority, h.handlerID, h.extensionPoint, "route", h.timeout, &h.calls, &h.errors, &h.panics, &h.timeouts, &h.blocked, &h.durationCount, &h.durationSumMS, &h.durationMaxMS))
	}
	return out
}

func statusHandlerSummaries(handlers []*statusHandler) []DispatchHandlerSummary {
	out := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, h := range handlers {
		out = append(out, extensionSummary(h.pluginID, h.artifactID, h.priority, h.handlerID, h.extensionPoint, "status", h.timeout, &h.calls, &h.errors, &h.panics, &h.timeouts, &h.blocked, &h.durationCount, &h.durationSumMS, &h.durationMaxMS))
	}
	return out
}

func ruleHandlerSummaries(handlers []*ruleHandler) []DispatchHandlerSummary {
	out := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, h := range handlers {
		out = append(out, extensionSummary(h.pluginID, h.artifactID, h.priority, h.handlerID, h.extensionPoint, "rule", h.timeout, &h.calls, &h.errors, &h.panics, &h.timeouts, &h.blocked, &h.durationCount, &h.durationSumMS, &h.durationMaxMS))
	}
	return out
}

func middlewareHandlerSummaries(handlers []*middlewareHandler) []DispatchHandlerSummary {
	out := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, h := range handlers {
		out = append(out, extensionSummary(h.pluginID, h.artifactID, h.priority, h.handlerID, h.extensionPoint, h.failPolicy, h.timeout, &h.calls, &h.errors, &h.panics, &h.timeouts, &h.blocked, &h.durationCount, &h.durationSumMS, &h.durationMaxMS))
	}
	return out
}

func subscriberHandlerSummaries(handlers []*subscriberHandler) []DispatchHandlerSummary {
	out := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, h := range handlers {
		out = append(out, extensionSummary(h.pluginID, h.artifactID, h.priority, h.handlerID, h.extensionPoint, h.mode, h.timeout, &h.calls, &h.errors, &h.panics, &h.timeouts, &h.blocked, &h.durationCount, &h.durationSumMS, &h.durationMaxMS))
	}
	return out
}

func normalizeRouteDecision(decision api.RouteDecision, providerID string) api.RouteDecision {
	if decision.ProviderID == "" {
		decision.ProviderID = providerID
	}
	switch decision.Action {
	case api.RouteDecisionOverride, api.RouteDecisionFallback, api.RouteDecisionReject, api.RouteDecisionPass:
	default:
		if decision.Upstream != "" {
			decision.Action = api.RouteDecisionOverride
		} else {
			decision.Action = api.RouteDecisionPass
		}
	}
	if decision.CacheTTL == 0 {
		decision.CacheTTL = time.Minute
	}
	return decision
}

func normalizeRuleDecision(decision api.RuleEvaluateDecision, providerID string) api.RuleEvaluateDecision {
	if decision.ProviderID == "" {
		decision.ProviderID = providerID
	}
	if decision.Deny || decision.Reject {
		decision.Allow = false
		return decision
	}
	if !decision.Allow && decision.Reason == "" {
		decision.Allow = true
	}
	return decision
}

func routeDecisionSummary(host string, decision api.RouteDecision, source string) RouteDecisionSummary {
	now := time.Now()
	expiresAt := int64(0)
	if decision.CacheTTL > 0 {
		expiresAt = now.Add(decision.CacheTTL).Unix()
	}
	targetHost := host
	if decision.Host != "" {
		targetHost = decision.Host
	}
	metadata := copyStringMap(decision.Metadata)
	if decision.Explanation != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["explanation"] = decision.Explanation
	}
	return RouteDecisionSummary{
		Host: targetHost, Action: decision.Action, Upstream: decision.Upstream, ProviderID: decision.ProviderID,
		Source: source, Reason: decision.Reason, Metadata: metadata,
		CreatedAt: now.Unix(), ExpiresAt: expiresAt,
	}
}

func (m *Manager) cacheRoute(host string, decision api.RouteDecision, summary RouteDecisionSummary) {
	if host == "" || decision.Action == api.RouteDecisionPass {
		return
	}
	m.routeCacheMu.Lock()
	defer m.routeCacheMu.Unlock()
	m.routeCache[host] = routeCacheEntry{summary: summary, decision: decision}
}

func (m *Manager) cachedRoute(host string) (RouteDecisionSummary, bool) {
	m.routeCacheMu.Lock()
	defer m.routeCacheMu.Unlock()
	entry, ok := m.routeCache[host]
	if !ok {
		return RouteDecisionSummary{}, false
	}
	if entry.summary.ExpiresAt > 0 && entry.summary.ExpiresAt <= time.Now().Unix() {
		delete(m.routeCache, host)
		return RouteDecisionSummary{}, false
	}
	return entry.summary, true
}

func cachedDecision(summary RouteDecisionSummary) api.RouteDecision {
	return api.RouteDecision{
		Action: summary.Action, Upstream: summary.Upstream, Host: summary.Host, Reason: summary.Reason,
		ProviderID: summary.ProviderID, Metadata: copyStringMap(summary.Metadata),
	}
}

func copyStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
