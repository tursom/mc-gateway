// plugin/api/api.go 定义托管网关插件从宿主进程获得的稳定 API 能力。

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

	// PreflightContext 是治理预检时传给插件的上下文，包含当前配置、
	// 发布动作、运行限制和 manifest 中声明的范围信息。
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

	// SelfTestProfile 描述插件自检运行的配置档位，例如 dev 或 prod。
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

	// Gateway 是宿主暴露给插件的能力集合。插件只能通过这些方法注册钩子、
	// 上报观测数据、访问受限存储或创建后台任务。
	Gateway interface {
		// ExitWaitGroup 返回进程退出等待组，插件启动的长期 goroutine 应纳入该等待组。
		ExitWaitGroup() *sync.WaitGroup

		// Hook 注册钩子处理器。推荐通过 RegisterHookHandler 使用类型安全包装。
		Hook(hook string, handler any) error

		// EmitEvent 上报 manifest 声明的低基数事件。
		EmitEvent(ctx context.Context, name string, fields map[string]string) error
		// ObserveMetric 上报 manifest 声明的自定义指标最近值。
		ObserveMetric(ctx context.Context, name string, value float64, labels map[string]string) error
		// Logger 返回会自动脱敏并落库摘要的插件日志器。
		Logger() Logger
		// DataStore 返回按插件隔离、受配额限制的键值数据存储。
		DataStore() DataStore
		// FileStore 返回按插件和命名空间隔离的运行态文件存储。
		FileStore() FileStore
		// ExternalClient 返回 manifest 中声明的外部依赖客户端。
		ExternalClient(name string) ExternalClient
		// RegisterBackgroundTask 注册可由宿主调度或手动触发的后台任务。
		RegisterBackgroundTask(task BackgroundTask) error
	}

	// EventSchema 声明插件可上报的事件名和字段白名单。
	EventSchema struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields,omitempty"`
	}

	// CustomMetricSchema 声明插件可上报的指标名、类型和标签白名单。
	CustomMetricSchema struct {
		Name   string   `json:"name"`
		Type   string   `json:"type,omitempty"`
		Labels []string `json:"labels,omitempty"`
	}

	// Logger 是插件日志接口。字段会被宿主清洗和脱敏后保存为摘要。
	Logger interface {
		Debug(ctx context.Context, message string, fields map[string]string)
		Info(ctx context.Context, message string, fields map[string]string)
		Warn(ctx context.Context, message string, fields map[string]string)
		Error(ctx context.Context, message string, fields map[string]string)
	}

	// DataRecord 是插件键值存储的一条记录。Retention 为 0 表示不过期。
	DataRecord struct {
		Key           string
		Value         []byte
		SchemaVersion int
		DataClass     string
		Exportable    bool
		Retention     time.Duration
	}

	// DataStore 为插件提供受配额限制的持久化键值存储。
	DataStore interface {
		Put(ctx context.Context, record DataRecord) error
		Get(ctx context.Context, key string) (DataRecord, error)
		Delete(ctx context.Context, key string) error
	}

	// FileStore 为插件提供受命名空间和路径校验保护的运行态文件存储。
	FileStore interface {
		// ResourcePath 返回制品随包发布的只读资源路径。
		ResourcePath(name string) (string, error)
		// Write 写入运行态文件，并记录数据分类和保留时间。
		Write(ctx context.Context, namespace, name string, data []byte, dataClass string, retention time.Duration) error
		// Read 读取运行态文件，maxBytes 用于限制单次读取大小。
		Read(ctx context.Context, namespace, name string, maxBytes int64) ([]byte, error)
		// Delete 删除运行态文件和对应仓库记录。
		Delete(ctx context.Context, namespace, name string) error
	}

	// ExternalRequest 描述一次通过宿主外部依赖客户端发出的 HTTP 请求。
	ExternalRequest struct {
		Method  string
		URL     string
		Header  http.Header
		Body    io.Reader
		Timeout time.Duration
	}

	// ExternalResponse 保存外部 HTTP 调用返回的状态、头和受限大小的响应体。
	ExternalResponse struct {
		StatusCode int
		Header     http.Header
		Body       []byte
	}

	// ExternalClient 只允许访问 manifest 声明过的外部依赖，并由宿主负责超时、
	// 观测、简单重试和熔断。
	ExternalClient interface {
		DoHTTP(ctx context.Context, req ExternalRequest) (ExternalResponse, error)
		DialTCP(ctx context.Context, address string, timeout time.Duration) (net.Conn, error)
		HealthCheck(ctx context.Context) error
	}

	// BackgroundTask 描述插件注册给宿主调度的后台任务。
	BackgroundTask struct {
		// ID 必须稳定且唯一，用于管理端触发、日志和调度状态展示。
		ID   string
		Name string
		// Interval 为 0 表示不自动周期执行。
		Interval   time.Duration
		RunOnStart bool
		// Jitter 用于打散周期任务，避免多个插件或实例同时触发。
		Jitter  time.Duration
		Timeout time.Duration
		// Manual 为 true 时只允许通过管理端手动触发。
		Manual bool
		Run    func(context.Context) error
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

// RegisterUpstreamConnectHandlerV2 注册客户端连接接管处理器。该扩展点没有
// 独立 acceptor；不处理当前连接时，handler 应调用 request.Flow.Next。
func RegisterUpstreamConnectHandlerV2(gateway Gateway, handler UpstreamConnectHandlerV2) error {
	return gateway.Hook(HookUpstreamConnectV2.key, handler)
}
