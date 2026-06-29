# M5：Operations 生产验收未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

让运维能从 Admin/API/CLI 排查插件故障、清理资源和解释多实例状态，而不是只看到内部模型。

## 未完成工作

1. 补外部 exporter 边界：
   - Prometheus/OTel exporter 的启用、失败、降级和回滚策略。
   - exporter 失败不能影响连接路径。
   - label 低基数和敏感字段拒绝策略需要在 exporter 输出侧继续验证。
2. 统一跨类别长期保留策略：
   - diagnostic package、event、metric、trace、background task、PluginDataStore 和 PluginFileStore 的 retention 规则需要可解释。
   - GC dry-run 必须展示候选对象、大小、原因和 protected 状态。
   - apply 必须写审计。
3. 补多实例运维验收：
   - gateway node heartbeat、plugin node runtime state、partial rollout、background task lease 和 GC 行为在多节点下可重复验证。
   - singleton lease renew、lease lost cancellation 和 sharded task 分配需要跨节点场景。
4. 补 ExternalClient 验收：
   - timeout、retry、fail policy、circuit breaker、health status 和 recent error。
   - 错误摘要不包含 secret、token、session response 或完整 payload。
   - native plugin 下的网络边界限制必须如实表达为治理/观测能力，而不是强隔离。
5. 补诊断包可用性验收：
   - 诊断包保持可解析 JSON。
   - 包含 plugin state、dispatch summary、recent errors、trace/event/metric summary 和 runbook section。
   - 默认不包含完整 packet payload、secret 或 token。

## 验收

- 插件 disable 后 background task 停止。
- GC dry-run/apply 能清理过期 data/file、diagnostic package 和 orphan runtime file，并写审计。
- diagnostic package 脱敏检查通过，未过期包受保护。
- `go run ./cmd/gateway plugin features` 的 operations 段能看到已启用的真实能力和仍未启用的 exporter 边界。
- `go test ./internal/pluginmanager ./cmd/gateway` 通过。

## 回滚边界

- exporter、event subscriber、diagnostic 和 GC 失败不能影响连接路径。
- event 队列满默认 drop，不阻塞主流程。
