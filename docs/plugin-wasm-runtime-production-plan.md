# WASM 运行时生产可用化实施计划

返回：[运行时和未来能力](plugin-development/runtimes-and-future.md)

## 背景

当前 WASM 能力已经有 `wazero` validation containment、module byte cache、context timeout、memory page limit、默认无文件/网络 host capability，以及低风险 extension point 白名单。它仍然被事实源表达为 `reserved`、`data_plane=false`，原因是现在只完成了受限验证和 runtime adapter scaffolding，还没有形成一条可由 Admin/CLI 正常启用、接入 dispatch、可观测、可回滚的生产数据面路径。

本文目标是把“WASM 运行时完全可用”拆成可实施、可验收、可回滚的工作项。这里的“完全可用”不是支持任意 WASI 程序、任意网络访问或 Minecraft 长连接代理，而是指对明确声明支持的低风险扩展点具备完整生产闭环。

## 目标状态

1. `runtime.type=wasm` 可以在受支持 extension point 上被生产启用。
2. `plugin features`、Admin API、CLI 和文档一致表达 WASM 的真实状态：
   - `implemented=true`
   - `maturity=partial`
   - `data_plane=true`
   - `unsupported_reason` 明确列出不支持 protocol-proxy、任意网络、文件和高风险 extension point。
3. WASM 插件能通过 manifest、preflight、governance、conformance、enable、disable、rollback、status、diagnostics 和 audit 的完整路径。
4. WASM 的 failure containment 可验证：timeout、trap、memory exceeded、bad output、ABI mismatch 只影响当前调用或当前插件，不影响 gateway 主路径。
5. 默认安全边界保持最小权限：无文件、无网络、无环境变量、无 secret value 注入。

## 非目标

- 不支持 `upstream.connect/v1` protocol-proxy。
- 不支持直接返回或持有 Go `net.Conn`。
- 不支持任意 TCP/UDP 网络访问。
- 不支持任意文件系统访问。
- 不支持把 WASM 当作 Go plugin SDK 的 drop-in 替代。
- 不支持 Admin auth、Minecraft auth provider 或 ingress listener 的 WASM 数据面。
- 不承诺通用 WASI 应用兼容；只承诺 mc-gateway 定义的 host ABI。

## 架构决策

### 推荐决策：WASM 不依赖 `sandbox-process` service mode

当前 `WASMAdapter` 挂在 `PluginServiceModeSandboxProcess` 下，但 `sandbox-process` service mode 仍是 reserved/non-data-plane。为了只启用 WASM 而不误宣称 sandbox-process 数据面可用，建议把 WASM 作为独立 contained runtime 接入现有插件服务：

- `PluginServiceModeInProcess + RuntimeWASM`：由 gateway 进程内 `wazero` runtime 承载。
- `RuntimeWASM` 自身提供强默认隔离，不继承 Go plugin 的 native 能力。
- `sandbox-process + RuntimeWASM` 可以保留为未来更强隔离承载方式，但不作为第一版生产启用前置条件。

如果坚持复用 `sandbox-process` service mode，则必须同时把 service mode 事实源拆细到“WASM 子路径可用，sandbox-process binary/container 子路径仍 reserved”，否则 Admin/CLI 容易把 sandbox-process 误读为完整数据面。

## 工作分解

### W0：事实源和启用模型

1. 更新 `RuntimeTypeFeatures()` 中 `RuntimeWASM` 的事实表达：
   - 从 `reserved/data_plane=false` 调整为 `partial/data_plane=true`。
   - `unsupported_reason` 明确列出仅支持低风险 extension point。
2. 更新 `RuntimeAdapterFactory.AdapterFor`：
   - 支持 `PluginServiceModeInProcess + RuntimeWASM` 返回 `WASMAdapter`。
   - 保留 `PluginServiceModeSandboxProcess + RuntimeWASM` 为 future-gated 或 reserved path。
3. 更新 `validateArtifactGate` 和 `preflightChecks`：
   - 不再因为 service mode 不是 `sandbox-process` 阻断 WASM。
   - 继续阻断无法强制的 `runtime.required_capabilities`。
   - 继续阻断高风险 extension point。
4. 确认 Admin runtime panel 和 CLI feature 输出不会把 WASM 说成支持网络、文件或 protocol-proxy。

