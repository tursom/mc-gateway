# 宿主能力

插件通过 `api.Gateway` 使用宿主能力。native Go 插件理论上可以绕过这些能力直接访问网络和文件系统，但开发规范要求优先使用宿主接口，以获得统一观测、审计、脱敏、超时、配额和治理。

## Gateway 接口

```go
type Gateway interface {
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
```

## 事件

Manifest：

```yaml
events:
  - name: auth.failure
    fields:
      - result
      - mode
```

代码：

```go
_ = gateway.EmitEvent(ctx, "auth.failure", map[string]string{
	"result": "backend_unavailable",
	"mode": "fixture",
})
```

规则：

- 只上报 manifest 声明过的事件名和字段。
- 字段保持低基数。
- 不写入玩家名、UUID、token、session response、secret 或 packet payload。
- 事件表达观测结果，不应反向改变 gateway core 的连接决策。

## 自定义指标

Manifest：

```yaml
custom_metrics:
  - name: auth.attempts
    type: counter
    labels:
      - result
      - mode
```

代码：

```go
_ = gateway.ObserveMetric(ctx, "auth.attempts", 1, map[string]string{
	"result": "accepted",
	"mode": "fixture",
})
```

指标标签必须低基数。不要把 host 列表、玩家身份、IP 明细或请求 ID 放进标签；这些信息应进入脱敏日志或诊断摘要。

## 日志

```go
gateway.Logger().Info(ctx, "backend selected", map[string]string{
	"route": "blue",
})
```

日志字段会被宿主清洗和脱敏后保存摘要。插件仍应主动避免写入敏感值。错误日志建议包含稳定 code 和低基数字段，方便 Admin UI 和诊断包聚合。

## DataStore

Manifest：

```yaml
data_stores:
  - name: profile-cache
    schema_version: 1
    data_class: profile_cache
    quota_bytes: 1048576
    retention: 24h
    exportable: false
```

代码：

```go
err := gateway.DataStore().Put(ctx, api.DataRecord{
	Key: "profile/example",
	Value: data,
	SchemaVersion: 1,
	DataClass: "profile_cache",
	Exportable: false,
	Retention: 24 * time.Hour,
})
```

开发规则：

- key 必须稳定，不能包含路径逃逸语义。
- value 大小受配额限制。
- `DataClass` 要和 manifest 对齐。
- 可迁移数据才标记 `Exportable: true`。

## FileStore

Manifest：

```yaml
file_stores:
  - namespace: cache
    data_class: profile_cache
    quota_bytes: 1048576
    retention: 24h
```

代码：

```go
_ = gateway.FileStore().Write(ctx, "cache", "profiles/index.json", data, "profile_cache", 24*time.Hour)
data, err := gateway.FileStore().Read(ctx, "cache", "profiles/index.json", 256*1024)
```

规则：

- 使用命名空间隔离 runtime 文件。
- 读取包内只读资源用 `ResourcePath`。
- 写入 runtime 文件时传 data class 和 retention。
- 不要自行拼接 gateway runtime 目录路径。

## ExternalClient

Manifest：

```yaml
external_dependencies:
  - name: backend
    endpoint: tcp://
    purpose: auth
    required: true
    timeout: 3s
    retry: 0
    fail_policy: fail_closed
    data_classes:
      - operational
```

代码：

```go
conn, err := gateway.ExternalClient("backend").DialTCP(ctx, "127.0.0.1:25566", 3*time.Second)
```

HTTP：

```go
resp, err := gateway.ExternalClient("profile-api").DoHTTP(ctx, api.ExternalRequest{
	Method: http.MethodGet,
	URL: "https://profile.example/internal/health",
	Timeout: 3 * time.Second,
})
```

规则：

- 只使用 manifest 声明过的 dependency name。
- 设置 timeout，传递 context。
- 根据 `fail_policy` 明确外部依赖失败时的行为。
- native 插件不能被强制禁止直接联网，但直接联网会绕过统一健康、熔断和审计。

## 后台任务

Manifest：

```yaml
background_tasks:
  - id: profile-cache-gc
    name: Profile cache GC
    mode: manual
    manual: true
    timeout: 1s
    run_policy: per_node
```

代码：

```go
err := gateway.RegisterBackgroundTask(api.BackgroundTask{
	ID: "profile-cache-gc",
	Name: "Profile cache GC",
	Manual: true,
	Timeout: time.Second,
	Run: func(ctx context.Context) error {
		return cleanupProfiles(ctx)
	},
})
```

规则：

- task ID 必须稳定。
- 周期任务要设置 jitter，避免多实例同时打外部依赖。
- 手动任务必须可超时、可取消、可审计。
- 任务失败不应破坏连接 hot path。

## 连接所有权

`Gateway.HandleConn` 已删除。`upstream.connect/v2` handler 通过阻塞的
`Flow.Next` 或 `Flow.Core` 交还连接；不调用 continuation 表示插件完整处理。
handler 返回后宿主关闭 root connection。

## 敏感信息约束

以下内容默认不能进入日志、事件、指标标签、普通诊断或审计明文：

- secret、token、session response
- 玩家 UUID、玩家名，除非有明确脱敏或聚合策略
- packet payload
- WebSocket Upgrade request headers
- 外部系统原始响应体
- 私有网络完整拓扑

需要诊断时生成脱敏摘要，或把敏感值留在受权限控制的专用 secret/config 通道。
