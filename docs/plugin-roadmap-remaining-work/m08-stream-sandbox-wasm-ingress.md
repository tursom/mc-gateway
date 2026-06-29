# M8：Stream、Sandbox、WASM 和 Ingress 完成记录

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

在 `go-plugin-process` drain-only 和 conformance 稳定后，实现真正跨进程 stream、隔离 runtime、低风险 WASM extension point 和 gateway-managed ingress listener。

## 当前结论

M8 已完成为“可验证但默认不暴露 sandbox/WASM/ingress 数据面”的安全闭环：

- `stream.proxy/v1` 是稳定协议契约，覆盖 half-close、deadline、backpressure、cancel、byte accounting，并通过独立 fixture 校验，不依赖内部 UDS relay。
- `sandbox-process` 具备 supervisor、control RPC、filesystem staging、network namespace 默认隔离、env allowlist、CPU/memory rlimit、secret handle RPC 和诊断摘要；默认服务模式仍被 future gate 和 reserved service-mode 阻断。
- sandbox policy 现在 fail-closed：显式启用 network、注入疑似 secret/token env，或绕过默认 CPU/memory limit 归一化后仍缺少资源限制，都会阻断 adapter/start，不再只依赖诊断或审计。
- WASM 使用 wazero 运行 validation containment，具备 host ABI 标识、模块字节缓存、context timeout、memory page limit、默认无文件/网络 host capability，且首批只允许 rule、route、config validate 低风险 extension point。
- WASM timeout、trap、memory exceeded 均只影响当前 validation invocation，后续 `ok` invocation 可继续执行。
- `ingress.service/v1` schema、contract、preflight、governance、port conflict、reserved listener conflict 均可识别；future gate 关闭时不能启用数据面。
- gateway-owned ingress lifecycle 已具备 listener start、TLS/secret ref 脱敏、health snapshot、disable、drain、reserved listener runtime refresh；即使 gate 打开，Start 也会拒绝与 gateway reserved listener 重叠的绑定。
- Admin API 和 CLI feature facts 继续来自共享 fact source：sandbox/WASM/ingress 标记为 reserved/non-data-plane，不宣称默认可用数据面。

## 验收

- required capability 无法强制时阻断启用：`validateArtifactGate` 和 `preflightChecks` 对 sandbox/WASM `runtime.required_capabilities` 返回 blocking error/check。
- WASM timeout、trap、memory exceeded 只影响当前调用：`TestWASMValidationContainment` 覆盖 timeout、panic/trap、memory 后继续执行 `ok`。
- sandbox 插件不能访问未授权 secret、network、files：secret RPC 只返回授权 handle/version，secret 不进入 env；filesystem root escape 被拒绝；network 默认新 namespace 且 `NetworkEnabled` policy 被阻断。
- `ingress.service/v1` 在 gate 关闭时只能 schema/contract/preflight 识别，不能启用：`TestIngressServiceSchemaGateDisabledBlocksEnable` 覆盖 schema valid + disabled block + enable blocked。
- ingress disable 后停止接收新连接并 drain：`TestIngressListenerLifecycleWithGate` 覆盖 drain、disable 和后续 dial 失败。
- gateway reserved listeners 不可被插件 lifecycle 覆盖：`TestIngressListenerLifecycleRejectsReservedListenerConflict` 覆盖 Start 阶段冲突阻断。

## 证据入口

- Stream protocol: `internal/pluginmanager/stream_protocol.go`, `internal/pluginmanager/stream_protocol_test.go`, `examples/plugins/extension-ecosystem/stream-proxy-conformance.json`
- Sandbox runtime: `internal/pluginmanager/sandbox_runtime.go`, `internal/pluginmanager/future_test.go`
- WASM runtime: `internal/pluginmanager/wasm_runtime.go`, `internal/pluginmanager/future_test.go`
- Ingress lifecycle: `internal/pluginmanager/ingress_lifecycle.go`, `internal/pluginmanager/governance.go`, `internal/pluginmanager/future_test.go`
- Feature facts/Admin/CLI: `internal/pluginmanager/features.go`, `internal/pluginmanager/runtime_lifecycle.go`, `cmd/gateway/admin_api_test.go`, `cmd/gateway/plugin_cli_toolchain.go`, `cmd/gateway/plugin_cli_toolchain_test.go`

本阶段不声明以下能力：

- sandbox-process 不是默认 active data-plane；service mode 仍是 reserved，默认回落到 in-process。
- WASM 不是默认 active data-plane；当前只提供 contained validation/runtime adapter scaffolding 和 gate-blocked enable path。
- `ingress.service/v1` 不是默认 active data-plane；schema/preflight/governance 可识别，listener lifecycle 必须显式 future gate 才能调用。

## 回滚边界

- sandbox、WASM、ingress 都必须 feature flag 或 service mode 可关闭。
- 任何 future runtime 故障不得影响 `in-process go-plugin`。

## 最小验证命令

- `go test ./internal/pluginmanager -run 'Test(StreamProxy|Sandbox|WASM|Ingress|PluginServiceMode|RuntimeAdapter|PluginHostCrash|RepositoryPluginNode)' -count=1`
- `go test ./cmd/gateway -run 'TestPlugin|TestAdmin.*Plugin|TestCapability|TestFeatures|TestConfig.*Ingress|Test.*Ingress' -count=1`
- `go test ./internal/pluginmanager ./cmd/gateway`
- `go run ./cmd/gateway plugin features`
- `git diff --check`
