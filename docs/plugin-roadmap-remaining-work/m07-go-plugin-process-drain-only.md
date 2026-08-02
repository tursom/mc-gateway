> **Archived:** This document records the superseded pre-v2 plugin design. Current behavior is defined by `docs/plugin-development/extension-points.md`.

# M7：go-plugin-process Drain-only 完成记录

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
   - plugin-host crash 只更新 runtime health、diagnostics、service state、node runtime state 和 operation log，不导致 gateway/Admin 主进程退出。
   - disable 后旧 host 进入 drain，连接自然结束或 force close 后 host stop，OS 回收子进程及其 `.so`/Go heap。
4. connection takeover drain-only 验收：
   - disable 先从 dispatch 移除 handler，新连接 pass-through，不再进入旧 plugin-host。
   - 已有 connection takeover 连接继续 drain，自然结束后停止旧 host。
   - force close draining connection 后 active proxy summary 清零，force-close 计数和 host disabled/exited 状态一致。
5. Admin/CLI 状态一致性：
   - Admin API、CLI schema 和 `plugin features` 共享 `PluginServiceState`、`PluginHostRuntimeSummary`、`PluginNodeState`、`PluginHostFeature`。
   - pid、started_at、drain_mode、crash_count、last_error、active_mode、data_plane_mode 字段一致。
   - `go-plugin-process` 继续标注为 `partial`：它支持 trusted Go plugin 进程级加载/回收、drain-only 和 per-node crash isolation，不承诺 sandbox、完整不可信隔离、fd-live 或非 Linux process-table discovery。

## 验收

- `go-plugin-process` 模式下主进程不加载目标 `.so`。
- 子进程 crash 不影响 gateway/Admin 主进程。
- disable 后旧 host drain 并退出。
- `go test ./internal/pluginmanager ./cmd/gateway` 通过，并包含真实子进程 handshake、supervisor start-stop、lifecycle、upstream dialer bridge 和 connection takeover drain-only 测试。

## 证据

- `internal/pluginmanager/process_runtime_test.go`
  - `TestGoPluginProcessDoesNotOpenPluginInGatewayProcess`：构建真实 `.so` 和 gateway 测试二进制，验证 `plugin.Open()` 记录的 PID 是 plugin-host 子进程 PID，不是 gateway/Admin 主进程 PID。
  - `TestGoPluginProcessCrashUpdatesManagerStateWithoutExitingGateway`：启动真实 plugin-host，kill 子进程后验证 host summary failed/isolated/backoff、dispatch 移除、`plugin_service_state.last_error`、`plugin_node_states` 和 plugin runtime failed 状态，同时父进程继续执行。
  - `TestGoPluginProcessAdapterReportsRealHostCrashWithoutExitingGateway`：验证 adapter health/diagnostics 暴露 crash count、last error、drain-only、started/exited/crash timestamps。
  - `TestGoPluginProcessUpstreamDialerBridgeUsesHostProcess`：验证 `legacy upstream-connect contract` dialer 模式经 plugin-host 子进程桥接返回 `net.Conn`，并保持 gateway 主流程后续转发语义。
  - `TestGoPluginProcessProtocolProxyBridgeDrainsAndStopsHost`：验证 connection takeover 真实子进程桥接、disable 后新连接 pass-through、旧连接自然 drain 后 host stopped/exited。
  - `TestGoPluginProcessProtocolProxyForceCloseStopsDrainingHost`：验证 force close draining connection 后 active proxy summary 清零、force-close 计数递增、旧 host disabled/exited。
- `cmd/gateway/plugin_host_test.go`
  - `TestPluginHostHandshakeSubprocess`、`TestPluginHostSupervisorStartsAndStopsHost`、`TestPluginHostSupervisorLifecycleCommands`：覆盖真实子进程 handshake、supervisor start-stop、init/reload/drain/destroy lifecycle。

## 边界

- `go-plugin-process` 是可信 Go plugin 的进程级加载/回收与 drain-only 能力，不是 sandbox。
- 不承诺不可信代码隔离、fd-live migration、sandbox enforcement，也不承诺非 Linux process-table orphan discovery。

## 回滚边界

- runtime mode 切换失败时回到 `in-process`。
- `go-plugin-process` 可通过 service mode 禁用，不影响默认路径。
