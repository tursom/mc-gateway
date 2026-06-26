package api

import (
	"context"
	"errors"
	"net"
	"time"
	"unsafe"
)

var (
	UnsupportedHookType = errors.New("unsupported hook type")
	ErrPass             = errors.New("plugin handler pass")
	ErrBlocked          = errors.New("plugin handler blocked")
)

type (
	ConnectionIDContextKey struct{}
	TraceIDContextKey      struct{}

	HookType[Accept, Handler any] struct {
		key string
	}

	HookHandler[Accept, Handler any] struct {
		acceptor Accept
		handler  Handler
	}

	UpstreamConnectRequest struct {
		Context          context.Context
		Source           net.Conn
		Host             string
		Upstream         string
		InitialData      []byte
		Metadata         map[string]string
		ConnectionID     string
		TraceID          string
		SourceAddr       string
		ServerHost       string
		RawServerHost    string
		ProtocolVersion  int
		NextState        int
		RouteID          string
		RouteTags        []string
		UpstreamRaw      string
		UpstreamProtocol string
		UpstreamAddress  string
		Transport        string
		ServiceName      string
		ListenerPort     int
	}

	UpstreamConnectAcceptor func(UpstreamConnectRequest) bool
	UpstreamConnectHandler  func(UpstreamConnectRequest) (net.Conn, error)

	RouteResolveRequest struct {
		Context          context.Context      `json:"-"`
		Host             string               `json:"host"`
		RawServerHost    string               `json:"raw_server_host,omitempty"`
		SourceAddr       string               `json:"source_addr,omitempty"`
		ProtocolVersion  int                  `json:"protocol_version,omitempty"`
		NextState        int                  `json:"next_state,omitempty"`
		FallbackUpstream string               `json:"fallback_upstream,omitempty"`
		FallbackHit      bool                 `json:"fallback_hit"`
		Refresh          bool                 `json:"refresh,omitempty"`
		Metadata         map[string]string    `json:"metadata,omitempty"`
		Handshake        UpstreamHandshakeRef `json:"handshake,omitempty"`
	}

	UpstreamHandshakeRef struct {
		ServerHost      string `json:"server_host,omitempty"`
		RawServerHost   string `json:"raw_server_host,omitempty"`
		ProtocolVersion int    `json:"protocol_version,omitempty"`
		NextState       int    `json:"next_state,omitempty"`
	}

	RouteDecision struct {
		Action      string            `json:"action"`
		Upstream    string            `json:"upstream,omitempty"`
		Host        string            `json:"host,omitempty"`
		Reason      string            `json:"reason,omitempty"`
		ProviderID  string            `json:"provider_id,omitempty"`
		CacheTTL    time.Duration     `json:"cache_ttl,omitempty"`
		Metadata    map[string]string `json:"metadata,omitempty"`
		Explanation string            `json:"explanation,omitempty"`
	}

	RouteResolveAcceptor func(RouteResolveRequest) bool
	RouteResolveHandler  func(RouteResolveRequest) (RouteDecision, error)

	StatusPingRequest struct {
		Context         context.Context   `json:"-"`
		Host            string            `json:"host"`
		RawServerHost   string            `json:"raw_server_host,omitempty"`
		SourceAddr      string            `json:"source_addr,omitempty"`
		ProtocolVersion int               `json:"protocol_version,omitempty"`
		Metadata        map[string]string `json:"metadata,omitempty"`
	}

	StatusPingResponse struct {
		MOTD              string            `json:"motd,omitempty"`
		Favicon           string            `json:"favicon,omitempty"`
		OnlinePlayers     int               `json:"online_players,omitempty"`
		MaxPlayers        int               `json:"max_players,omitempty"`
		VersionText       string            `json:"version_text,omitempty"`
		ProtocolVersion   int               `json:"protocol_version,omitempty"`
		Maintenance       bool              `json:"maintenance,omitempty"`
		MaintenanceWindow string            `json:"maintenance_window,omitempty"`
		Metadata          map[string]string `json:"metadata,omitempty"`
	}

	StatusPingAcceptor func(StatusPingRequest) bool
	StatusPingHandler  func(StatusPingRequest) (StatusPingResponse, error)

	FilterDecision struct {
		Allow      bool              `json:"allow"`
		Reject     bool              `json:"reject,omitempty"`
		Reason     string            `json:"reason,omitempty"`
		FailPolicy string            `json:"fail_policy,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	ConnectionFilterRequest struct {
		Context    context.Context   `json:"-"`
		SourceAddr string            `json:"source_addr,omitempty"`
		Transport  string            `json:"transport,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	ConnectionFilterAcceptor func(ConnectionFilterRequest) bool
	ConnectionFilterHandler  func(ConnectionFilterRequest) (FilterDecision, error)

	HandshakeFilterRequest struct {
		Context         context.Context   `json:"-"`
		SourceAddr      string            `json:"source_addr,omitempty"`
		ServerHost      string            `json:"server_host"`
		RawServerHost   string            `json:"raw_server_host,omitempty"`
		ProtocolVersion int               `json:"protocol_version,omitempty"`
		NextState       int               `json:"next_state,omitempty"`
		Metadata        map[string]string `json:"metadata,omitempty"`
	}

	HandshakeFilterDecision struct {
		FilterDecision
		RewriteHost string `json:"rewrite_host,omitempty"`
	}

	HandshakeFilterAcceptor func(HandshakeFilterRequest) bool
	HandshakeFilterHandler  func(HandshakeFilterRequest) (HandshakeFilterDecision, error)

	EventDeliveryRequest struct {
		Context      context.Context   `json:"-"`
		PluginID     string            `json:"plugin_id"`
		Name         string            `json:"name"`
		Fields       map[string]string `json:"fields,omitempty"`
		TraceID      string            `json:"trace_id,omitempty"`
		ConnectionID string            `json:"connection_id,omitempty"`
		Attempt      int               `json:"attempt"`
		Mode         string            `json:"mode,omitempty"`
		Metadata     map[string]string `json:"metadata,omitempty"`
	}

	EventDeliveryResult struct {
		OK     bool   `json:"ok"`
		Retry  bool   `json:"retry,omitempty"`
		Reason string `json:"reason,omitempty"`
	}

	EventSubscriberAcceptor func(EventDeliveryRequest) bool
	EventSubscriberHandler  func(EventDeliveryRequest) (EventDeliveryResult, error)

	ProviderRegistration struct {
		Type         string            `json:"type"`
		Name         string            `json:"name"`
		Priority     int               `json:"priority,omitempty"`
		Fallback     bool              `json:"fallback,omitempty"`
		Dependencies []string          `json:"dependencies,omitempty"`
		Metadata     map[string]string `json:"metadata,omitempty"`
	}

	ProviderAcceptor func(ProviderRegistration) bool
	ProviderHandler  func() (ProviderRegistration, error)
)

var (
	RouteDecisionPass     = "pass"
	RouteDecisionOverride = "override"
	RouteDecisionFallback = "fallback"
	RouteDecisionReject   = "reject"

	DeliveryBestEffort  = "best_effort"
	DeliveryAtLeastOnce = "at_least_once"

	FailPolicyOpen  = "fail_open"
	FailPolicyClose = "fail_closed"

	HookUpstreamConnect = HookType[
		UpstreamConnectAcceptor,
		UpstreamConnectHandler,
	]{
		key: "upstream.connect/v1",
	}

	HookUpstream = HookType[
		func(source net.Conn, host string) bool,
		func(source net.Conn, host string) (net.Conn, error),
	]{
		key: "upstream",
	}

	HookRouteResolve = HookType[
		RouteResolveAcceptor,
		RouteResolveHandler,
	]{
		key: "route.resolve/v1",
	}

	HookRouteResolver = HookType[
		RouteResolveAcceptor,
		RouteResolveHandler,
	]{
		key: "route.resolver/v1",
	}

	HookStatusPing = HookType[
		StatusPingAcceptor,
		StatusPingHandler,
	]{
		key: "status.ping/v1",
	}

	HookConnectionFilter = HookType[
		ConnectionFilterAcceptor,
		ConnectionFilterHandler,
	]{
		key: "connection.filter/v1",
	}

	HookHandshakeFilter = HookType[
		HandshakeFilterAcceptor,
		HandshakeFilterHandler,
	]{
		key: "handshake.filter/v1",
	}

	HookEventSubscriber = HookType[
		EventSubscriberAcceptor,
		EventSubscriberHandler,
	]{
		key: "event.subscriber/v1",
	}

	HookProvider = HookType[
		ProviderAcceptor,
		ProviderHandler,
	]{
		key: "provider/v1",
	}

	HookAuthProvider = HookType[
		ProviderAcceptor,
		ProviderHandler,
	]{
		key: "auth.provider/v1",
	}

	HookAdminAuthProvider = HookType[
		ProviderAcceptor,
		ProviderHandler,
	]{
		key: "admin.auth.provider/v1",
	}
)

func (h HookType[Accept, Handler]) Key() string {
	return h.key
}

func (h HookType[Accept, Handler]) AsAny() HookType[any, any] {
	return *(*HookType[any, any])(unsafe.Pointer(&h))
}

func (h HookHandler[Acceptor, Handler]) Acceptor() Acceptor {
	return h.acceptor
}

func (h HookHandler[Acceptor, Handler]) Handler() Handler {
	return h.handler
}
