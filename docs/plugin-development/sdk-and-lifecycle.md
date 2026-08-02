# SDK 生命周期和入口

Go 插件通过 `plugin/api` 与 gateway 交互。当前主路径是进程内 `go-plugin`，插件必须导出 `Plugin` 符号。

## 入口符号

```go
package main

import "github.com/tursom/mc-gateway/plugin/api"

type PluginImpl struct {
	api.AbstractPlugin
}

func Plugin() api.Plugin {
	return &PluginImpl{}
}
```

`runtime.entry_symbol` 默认是 `Plugin`。如果改名，manifest 和代码必须一致；否则 build 后的符号校验或服务端加载会失败。

## 生命周期接口

```go
type Plugin interface {
	Init(gateway Gateway) error
	Destroy() error
	NewConfigObj() any
	ReloadConfig(config any) error
}
```

| 方法 | 触发时机 | 规则 |
| --- | --- | --- |
| `Init` | 插件加载并准备启用时 | 注册 hook、初始化轻量资源；返回错误会阻止启用 |
| `Destroy` | 禁用、替换或进程退出清理时 | 释放资源；不能依赖 Go plugin 真卸载代码 |
| `NewConfigObj` | 配置解析前 | 返回可被 JSON 解码的配置结构 |
| `ReloadConfig` | 初次启用、配置变更、回滚时 | 做默认值和业务校验；返回错误会阻止配置应用 |

推荐写法：

```go
type PluginImpl struct {
	api.AbstractPlugin
	config Config
	gateway api.Gateway
}

type Config struct {
	MatchHost string `json:"match_host"`
	Upstream  string `json:"upstream"`
}

func (p *PluginImpl) NewConfigObj() any {
	return &Config{}
}

func (p *PluginImpl) ReloadConfig(config any) error {
	cfg, ok := config.(*Config)
	if !ok {
		return fmt.Errorf("unexpected config type %T", config)
	}
	if cfg.Upstream == "" {
		return errors.New("upstream is required")
	}
	p.config = *cfg
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	p.gateway = gateway
	return api.RegisterUpstreamConnectHandlerV2(gateway, p.takeover)
}
```

## Hook 注册

优先使用类型安全的 `api.RegisterHookHandler`：

```go
return api.RegisterHookHandler(
	gateway,
	api.HookRouteResolve,
	func(req api.RouteResolveRequest) bool {
		return req.Host == "blue.example"
	},
	func(req api.RouteResolveRequest) (api.RouteDecision, error) {
		return api.RouteDecision{
			Action:   api.RouteDecisionOverride,
			Upstream: "127.0.0.1:25566",
			Reason:   "blue route",
		}, nil
	},
)
```

`Acceptor` 返回 `false` 表示当前 handler 不处理该请求。`Handler` 返回值的语义由扩展点决定。

错误语义：

| 返回 | 含义 |
| --- | --- |
| `nil` error + 有效响应 | 插件处理成功 |
| `api.ErrPass` | 当前插件主动跳过，让后续插件或默认流程继续 |
| `api.ErrBlocked` | 插件明确阻断操作 |
| 其它 error | 视扩展点和 fail policy 处理，并记录错误计数 |
| panic | gateway 会 recover 并记录；`upstream.connect/v2` 直接关闭连接，不 fallback |

## 超时和 Context

`runtime_limits.handler_timeout_ms` 控制多数扩展点 handler 的执行超时。插件必须：

- 使用请求中的 `Context`。
- 外部 IO 传递 context 或设置 deadline。
- 不在 hot path 中做无限等待。
- 对 connection takeover 和 background task 明确处理 deadline、取消与 drain。

示例：

```go
func (p *PluginImpl) resolve(req api.RouteResolveRequest) (api.RouteDecision, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return api.RouteDecision{}, ctx.Err()
	default:
	}
	return api.RouteDecision{Action: api.RouteDecisionPass}, nil
}
```

## Goroutine 和退出

长期 goroutine 应纳入 `gateway.ExitWaitGroup()`：

```go
func (p *PluginImpl) Init(gateway api.Gateway) error {
	wg := gateway.ExitWaitGroup()
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.runLoop()
	}()
	return nil
}
```

不要让 goroutine 持有未关闭的连接、ticker 或 channel。`Destroy` 应停止插件自己创建的后台资源。

## 配置热加载

`ReloadConfig` 可能在启用、配置更新、回滚时调用。建议：

- 先完整校验新配置，再原子替换内存配置。
- 不在校验失败时修改旧配置。
- 对需要重建连接池、缓存或外部客户端的配置，先准备新资源，再切换引用。
- secret 轮换时支持 dual-read 或明确声明需要 reload/restart。

Go plugin 代码不能真正卸载。禁用后新连接不会进入插件；已有 connection
session 会 drain，管理员也可以强制关闭 root connection 来解开整条嵌套调用链。

## Preflight 和 Self-test

插件可选实现：

```go
type PreflightChecker interface {
	Preflight(context any) (api.PreflightResult, error)
}

type SelfTester interface {
	SelfTest(profile api.SelfTestProfile) (api.SelfTestResult, error)
}
```

用途：

- `Preflight` 检查启用、回滚或 promotion 前的配置、外部依赖和风险。
- `SelfTest` 检查插件内部健康，例如本地 fixture、外部依赖可达性或缓存状态。

返回的 check 必须可审计：

```go
return api.PreflightResult{Checks: []api.PreflightCheck{
	{Code: "backend_reachable", Severity: "info", Message: "backend health check passed"},
}}, nil
```

不要在 preflight 中修改生产状态。
