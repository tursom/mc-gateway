# M7：go-plugin-process Drain-only 未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

让可信 Go plugin 支持进程级卸载、资源回收和 crash 边界。这个阶段不提供 sandbox，也不承诺不可信代码隔离。

## 完成状态

1. 跨节点 crash policy 协调：
   - `plugin_service_state` 持久化 crash backoff/max/window，所有节点读取同一份 crash policy。
   - crash count、backoff、auto-isolation 和 service-level `last_error` 会进入 host summary、service state 和 operation log。
   - partial rollout 使用 `plugin_node_states` 聚合 per-node truth。某个节点 crash-loop 只会把该节点标为 failed；其他仍持有 healthy loaded runtime 的节点继续记录自己的 enabled/applied generation，不会被全局 plugin row 误导。
2. 跨平台进程表 orphan discovery：
   - fallback 顺序固定为 metadata-backed sweep、stale control socket handshake、supervisor-owned Stop/Kill cleanup、Linux `/proc` process-table discovery。
   - Linux 以外平台明确 unsupported reason：process-table orphan discovery requires Linux `/proc`，并退回 metadata、control socket handshake 和 supervisor cleanup。
3. 进程态回归测试：
   - `go-plugin-process` adapter 主进程只启动/控制 plugin-host，`.so` 由 `gateway plugin-host serve` 子进程加载。
   - 测试用插件 init 写入 PID，验证 `plugin.Open()` 发生在 host PID 而不是 gateway/Admin 主进程 PID。
   - plugin-host crash 只更新 runtime health/diagnostics，不导致 gateway/Admin 主进程退出。
   - disable 后旧 host 进入 drain，连接自然结束或 force close 后 host stop，OS 回收子进程及其 `.so`/Go heap。
4. protocol-proxy drain-only 验收：
   - disable 先从 dispatch 移除 handler，新连接 pass-through，不再进入旧 plugin-host。
   - 已有 protocol-proxy 连接继续 drain，自然结束后停止旧 host。
   - force close draining connection 后 active proxy summary 清零，force-close 计数和 host disabled/exited 状态一致。
5. Admin/CLI 状态一致性：
   - Admin API、CLI schema 和 `plugin features` 共享 `PluginServiceState`、`PluginHostRuntimeSummary`、`PluginNodeState`、`PluginHostFeature`。
   - pid、started_at、drain_mode、crash_count、last_error、active_mode、data_plane_mode 字段一致。
   - `go-plugin-process` 继续标注为 `partial`：它支持 trusted Go plugin 进程级加载/回收、drain-only 和 per-node crash isolation，不承诺 sandbox、完整不可信隔离、fd-live 或非 Linux process-table discovery。

## 验收

- `go-plugin-process` 模式下主进程不加载目标 `.so`。
- 子进程 crash 不影响 gateway/Admin 主进程。
- disable 后旧 host drain 并退出。
- `go test ./internal/pluginmanager ./cmd/gateway` 通过，并包含真实子进程 handshake、supervisor start-stop、lifecycle、upstream dialer bridge 和 protocol-proxy drain-only 测试。

## 回滚边界

- runtime mode 切换失败时回到 `in-process`。
- `go-plugin-process` 可通过 service mode 禁用，不影响默认路径。