验收：

- `go run ./cmd/gateway plugin features` 中 WASM 为 `implemented=true`、`maturity=partial`、`data_plane=true`。
- `sandbox-process` service mode 仍不因 WASM 而被误标为完整 data-plane。

### W1：WASM host ABI v1

定义稳定 ABI，而不是只调用 `validate()`：

1. ABI version：`mc-gateway.wasm.host/v1`。
2. Export 命名建议：
   - `mcgw_config_validate_v1`
   - `mcgw_rule_evaluate_v1`
   - `mcgw_route_resolve_v1`
3. 输入输出编码：
   - 第一版使用 JSON bytes，后续可增加 binary schema。
   - host 负责 canonical JSON、size limit 和 schema validation。
4. 必须定义：
   - request schema。
   - response schema。
   - error code。
   - fail-open/fail-closed 策略。
   - timeout、trap、memory exceeded 的标准映射。
5. host import 第一版保持最小：
   - `log(level, message_json)`。
   - `metric(name, labels_json, value)`，必须低基数。
   - 不提供 secret、file、network import。

验收：

- ABI schema 有单测。
- ABI mismatch 会阻断 preflight/enable。
- bad JSON、unknown field、oversized input/output 都有明确错误。

### W2：真实 extension dispatch

当前 `wasmHostedPlugin.Init()` 是 no-op。需要让 WASM runtime 注册真实 hook：

1. `config.validate/v1`
   - enable 前和配置变更时执行。
   - bad config 返回 blocking validation error。
2. `rule.evaluate/v1`
   - 接收 rule context，返回 allow/deny/reason。
   - 支持 fail-open/fail-closed policy。
3. `route.resolve/v1`
   - 接收 host/source/context，返回 route decision。
   - 不允许直接打开网络连接。
4. 每个 hook 都必须：
   - 走统一 timeout。
   - 走 memory/output limit。
   - 记录 low-cardinality metrics。
   - 记录 diagnostics summary。

验收：

- 启用 WASM rule 插件后，`EvaluateRule` 路径真实调用 WASM。
- 启用 WASM route 插件后，route decision 真实来自 WASM。
- disable 后 dispatch 中不再出现该 WASM 插件。

### W3：runtime lifecycle

补齐 `WASMAdapter` 的生产生命周期：

1. `Prepare`
   - 校验 manifest、ABI、exports、limits 和 extension point 白名单。
2. `Start`
   - 编译/缓存 module。
   - 创建 runtime instance。
   - 绑定 artifact generation。
3. `ReloadConfig`
   - 校验新 config。
   - 新调用使用新 config。
   - 正在执行的调用继续使用旧 snapshot。
4. `Drain`
   - 从 dispatch 移除新调用。
   - 等待或取消当前调用。
5. `Stop`
   - 释放 runtime/module instance 引用。
   - 更新 diagnostics。
6. `HealthCheck` 和 `Diagnostics`
   - 显示 ABI、module hash、cache status、last error、trap count、timeout count、memory error count、active calls。

验收：

- reload 不污染正在执行的调用。
- drain 后没有新调用进入旧 instance。
- stop 后 module/runtime 引用释放。

### W4：manifest、package 和 toolchain

1. manifest 增加 WASM 约束：
   - `runtime.type: wasm`
   - `runtime.entry: plugin.wasm`
   - `runtime.abi: mc-gateway.wasm.host/v1`
   - `runtime.limits.handler_timeout_ms`
   - `runtime.limits.memory_bytes`
2. package 校验：
   - `.mcgp` 必须包含 `plugin.wasm`。
   - artifact sha、module sha、ABI metadata 入库。
3. CLI：
   - `plugin validate` 检查 ABI/exports/limits。
   - `plugin test` 能运行 WASM fixture。
   - `plugin conformance` 能执行 WASM rule/route/config validate 场景。
4. 示例：
   - 新增 `examples/plugins/wasm-rule-policy` 或 `examples/plugins/wasm-route-policy`。
   - 示例应包含 source、build 脚本、manifest、conformance fixture。

验收：

- 示例 WASM 插件可 build/package/test/conformance。
- 缺少 `plugin.wasm`、ABI mismatch、缺少 export 均阻断。

### W5：治理和安全策略

