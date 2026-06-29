# M5：Operations 生产验收完成证据

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

让运维能从 Admin/API/CLI 排查插件故障、清理资源和解释多实例状态，而不是只看到内部模型。

## 完成状态

M5 已完成本阶段要求的生产验收闭环。真实外部 Prometheus/OTel sink 仍未默认启用；当前完成的是 exporter 边界、失败降级、输出门禁和回滚策略的事实表达与测试验收。CLI fact source 必须继续显示该边界为 reserved/disabled，不能把它描述成已启用的外部集成。

## 验收证据

1. 外部 exporter 边界
   - `Operations.ConfigureExporter`、`ExportSnapshot` 和 `RollbackExporter` 表达启用、失败降级、fail-open、回滚和状态摘要。
   - exporter 失败只更新 degraded/last_error/failure_count，不阻断 `ConnectUpstream` 连接路径。
   - exporter 输出边界拒绝敏感 label、超长 label value 和过多 label。
   - `go run ./cmd/gateway plugin features` 的 `operations.external_exporters` 显示 Prometheus/OTel `enabled=false`、`maturity=reserved`、fail-open 和 label/sensitive gate。
   - 测试：`TestOperationsExporterBoundaryFailsOpenAndValidatesOutput`、`TestPluginFeaturesAndManifestCommands`。

2. 跨类别 retention 和 GC
   - GC dry-run 输出候选对象、大小、原因、protected 状态和 retention rule。
   - retention rule 覆盖 diagnostic package、event、metric、trace、background task、PluginDataStore 和 PluginFileStore。
   - GC apply 删除过期 plugin data、过期 plugin file、过期 diagnostic package、orphan runtime file、过期 task lease、event/log/trace overflow，并写 `plugin_operations_gc` 审计。
   - 未过期 diagnostic package 和未过期 PluginFileStore 文件保持 protected。
   - 测试：`TestManagerOperationsBackgroundTaskDataQuotaExternalAndGC`、`TestManagerOperationsRecordsHandlerMetricsEventsAndDiagnostics`。

3. 多实例运维
   - gateway node heartbeat、plugin node runtime state、partial rollout、artifact distribution 状态和 cross-node apply fact 已可查询。
   - singleton lease skip、lease renew、lease lost cancellation 和 sharded task assignment 已有跨节点或跨节点所有权场景。
   - node-b 对 node-a 持有的 singleton task lease 执行 GC dry-run/apply 时必须保持 protected。
   - 测试：`TestManagerPluginNodeRuntimeStateAndPartialRollout`、`TestManagerBackgroundTaskSingletonLeaseSkipsSecondNode`、`TestManagerBackgroundTaskRenewsSingletonLease`、`TestManagerBackgroundTaskCancelsWhenSingletonLeaseLost`、`TestManagerBackgroundTaskShardedLeaseRunsAcrossNodes`。

4. ExternalClient 验收
   - 覆盖 timeout、retry、fail policy、circuit breaker、health status 和 recent error。
   - health/recent error 摘要默认脱敏，不包含 secret、token、session response 或 full packet payload。
   - native go plugin 的网络边界如实表达为 governance/observability，不声明强隔离。
   - 测试：`TestManagerExternalClientOperationsAcceptanceRedactsErrors`、`TestManagerOperationsBackgroundTaskDataQuotaExternalAndGC`。

5. 诊断包可用性
   - diagnostic package 保持可解析 JSON。
   - 顶层包含 plugin state、dispatch summary、recent errors、trace/event/metric summary 和 runbook。
   - 默认排除 full packet payload、secret、token、session response；配置按 schema 脱敏。
   - 测试：`TestManagerOperationsRecordsHandlerMetricsEventsAndDiagnostics`。

6. background task disable
   - 插件 disable 会停止已运行 background task，任务通过 context cancellation 退出。
   - 测试：`TestManagerDisableStopsBackgroundTask`。

## 本阶段命令

- `go test -count=1 ./internal/pluginmanager ./cmd/gateway`
- `go run ./cmd/gateway plugin features`
- `git diff --check`

## 回滚边界

- exporter、event subscriber、diagnostic 和 GC 失败不能影响连接路径。
- event 队列满默认 drop，不阻塞主流程。
- Prometheus/OTel 外部 sink 未作为默认生产依赖启用；如需回滚，只需禁用 exporter runtime 或保持 fact source 中的 `enabled=false` reserved 状态。
