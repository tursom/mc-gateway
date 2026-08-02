# 扩展点开发

本文覆盖当前 `plugin features` 暴露的所有 extension point。开发时以 `plugin/api/hook.go` 的类型定义为准。

## 总览

| 扩展点 | Hook | 状态 | 数据面 | 用途 |
| --- | --- | --- | --- | --- |
| `upstream.connect/v2` | `api.HookUpstreamConnectV2` | implemented | 是 | 在 core 读取 Minecraft 数据前接管客户端连接 |
| `route.resolve/v1` | `api.HookRouteResolve` | implemented | 是 | 在 SQLite fallback 之外做动态路由决策 |
| `route.resolver/v1` | `api.HookRouteResolver` | implemented | 是 | `route.resolve/v1` 的 provider/兼容表达 |
| `rule.evaluate/v1` | `api.HookRuleEvaluate` | implemented | 是 | 独立策略/规则评估 |
| `config.validate/v1` | 无独立 SDK hook | implemented | 否 | 配置校验能力声明和 conformance 场景 |
| `status.ping/v1` | `api.HookStatusPing` | implemented | 是 | 自定义 Minecraft server list ping 响应 |
| `connection.filter/v1` | `api.HookConnectionFilter` | implemented | 是 | 握手前连接过滤 |
| `handshake.filter/v1` | `api.HookHandshakeFilter` | implemented | 是 | 握手后过滤或改写 host |
| `event.subscriber/v1` | `api.HookEventSubscriber` | implemented | 否 | 订阅插件事件投递 |
| `provider/v1` | `api.HookProvider` | implemented | 否 | 通用 provider 注册和展示 |
| `auth.provider/v1` | `api.HookAuthProvider` | partial | 否 | provider 注册和状态可见；不接入 core Minecraft 登录流水线 |
| `admin.auth.provider/v1` | `api.HookAdminAuthProvider` | reserved | 否 | 预留管理页外部身份 provider，本地 admin break-glass 仍是实现路径 |
| `ingress.service/v1` | manifest service | partial | 是 | gateway-managed listener 生命周期已建模，受 future runtime gate 约束 |

## `upstream.connect/v2`

该 hook 收到的是已经适配成 `net.Conn` 的原始客户端流。TCP/KCP/QUIC 在任何
Minecraft 字节被 core 消费前进入；WebSocket 在 HTTP Upgrade 完成后进入，并
携带 Upgrade 请求的完整 Header 副本。

```go
type UpstreamConnectRequestV2 struct {
	Context      context.Context
	ConnectionID string
	TraceID      string
	PeerAddr     string
	LocalAddr    string
	Connection   ConnectionState
	Ingress      IngressContext
	Flow         UpstreamConnectFlow
}
```

插件只能替换 `Connection.Stream`、`EffectiveSourceAddr` 和 `Metadata`。
`PeerAddr`、`LocalAddr`、transport、listener、HTTP 和 QUIC facts 在整个链中
保持入口原值。Header 只用于本次分发，不能写入日志、指标、trace、数据库或诊断包。

注册和继续处理：

```go
return api.RegisterUpstreamConnectHandlerV2(gateway,
	func(req api.UpstreamConnectRequestV2) error {
		state := req.Connection
		state.Metadata["checked"] = "true"
		return req.Flow.Next(state)
	})
```

handler 有四种结束方式：

- 不调用 continuation，完整处理连接后返回 `nil`。
- `Next(state)` 阻塞执行下一个插件；链尾进入 core。
- `Core(state)` 跳过剩余插件，直接执行默认握手、路由和转发。
- 关闭连接并返回，用于拒绝。

`Next` 和 `Core` 在一次 handler 调用中合计只能调用一次；重复调用返回
`api.ErrContinuationUsed`。handler error、panic 或 runtime 崩溃都会关闭连接，
不会自动 fallback。插件读取过字节后若仍要继续，必须用能重放已读字节的
`net.Conn` wrapper 作为 replacement stream 传给 continuation。

manifest：

```yaml
extension_points:
  - type: hook
    key: upstream.connect/v2
capabilities:
  extension_points:
    - upstream.connect/v2
```

## `route.resolve/v1` 和 `route.resolver/v1`

用于动态路由、外部 CMDB、灰度、fallback 和拒绝。

```go
api.RegisterHookHandler(gateway, api.HookRouteResolve,
	func(req api.RouteResolveRequest) bool {
		return req.Host == "blue.example"
	},
	func(req api.RouteResolveRequest) (api.RouteDecision, error) {
		return api.RouteDecision{
			Action:      api.RouteDecisionOverride,
			Upstream:    "127.0.0.1:25566",
			ProviderID:  "blue-router",
			CacheTTL:    time.Minute,
			Explanation: "host matched blue.example",
		}, nil
	},
)
```

动作：

| Action | 含义 |
| --- | --- |
| `pass` | 不覆盖，继续后续插件或默认 SQLite fallback |
| `override` | 使用插件返回的 `Upstream` |
| `fallback` | 使用 fallback upstream |
| `reject` | 拒绝连接 |

路由插件应提供 `Reason` 或 `Explanation`，方便 Admin 和 conformance 解释决策。

## `rule.evaluate/v1`

