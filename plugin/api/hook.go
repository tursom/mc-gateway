// plugin/api/hook.go 定义插件钩子键、类型化钩子契约、请求模型和默认决策。

package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
	"unsafe"
)

var (
	// UnsupportedHookType 表示宿主不认识插件注册的钩子类型。
	UnsupportedHookType = errors.New("unsupported hook type")
	// ErrPass 表示当前处理器主动放弃处理，让后续处理器继续尝试。
	ErrPass = errors.New("plugin handler pass")
	// ErrBlocked 表示插件明确阻断当前连接或操作。
	ErrBlocked = errors.New("plugin handler blocked")
	// ErrContinuationUsed 表示同一个连接处理器重复选择后续流程。
	ErrContinuationUsed = errors.New("upstream continuation already used")
)

type (
	// ConnectionIDContextKey 和 TraceIDContextKey 保留给需要通过 context 传递链路标识的插件。
	ConnectionIDContextKey struct{}
	TraceIDContextKey      struct{}

	// HookType 描述一个类型安全的钩子键，Accept 是筛选函数类型，Handler 是处理函数类型。
	HookType[Accept, Handler any] struct {
		key string
	}

	// HookHandler 把筛选函数和处理函数成对注册到同一个钩子上。
	HookHandler[Accept, Handler any] struct {
		acceptor Accept
		handler  Handler
	}

	// ConnectionState 是插件可以交给后续插件或 core 的可变连接状态。
	// PeerAddr、入口传输等原始事实不在这里，不能被插件改写。
	ConnectionState struct {
		Stream              net.Conn          `json:"-"`
		EffectiveSourceAddr string            `json:"effective_source_addr,omitempty"`
		Metadata            map[string]string `json:"metadata,omitempty"`
	}

	HTTPIngressContext struct {
		Method  string      `json:"method,omitempty"`
		Host    string      `json:"host,omitempty"`
		Path    string      `json:"path,omitempty"`
		Headers http.Header `json:"headers,omitempty"`
	}

	QUICIngressContext struct {
		ApplicationProtocol string `json:"application_protocol,omitempty"`
	}

	IngressContext struct {
		Transport    string              `json:"transport,omitempty"`
		ServiceName  string              `json:"service_name,omitempty"`
		ListenerPort int                 `json:"listener_port,omitempty"`
		HTTP         *HTTPIngressContext `json:"http,omitempty"`
		QUIC         *QUICIngressContext `json:"quic,omitempty"`
	}

	// UpstreamConnectFlow 是每个处理器的一次性 continuation。Next 进入下一个
	// v2 插件（没有下一个时进入 core），Core 直接跳过剩余插件。
	UpstreamConnectFlow interface {
		Next(ConnectionState) error
		Core(ConnectionState) error
	}

	// UpstreamConnectRequestV2 在 core 读取任何 Minecraft 字节前把客户端连接
	// 交给插件。handler 在返回前拥有 Connection.Stream。
	UpstreamConnectRequestV2 struct {
		Context      context.Context     `json:"-"`
		ConnectionID string              `json:"connection_id,omitempty"`
		TraceID      string              `json:"trace_id,omitempty"`
		PeerAddr     string              `json:"peer_addr,omitempty"`
		LocalAddr    string              `json:"local_addr,omitempty"`
		Connection   ConnectionState     `json:"connection"`
		Ingress      IngressContext      `json:"ingress"`
		Flow         UpstreamConnectFlow `json:"-"`
	}

	UpstreamConnectHandlerV2 func(UpstreamConnectRequestV2) error

	// RouteResolveRequest 描述一次主机路由解析请求，并携带 SQLite 快照的兜底结果。
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

	// UpstreamHandshakeRef 是路由请求中稳定的握手摘要，便于插件记录或转发。
	UpstreamHandshakeRef struct {
		ServerHost      string `json:"server_host,omitempty"`
		RawServerHost   string `json:"raw_server_host,omitempty"`
		ProtocolVersion int    `json:"protocol_version,omitempty"`
		NextState       int    `json:"next_state,omitempty"`
	}

	// RouteDecision 是插件路由解析的返回值。Action 决定覆盖、兜底、拒绝或继续传递。
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

	// RuleEvaluateRequest 描述一次可复用的策略/规则评估请求。它不直接接管
	// Minecraft 登录或 Admin 登录数据面，只给插件和 conformance 提供稳定评估契约。
	RuleEvaluateRequest struct {
		Context    context.Context   `json:"-"`
		Subject    string            `json:"subject,omitempty"`
		Action     string            `json:"action,omitempty"`
		Resource   string            `json:"resource,omitempty"`
		Host       string            `json:"host,omitempty"`
		SourceAddr string            `json:"source_addr,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	// RuleEvaluateDecision 是规则评估结果。Allow/Deny 显式表达决策；Reject 用于
	// 兼容过滤类插件的写法；Reason/Metadata 用于审计和 explain。
	RuleEvaluateDecision struct {
		Allow      bool              `json:"allow"`
		Deny       bool              `json:"deny,omitempty"`
		Reject     bool              `json:"reject,omitempty"`
		Reason     string            `json:"reason,omitempty"`
		ProviderID string            `json:"provider_id,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	RuleEvaluateAcceptor func(RuleEvaluateRequest) bool
	RuleEvaluateHandler  func(RuleEvaluateRequest) (RuleEvaluateDecision, error)

	// StatusPingRequest 描述 Minecraft 状态查询请求，插件可以直接生成响应。
	StatusPingRequest struct {
		Context         context.Context   `json:"-"`
		Host            string            `json:"host"`
		RawServerHost   string            `json:"raw_server_host,omitempty"`
		SourceAddr      string            `json:"source_addr,omitempty"`
		ProtocolVersion int               `json:"protocol_version,omitempty"`
		Metadata        map[string]string `json:"metadata,omitempty"`
	}

	// StatusPingResponse 是插件返回给客户端的状态信息，最终会被宿主封成 Minecraft packet。
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

	// FilterDecision 描述连接或握手过滤结果。Allow 和 Reject 用于兼容不同插件写法。
	FilterDecision struct {
		Allow      bool              `json:"allow"`
		Reject     bool              `json:"reject,omitempty"`
		Reason     string            `json:"reason,omitempty"`
		FailPolicy string            `json:"fail_policy,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	// ConnectionFilterRequest 在读取 Minecraft 握手前触发，只包含来源和传输信息。
	ConnectionFilterRequest struct {
		Context    context.Context   `json:"-"`
		SourceAddr string            `json:"source_addr,omitempty"`
		Transport  string            `json:"transport,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}

	ConnectionFilterAcceptor func(ConnectionFilterRequest) bool
	ConnectionFilterHandler  func(ConnectionFilterRequest) (FilterDecision, error)

	// HandshakeFilterRequest 在握手解析后触发，可按主机名、协议版本和 next state 过滤。
	HandshakeFilterRequest struct {
		Context         context.Context   `json:"-"`
		SourceAddr      string            `json:"source_addr,omitempty"`
		ServerHost      string            `json:"server_host"`
		RawServerHost   string            `json:"raw_server_host,omitempty"`
		ProtocolVersion int               `json:"protocol_version,omitempty"`
		NextState       int               `json:"next_state,omitempty"`
		Metadata        map[string]string `json:"metadata,omitempty"`
	}

	// HandshakeFilterDecision 在过滤结果之外允许改写目标主机名。
	HandshakeFilterDecision struct {
		FilterDecision
		RewriteHost string `json:"rewrite_host,omitempty"`
	}

	HandshakeFilterAcceptor func(HandshakeFilterRequest) bool
	HandshakeFilterHandler  func(HandshakeFilterRequest) (HandshakeFilterDecision, error)

	// EventDeliveryRequest 是插件事件订阅者收到的投递请求。
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

	// EventDeliveryResult 控制事件订阅投递是否成功以及是否需要重试。
	EventDeliveryResult struct {
		OK     bool   `json:"ok"`
		Retry  bool   `json:"retry,omitempty"`
		Reason string `json:"reason,omitempty"`
	}

	EventSubscriberAcceptor func(EventDeliveryRequest) bool
	EventSubscriberHandler  func(EventDeliveryRequest) (EventDeliveryResult, error)

	// ProviderRegistration 描述插件向宿主声明的能力提供方，例如路由提供方。
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

// Clone 返回入口事实的深副本，避免插件通过 Header 或指针字段改写后续处理器看到的原始入口信息。
func (c IngressContext) Clone() IngressContext {
	if c.HTTP != nil {
		httpIngress := *c.HTTP
		httpIngress.Headers = c.HTTP.Headers.Clone()
		c.HTTP = &httpIngress
	}
	if c.QUIC != nil {
		quicIngress := *c.QUIC
		c.QUIC = &quicIngress
	}
	return c
}

var (
	// 路由决策动作使用字符串，方便 manifest、JSON API 和插件代码共享。
	RouteDecisionPass     = "pass"
	RouteDecisionOverride = "override"
	RouteDecisionFallback = "fallback"
	RouteDecisionReject   = "reject"

	DeliveryBestEffort  = "best_effort"
	DeliveryAtLeastOnce = "at_least_once"

	FailPolicyOpen  = "fail_open"
	FailPolicyClose = "fail_closed"

	// HookUpstreamConnectV2 在 Minecraft 解析前转移客户端连接所有权。
	HookUpstreamConnectV2 = HookType[
		struct{},
		UpstreamConnectHandlerV2,
	]{
		key: "upstream.connect/v2",
	}

	// HookRouteResolve 允许插件覆盖或拒绝主机到上游的路由结果。
	HookRouteResolve = HookType[
		RouteResolveAcceptor,
		RouteResolveHandler,
	]{
		key: "route.resolve/v1",
	}

	// HookRouteResolver 是 RouteResolve 的兼容别名。
	HookRouteResolver = HookType[
		RouteResolveAcceptor,
		RouteResolveHandler,
	]{
		key: "route.resolver/v1",
	}

	// HookRuleEvaluate 提供独立策略/规则评估扩展点，用于可执行 conformance
	// 和通用策略插件；是否接入具体数据面由宿主显式决定。
	HookRuleEvaluate = HookType[
		RuleEvaluateAcceptor,
		RuleEvaluateHandler,
	]{
		key: "rule.evaluate/v1",
	}

	// HookStatusPing 允许插件直接回答 Minecraft 状态查询。
	HookStatusPing = HookType[
		StatusPingAcceptor,
		StatusPingHandler,
	]{
		key: "status.ping/v1",
	}

	// HookConnectionFilter 在握手读取前执行，适合按 IP 或传输类型做轻量拦截。
	HookConnectionFilter = HookType[
		ConnectionFilterAcceptor,
		ConnectionFilterHandler,
	]{
		key: "connection.filter/v1",
	}

	// HookHandshakeFilter 在握手解析后执行，适合按目标主机名或协议版本过滤。
	HookHandshakeFilter = HookType[
		HandshakeFilterAcceptor,
		HandshakeFilterHandler,
	]{
		key: "handshake.filter/v1",
	}

	// HookEventSubscriber 让插件订阅其他插件上报的事件。
	HookEventSubscriber = HookType[
		EventSubscriberAcceptor,
		EventSubscriberHandler,
	]{
		key: "event.subscriber/v1",
	}

	// HookProvider 让插件声明自己提供的能力，供管理端和调度逻辑展示。
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
