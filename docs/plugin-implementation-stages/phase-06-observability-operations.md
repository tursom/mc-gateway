# 阶段 6：Observability And Operations

## 目标

补齐生产运行所需的观测和运维能力：metrics、业务事件、trace、日志、诊断包、后台任务、plugin_data、PluginFileStore、外部依赖治理、GC 和 Runbook。

阶段结束后，插件出问题时管理员能定位、降级、清理和恢复，而不是只能查看 gateway 日志。

## 可用性检查点

阶段结束时必须能做到：

- 管理员能看到 handler calls、duration、panic、timeout、active proxy connections。
- 插件可以上报脱敏业务事件和自定义指标。
- trace 能关联 connection、plugin handler、external dependency、backend dial。
- 插件能注册 interval/manual background task。
- 插件能使用 PluginDataStore 和 PluginFileStore，并受配额限制。
- 管理员能执行 plugin_data/file/log/artifact GC dry-run 和清理。

## 范围

### Metrics

实现：

- plugin handler calls。
- duration histogram。
- errors/panic/timeout。
- active calls。
- active proxy connections。
- build duration/failures。
- external dependency requests/duration/inflight/circuit state。
- event delivery queue/drop/dead letter。

### Events 和 custom metrics

- 插件 manifest 声明 event schema。
- `EmitEvent` 接收低基数字段。
- 未声明或高基数字段拒绝或 drop。
- 最近事件摘要保留。
- custom metric schema 和低基数 label 限制。

### Tracing

- gateway 生成 connection ID 和 trace ID。
- SDK 通过 context 传递。
- 日志带 trace/connection ID。
- trace 摘要脱敏。
- 默认不向第三方依赖注入 `traceparent`，除非策略允许。

### Background Task

- interval/manual。
- run-on-start。
- jitter。
- timeout。
- non-reentrant。
- manual trigger 权限和 confirm token。
- last/next run、skipped、consecutive failures。

### Data 和 files

PluginDataStore：

- schema version。
- data class。
- quota。
- retention。
- exportable 标记。
- GC。

PluginFileStore：

- resources readonly。
- runtime data/cache/tmp/log/diagnostic。
- path traversal 防护。
- quota。
- retention。
- orphaned dir 检测。

### External dependencies

- endpoint、purpose、required、timeout、retry、fail policy。
- `ExternalClient` 受控 HTTP/TCP 调用。
- health check。
- circuit breaker。
- data classes。
- 最近错误摘要。

## 明确不做

- 不承诺 native plugin 无法绕过 ExternalClient。
- 不默认开启 Prometheus/OTel exporter 的完整外部集成。
- 不保存完整 packet payload、secret、token、session response。

## 实现任务

1. 实现 metrics 内部模型和 Admin API。
2. 实现 business event/custom metric SDK。
3. 实现 trace summary。
4. 实现 plugin logger 和日志摘要。
5. 实现 background task 注册和状态。
6. 实现 PluginDataStore。
7. 实现 PluginFileStore。
8. 实现 ExternalClient。
9. 实现 diagnostic package。
10. 实现 GC APIs 和 Runbook。

## 验收

- mc-auth-proxy 示例能上报 `auth.success`/`auth.failure` 摘要。
- session server 调用通过 ExternalClient 记录 latency 和错误。
- background task 超时不会阻塞连接路径。
- plugin_data 超配额时写入失败且不会无限增长 SQLite。
- 诊断包不包含 secret 明文和完整 packet。
- GC dry-run 能展示将清理的对象和大小。

## 回滚策略

- exporter 失败不能影响连接路径。
- event 队列满默认 drop，不阻塞主流程。
- background task 可取消；disable 插件时任务停止。
- plugin_data/file GC 先 dry-run，清理操作写审计。