1. extension point 白名单：
   - 第一版只允许 `config.validate/v1`、`rule.evaluate/v1`、`route.resolve/v1`。
   - 阻断 `upstream.connect/v1`、`status.ping/v1`、provider、ingress、event subscriber。
2. capability policy：
   - 任意文件、网络、env、secret env capability 均 blocking。
   - 未来 secret 只能通过显式 host ABI handle，并需要单独设计。
3. release gate：
   - WASM artifact 必须通过 ABI validation、conformance、resource limit smoke。
   - prod profile 下缺失 conformance fixture 必须 blocking。
4. supply-chain：
   - source build 或 external CI 路径必须记录 WASM toolchain、target、module sha、SBOM。

验收：

- 高风险 extension point 被 preflight 和 governance 阻断。
- required capability 无法强制时阻断。
- advisory/supply-chain gate 同样作用于 WASM artifact。

### W6：观测、审计和运维

1. Metrics：
   - call count。
   - duration histogram。
   - timeout/trap/memory exceeded count。
   - active calls。
2. Events/audit：
   - enable/reload/disable/rollback。
   - validation failure。
   - ABI mismatch。
   - repeated trap quarantine。
3. Diagnostics package：
   - manifest summary。
   - ABI summary。
   - module hash。
   - recent failures redacted。
4. Admin UI：
   - 显示 runtime type、ABI、limits、supported extension points、last error。

验收：

- diagnostics 中不泄漏 config secret。
- repeated trap 可触发 fail policy 或 quarantine。

### W7：测试矩阵

必须新增或扩展以下测试：

1. Unit
   - ABI schema encode/decode。
   - export detection。
   - memory/time/output limit。
   - low-risk extension whitelist。
2. Runtime
   - config validate OK/fail。
   - rule allow/deny/error。
   - route override/fallback/reject。
   - timeout/trap/memory exceeded 后下一次调用仍可成功。
3. Lifecycle
   - enable。
   - reload config。
   - disable removes dispatch。
   - rollback restores previous artifact/config。
4. Governance
   - high-risk extension blocked。
   - required unsupported capability blocked。
   - missing conformance blocked in prod.
5. CLI/Admin
   - `plugin features` fact source。
   - `plugin test`/`plugin conformance` for WASM sample。
   - Admin API status exposes WASM runtime facts.

最小验证命令：

```sh
git diff --check
go test ./internal/pluginmanager ./cmd/gateway
go run ./cmd/gateway plugin features
go run ./cmd/gateway plugin validate examples/plugins/wasm-rule-policy
go run ./cmd/gateway plugin test examples/plugins/wasm-rule-policy --profile manifest
go run ./cmd/gateway plugin conformance examples/plugins/wasm-rule-policy
```

## 分阶段落地顺序

1. ABI 和 fixture 先行：只定义 schema、exports、错误语义和 conformance fixture。
2. 实现 `config.validate/v1`：最小低风险生产链路。
3. 实现 `rule.evaluate/v1`：接入真实数据面但保持纯计算。
4. 实现 `route.resolve/v1`：接入路由决策，仍不开放网络。
5. 完成 lifecycle、diagnostics、Admin/CLI facts。
6. 打开 `RuntimeWASM` 的 `partial/data_plane=true` 事实表达。
7. 追加 release gate 和生产文档。

每一步都必须保持默认 `go-plugin` 主路径不回归。

## 宣告生产可用前的退出条件

- WASM 示例插件存在并通过 conformance。
- WASM 插件 enable 后能真实影响 rule/route/config validate。
- disable/rollback 能恢复旧行为。
- timeout/trap/memory exceeded 不影响 gateway 和后续调用。
- Admin/CLI/API feature facts 与实际行为一致。
- `sandbox-process` 不被误标为完整生产数据面。
- 文档明确 WASM 第一版只支持低风险 extension point。

## 回滚边界

- 如果 WASM runtime 启用失败，artifact 保持 uploaded/rejected，不改变 active data-plane。
- 如果 WASM 调用失败，按 extension point fail policy 返回 fallback，不影响其它插件。
- disable/rollback 必须从 dispatch 移除 WASM handlers。
- `RuntimeWASM` feature fact 可以回退为 `reserved/non-data-plane`，但不能留下 Admin/CLI 误导文案。
