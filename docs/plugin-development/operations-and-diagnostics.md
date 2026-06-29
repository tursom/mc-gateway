# 运维和诊断

插件开发不是只写数据面 handler。每个生产插件都应提供可观测、可诊断、可清理、可降级的运行面。本页覆盖 `plugin features` 中的 operations 能力。

## 运行面命令

开发和验收时至少会用到：

```sh
go run ./cmd/gateway plugin logs <plugin-id>
go run ./cmd/gateway plugin events <plugin-id>
go run ./cmd/gateway plugin metrics <plugin-id>
go run ./cmd/gateway plugin diagnose <plugin-id>
go run ./cmd/gateway plugin task list <plugin-id>
go run ./cmd/gateway plugin task run <plugin-id> <task-id> --confirm-token <token>
go run ./cmd/gateway plugin task cancel <plugin-id> <task-id>
go run ./cmd/gateway plugin external list <plugin-id>
go run ./cmd/gateway plugin external health-check <plugin-id> <dependency>
go run ./cmd/gateway plugin data inspect <plugin-id>
go run ./cmd/gateway plugin data gc <plugin-id>
go run ./cmd/gateway plugin files inspect <plugin-id>
go run ./cmd/gateway plugin files gc <plugin-id>
go run ./cmd/gateway plugin gc
```

这些命令都通过 Admin API 读取同一个 operations snapshot。插件作者应让 manifest 声明和代码上报能支撑这些视图。

## 日志

代码：

```go
gateway.Logger().Info(ctx, "profile cache refreshed", map[string]string{
	"result": "ok",
	"source": "manual",
})
```

要求：

- 日志 message 使用稳定短语，字段使用低基数字符串。
- 错误日志带稳定 code 或 reason。
- 不写 secret、token、session response、packet payload、玩家 UUID 或玩家名。
- 如果必须定位单个玩家问题，使用脱敏 hash 或专门诊断包，不进入普通日志字段。

## 事件队列

事件声明：

```yaml
events:
  - name: auth.failure
    fields: [result, mode]
```

开发要求：

- 事件字段必须低基数。
- 事件队列满或 subscriber 失败不能阻塞连接 hot path。
- `event.subscriber/v1` 插件必须覆盖 best-effort、at-least-once、retry、dead-letter 或 drop 策略。
- 事件是观测，不是 core 连接决策输入。

## 指标

指标声明：

```yaml
custom_metrics:
  - name: auth.attempts
    type: counter
    labels: [result, mode]
```

要求：

- label 值长度和基数要受控。
- 不把 IP、玩家身份、trace ID、connection ID 放进 label。
- 指标只表达聚合状态；需要细节时用日志或诊断包。

## 诊断包

`plugin diagnose <plugin-id>` 应能回答：

- 当前 desired/runtime state 是否一致。
- artifact、config、extension points、providers 是否匹配。
- 最近日志、事件、指标和 handler 错误摘要。
- background task 最近运行状态。
- external dependency 最近健康状态。
- data/file quota、retention 和 GC candidate。
- node runtime state、rollout partial failure 和 stale node 情况。

插件开发要求：

- 提供足够的脱敏摘要字段。
- 不依赖读取 secret 明文生成诊断。
- 诊断中间文件放入声明过的 `file_stores` namespace，例如 `diagnostic`。
- 默认诊断保留期按 operations retention 策略清理。

## 后台任务

Manifest：

```yaml
background_tasks:
  - id: sync-routes
    name: Sync routes
    mode: interval
    interval: 1m
    jitter: 10s
    timeout: 5s
    retry: 2
    run_policy: singleton
    lease_ttl: 30s
```

开发要求：

- `ID` 稳定，不能随版本随机变化。
- interval 任务设置 jitter。
- 长任务必须尊重 context cancel。
- `run_policy` 明确 per-node、singleton 或 sharded。
- 需要手动触发的任务设置 `manual: true` 和 confirm token 流程。
- 任务失败应更新运维摘要，不应破坏连接 hot path。

## 外部依赖健康

Manifest：

```yaml
external_dependencies:
  - name: profile-api
    endpoint: https://profile.example
    purpose: profile-cache
    required: true
    timeout: 3s
    retry: 1
    fail_policy: degraded
    traceparent: true
```

代码应优先使用：

```go
err := gateway.ExternalClient("profile-api").HealthCheck(ctx)
```

开发要求：

- health-check 只做轻量探测。
- 区分 `fail_open`、`fail_closed`、`degraded` 和 `fallback`。
- 外部依赖超时、熔断和连续失败要能在 `external list` 中解释。
- 不把外部响应体原文写入日志或事件。

## Data/File 资源和 GC

DataStore 和 FileStore 都必须有 manifest 声明。开发时要设计：

- quota bytes
- retention
- data class
- exportable
- namespace
- readonly resources

GC 命令：

```sh
go run ./cmd/gateway plugin data gc <plugin-id>
go run ./cmd/gateway plugin data gc <plugin-id> --dry-run=false
go run ./cmd/gateway plugin files gc <plugin-id>
go run ./cmd/gateway plugin files gc <plugin-id> --dry-run=false
go run ./cmd/gateway plugin gc
```

规则：

- dry-run 必须解释 protected candidates。
- cache/tmp/log/diagnostic 可按 retention 清理。
- data 类资源只有在 manifest retention/exportable 策略允许时清理。
- 删除插件时要区分 artifact GC、operations GC 和 runtime data retention。

## 多实例和节点状态

operations fact source 支持 node state、artifact distribution、partial rollout failure 和 cross-node apply。插件开发者需要：

- 不假设只有一个 gateway 实例。
- 后台任务按 run policy 设计 lease。
- provider 和 route resolver 输出可解释 decision。
- promotion 和 repository apply 不自动启用生产流量。
- partial failure 时，诊断能显示哪些 node ready、failed、stale。

## 外部 Exporter

Prometheus 和 OTel exporter 当前在 fact source 的 `external_exporters` 中是 reserved。开发者可以准备低基数指标和脱敏事件，但不能把外部 exporter 当作已启用 sink。当前权威运行态摘要仍是 in-process operations snapshot。

## 验收清单

生产插件至少提供：

- `logs/events/metrics` 中能看到低基数摘要。
- `diagnose` 不含敏感明文。
- `task list` 能解释后台任务状态。
- `external list` 能解释依赖健康。
- `data/files inspect` 能解释配额和 retention。
- `gc` dry-run 能解释可清理和受保护对象。
- 多实例场景下不会重复执行 singleton 任务。