用于独立策略评估，不直接代表 Minecraft 登录或 Admin 登录。

```go
api.RegisterHookHandler(gateway, api.HookRuleEvaluate,
	func(req api.RuleEvaluateRequest) bool {
		return req.Action == "connect"
	},
	func(req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
		if req.Host == "blocked.example" {
			return api.RuleEvaluateDecision{Deny: true, Reason: "blocked host"}, nil
		}
		return api.RuleEvaluateDecision{Allow: true}, nil
	},
)
```

规则插件应在 manifest 中声明涉及的 scope、fail policy 和 conformance fixture，避免规则错误破坏默认路由。

## `status.ping/v1`

用于自定义 Minecraft server list ping。

```go
api.RegisterHookHandler(gateway, api.HookStatusPing,
	func(req api.StatusPingRequest) bool {
		return req.Host == "maintenance.example"
	},
	func(req api.StatusPingRequest) (api.StatusPingResponse, error) {
		return api.StatusPingResponse{
			MOTD:        "Maintenance",
			MaxPlayers: 100,
			Maintenance: true,
		}, nil
	},
)
```

适合 MOTD、favicon、在线人数、维护窗口和版本提示。完整登录或 play 阶段逻辑仍应走 connection takeover。

## `connection.filter/v1`

握手读取前执行，只包含来源和传输信息。

```go
api.RegisterHookHandler(gateway, api.HookConnectionFilter,
	func(req api.ConnectionFilterRequest) bool { return true },
	func(req api.ConnectionFilterRequest) (api.FilterDecision, error) {
		if strings.HasPrefix(req.SourceAddr, "203.0.113.") {
			return api.FilterDecision{Reject: true, Reason: "blocked source"}, nil
		}
		return api.FilterDecision{Allow: true}, nil
	},
)
```

manifest 中建议声明：

```yaml
capabilities:
  middleware:
    fail_policy: fail_open
```

`fail_open` 适合可用性优先场景，`fail_closed` 适合安全优先场景。

## `handshake.filter/v1`

握手解析后执行，可按 host、raw host、协议版本和 next state 过滤或改写 host。

```go
api.RegisterHookHandler(gateway, api.HookHandshakeFilter,
	func(req api.HandshakeFilterRequest) bool {
		return req.ServerHost == "legacy.example"
	},
	func(req api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
		return api.HandshakeFilterDecision{
			FilterDecision: api.FilterDecision{Allow: true},
			RewriteHost: "blue.example",
		}, nil
	},
)
```

改写 host 会影响后续路由和扩展点看到的目标 host。插件应在 conformance 中覆盖 rewrite、reject、error fail-open/fail-closed。

## `event.subscriber/v1`

用于订阅插件事件投递。

```go
api.RegisterHookHandler(gateway, api.HookEventSubscriber,
	func(req api.EventDeliveryRequest) bool {
		return strings.HasPrefix(req.Name, "auth.")
	},
	func(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
		return api.EventDeliveryResult{OK: true}, nil
	},
)
```

能力声明：

```yaml
capabilities:
  event_subscriber:
    mode: at_least_once
    max_retry: 3
```

订阅者失败不应阻塞连接主路径。需要重试时返回 `Retry: true`，并确保事件字段低基数、脱敏。

## Provider 类扩展点

```go
api.RegisterHookHandler(gateway, api.HookProvider,
	func(reg api.ProviderRegistration) bool { return true },
	func() (api.ProviderRegistration, error) {
		return api.ProviderRegistration{
			Type: "route.resolver/v1",
			Name: "external-cmdb",
			Priority: 100,
			Fallback: true,
			Dependencies: []string{"cmdb"},
		}, nil
	},
)
```

`auth.provider/v1` 只是 provider 注册和状态可见，不接入 gateway core 的 Minecraft 登录流水线。MC 登录应由 `upstream.connect/v2` connection takeover 插件完整实现。

`admin.auth.provider/v1` 是预留能力。即使实现外部 OIDC/LDAP/SSO，也必须保留本地 admin break-glass 登录，且不能影响 Minecraft 连接路径。

## `config.validate/v1`

当前作为 manifest、conformance 和治理中的配置校验能力表达。Go SDK 没有独立 `HookConfigValidate`。插件应通过以下方式覆盖配置校验：

- `config_schema` 声明结构。
- `ReloadConfig` 做业务校验。
- `Preflight` 做启用前外部依赖和环境校验。
- `conformance.json` 声明 invalid config fixture。

## `ingress.service/v1`

当前 lifecycle 和 schema 已建模，但受 future runtime gate 约束。开发原则：

- 插件不能自行任意监听生产端口。
- listener 必须由 gateway/supervisor 管理。
- 端口冲突、TLS secret refs、disable drain、reserved listener 冲突必须进入 preflight/governance。
- 不要把 ingress 当作 `upstream.connect/v2` 的变体。

manifest 能力示例：

```yaml
extension_points:
  - type: service
    key: ingress.service/v1
capabilities:
  ingress:
    protocol: tcp
    bind: 0.0.0.0
    port: 25566
    tls:
      enabled: false
    health:
      path: /healthz
      interval: 10s
      timeout: 1s
```

当前实现会把它作为 partial/future-gated 能力处理；不能在文档或 UI 中表达成无条件生产可用。
