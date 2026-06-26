package api

import (
	"net"
	"sync"
)

type (
	Plugin interface {
		// Init 初始化插件
		// 返回错误表示初始化失败，插件将不会被启用
		Init(gateway Gateway) error
		// Destroy 销毁插件
		// 返回错误表示销毁失败，通常是资源释放失败
		// 但插件仍然会被标记为已销毁，后续调用 Init() 会重新初始化
		Destroy() error

		// NewConfigObj 创建插件配置对象
		NewConfigObj() any
		// ReloadConfig 重新加载插件配置
		// config 是 NewConfigObj() 返回的对象
		// 如果返回错误，则表示配置加载失败，插件将不会被启用
		ReloadConfig(config any) error
	}

	PreflightCheck struct {
		Code     string `json:"code"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	}

	PreflightContext struct {
		PluginID      string         `json:"plugin_id"`
		ArtifactID    string         `json:"artifact_id"`
		Profile       string         `json:"profile"`
		Action        string         `json:"action"`
		Config        map[string]any `json:"config,omitempty"`
		Scope         any            `json:"scope,omitempty"`
		Rollout       any            `json:"rollout,omitempty"`
		RuntimeLimits any            `json:"runtime_limits,omitempty"`
		Features      []string       `json:"features,omitempty"`
	}

	PreflightResult struct {
		Checks []PreflightCheck `json:"checks"`
	}

	SelfTestProfile struct {
		Name string `json:"name"`
	}

	SelfTestResult struct {
		Checks []PreflightCheck `json:"checks"`
	}

	PreflightChecker interface {
		Preflight(context any) (PreflightResult, error)
	}

	SelfTester interface {
		SelfTest(profile SelfTestProfile) (SelfTestResult, error)
	}

	Gateway interface {
		HandleConn(conn net.Conn)

		ExitWaitGroup() *sync.WaitGroup

		Hook(hook string, handler any) error
	}

	AbstractPlugin struct{}
)

// Init 初始化插件
// 返回错误表示初始化失败，插件将不会被启用
func (p AbstractPlugin) Init(gateway Gateway) error {
	return nil // 默认实现，返回 nil 表示初始化成功
}

// Destroy 销毁插件
// 返回错误表示销毁失败，通常是资源释放失败
// 但插件仍然会被标记为已销毁，后续调用 Init() 会重新初始化
func (p AbstractPlugin) Destroy() error {
	return nil // 默认实现，返回 nil 表示销毁成功
}

// NewConfigObj 创建插件配置对象
func (p AbstractPlugin) NewConfigObj() any {
	return struct{}{} // 默认实现，返回一个空结构体
}

// ReloadConfig 重新加载插件配置
// config 是 NewConfigObj() 返回的对象
// 如果返回错误，则表示配置加载失败，插件将不会被启用
func (p AbstractPlugin) ReloadConfig(config any) error {
	return nil // 默认实现，返回 nil 表示配置加载成功
}

func RegisterHookHandler[Accept, Handle any](
	gateway Gateway,
	hook HookType[Accept, Handle],
	accept Accept,
	handler Handle,
) error {
	return gateway.Hook(hook.key, HookHandler[Accept, Handle]{accept, handler})
}
