package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
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

		EmitEvent(ctx context.Context, name string, fields map[string]string) error
		ObserveMetric(ctx context.Context, name string, value float64, labels map[string]string) error
		Logger() Logger
		DataStore() DataStore
		FileStore() FileStore
		ExternalClient(name string) ExternalClient
		RegisterBackgroundTask(task BackgroundTask) error
	}

	EventSchema struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields,omitempty"`
	}

	CustomMetricSchema struct {
		Name   string   `json:"name"`
		Type   string   `json:"type,omitempty"`
		Labels []string `json:"labels,omitempty"`
	}

	Logger interface {
		Debug(ctx context.Context, message string, fields map[string]string)
		Info(ctx context.Context, message string, fields map[string]string)
		Warn(ctx context.Context, message string, fields map[string]string)
		Error(ctx context.Context, message string, fields map[string]string)
	}

	DataRecord struct {
		Key           string
		Value         []byte
		SchemaVersion int
		DataClass     string
		Exportable    bool
		Retention     time.Duration
	}

	DataStore interface {
		Put(ctx context.Context, record DataRecord) error
		Get(ctx context.Context, key string) (DataRecord, error)
		Delete(ctx context.Context, key string) error
	}

	FileStore interface {
		ResourcePath(name string) (string, error)
		Write(ctx context.Context, namespace, name string, data []byte, dataClass string, retention time.Duration) error
		Read(ctx context.Context, namespace, name string, maxBytes int64) ([]byte, error)
		Delete(ctx context.Context, namespace, name string) error
	}

	ExternalRequest struct {
		Method  string
		URL     string
		Header  http.Header
		Body    io.Reader
		Timeout time.Duration
	}

	ExternalResponse struct {
		StatusCode int
		Header     http.Header
		Body       []byte
	}

	ExternalClient interface {
		DoHTTP(ctx context.Context, req ExternalRequest) (ExternalResponse, error)
		DialTCP(ctx context.Context, address string, timeout time.Duration) (net.Conn, error)
		HealthCheck(ctx context.Context) error
	}

	BackgroundTask struct {
		ID         string
		Name       string
		Interval   time.Duration
		RunOnStart bool
		Jitter     time.Duration
		Timeout    time.Duration
		Manual     bool
		Run        func(context.Context) error
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
