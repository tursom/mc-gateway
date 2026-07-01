# Sandbox Process 运行时生产可用化实施计划

返回：[运行时和未来能力](plugin-development/runtimes-and-future.md)

相关：[Stream、Sandbox、WASM 和 Ingress 完成记录](plugin-roadmap-remaining-work/m08-stream-sandbox-wasm-ingress.md)

## 背景

当前 `sandbox-process` 已经形成 partial data-plane：`SandboxProcessAdapter`、supervisor、control RPC、filesystem staging、network namespace 默认隔离、env allowlist、CPU/memory rlimit、secret handle RPC、diagnostics summary、request/response dispatch、stream relay、治理准入、观测和 conformance release gate 均已接入。它的安全策略偏向 fail-closed：无法强制的网络访问、疑似 secret/token 环境变量、缺失 CPU/memory 限制、缺失 sandbox conformance fixture 都会阻断。

`plugin features` 事实源现在把 `runtime.type=sandbox-process` 和 `service_mode=sandbox-process` 表达为 `partial`、`data_plane=true`。它仍不是“完整无限制 runtime”：只对明确支持并通过 conformance 的 extension point 开放；部署方显式关闭 future gate、环境自检失败、policy 无法强制或 governance/conformance 失败时，Admin、CLI 和 API 仍会展示 blocked/non-active data-plane，并保留回落到 `in-process` 的安全边界。

本文目标是把“sandbox-process 完全可用”拆成可实施、可验收、可回滚的工作项。这里的“完全可用”指：对明确支持的 extension point，sandbox 插件可以通过 Admin/CLI 上传、校验、准入、启用、调用、观测、reload、drain、disable、rollback 和故障隔离的完整生产闭环。

## 当前事实

主要事实源和剩余边界如下：

- `internal/pluginmanager/features.go`：`RuntimeSandbox` 和 `PluginServiceModeSandboxProcess` 默认是 `partial`、`data_plane=true`；显式 gate-off 或 self-check 失败时会降为不可用状态。
- `internal/pluginmanager/future.go`：`ApplyPluginServiceMode` 支持 sandbox active/data-plane mode；失败时保留旧 active data-plane。
- `internal/pluginmanager/runtime_lifecycle.go`：`RuntimeAdapterFactory` 的 `sandbox-process + sandbox-process` adapter 已返回 partial data-plane lifecycle。
- `internal/pluginmanager/sandbox_runtime.go`：进程启动、chroot staging、namespace、rlimit、control RPC、secret handle、diagnostics、hook/provider/subscriber 注册和 stream relay 已接入。
- `internal/pluginmanager/manager.go`：sandbox service mode、artifact metadata、capability、governance、rollback 和 promotion gate 均走 fail-closed。
- `cmd/gateway/plugin_cli_toolchain.go`：CLI 支持 sandbox validate/test/package/preflight/conformance，并要求 strict fixture coverage。
- `plugin/sandboxsdk` 与 `examples/plugins/sandbox-*`：提供 route resolver、rule policy、stream proxy 的可运行样例和 conformance fixture。

这些事实必须保持一致。任何后续能力提升都不能只改 UI 文案或 feature facts 来宣称可用。

## 目标状态

1. `runtime.type=sandbox-process` 可以在受支持 extension point 上被生产启用。
2. `PluginServiceModeSandboxProcess` 有明确启用模型：
   - 支持 desired/active/data-plane 状态。
   - 切换失败能回退到 `in-process`。
   - 是否需要 restart 有一致表达。
3. sandbox 插件通过独立进程承载，插件 crash 不导致 gateway 主进程退出。
4. capabilities 从声明/审计变成强制权限边界；无法强制时继续 fail-closed。
5. 支持 request/response 类 extension point 的真实 RPC 数据面。
6. 支持 protocol-proxy 类场景时必须走 `stream.proxy/v1` 或新版本 extension point，不复用进程内 `net.Conn` 返回语义。
7. Secret、网络、文件、环境变量、CPU/memory、process 行为有可验证的隔离策略。
8. Admin、CLI、API、schema export、文档和 `plugin features` 对 runtime 状态表达一致。

## 非目标

- 不把 `go-plugin-process` 当作 sandbox。
- 不让 sandbox 插件直接返回 gateway 进程内 `net.Conn`。
- 不承诺达到 `in-process go-plugin` 的 hot path 延迟。
- 不支持未声明 capability 的文件、网络、secret 或进程能力。
- 不支持在无法强制权限时降级为“只审计”。
- 不要求第一版支持所有平台；第一版生产路径可以限定 Linux。
- 不把 WASM 生产化作为 sandbox-process 生产化的前置条件。

## 架构决策

### D1：sandbox-process 是独立 service mode

`sandbox-process` 应作为与 `in-process`、`go-plugin-process` 并列的插件服务模式。它不能因为已有 `SandboxProcessAdapter` 就自动混入默认 `in-process` 路径，也不能借用 `go-plugin-process` 的可信 Go 插件语义。

第一版推荐策略：

- 默认仍是 `in-process go-plugin`。
- Admin/CLI 可以保存 `sandbox-process` desired mode。
- gateway 启动或显式 apply 时才尝试进入 sandbox active mode。
- apply 失败必须保留旧 active mode，并记录机器可读 reason。

如果后续要支持“同一 gateway 同时加载 go-plugin 和 sandbox-process artifact”，需要新增 per-artifact runtime routing，而不是只依赖全局 `m.adapter`。

### D2：数据面按 extension point 分层

request/response 类 extension point 走 control/data RPC：

- `route.resolve/v1`
- `rule.evaluate/v1`
- `config.validate/v1`
- `status.ping/v1`
- provider 类轻量查询
- event subscriber 异步投递

长连接或 protocol-proxy 类 extension point 走 stream relay：

- `stream.proxy/v1`
- 或未来 `upstream.connect/v2`

不能把 `upstream.connect/v1` 的 `(net.Conn, error)` 语义强行映射到 sandbox-process。

### D3：能力强制失败必须阻断

manifest 中声明 required capability 后，gateway 必须能证明目标部署可以强制它。不能强制就阻断 preflight/enable/rollback/promotion apply。

第一版可以只支持一组窄 capability，然后逐步扩展：

| Capability | 第一版强制方式 | 不能支持时的行为 |
| --- | --- | --- |
| `filesystem.read` | chroot 或 mount namespace，只读 bind mount | blocking |
| `filesystem.write` | 独立 runtime volume，配额和路径白名单 | blocking |
| `network.none` | 独立 net namespace，无 host network | blocking |
| `network.egress` | egress proxy 或 firewall policy，不开放任意 host network | blocking |
| `env` | 显式 allowlist，禁止 secret/token key | blocking |
| `secret.handle` | gateway Secret RPC，短期 value 或 versioned handle | blocking |
| `cpu.memory` | cgroup v2 或 rlimit，需记录实际 enforcement | blocking |
| `process.restricted` | no new privs、seccomp、pid/user namespace、禁止额外 exec 策略 | blocking |

## 工作分解

### S0：事实源和启用模型

目标：先让 runtime 状态表达和 apply 语义可控，避免“半启用”。

工作：

1. 定义 sandbox production gate，例如 `FutureRuntimeGates.SandboxProcess` 加 deployment-level policy。
2. 更新 `PluginServiceModeFeatureFor` 和 `RuntimeTypeFeature` 的提升条件：
   - gate 关闭：仍是 `reserved/data_plane=false`。
   - gate 打开但未通过环境自检：`partial/data_plane=false`，给出具体 reason。
   - 环境自检通过：`partial/data_plane=true`。
3. `ApplyPluginServiceMode` 支持 sandbox active mode：
   - 校验 adapter factory、Linux/namespace/cgroup 能力、policy profile。
   - 成功后写入 active mode。
   - 失败时保持旧 active mode 并落审计。
4. 明确 `service_mode` 和 artifact `runtime.type` 的关系：
   - 第一版可以要求 sandbox artifact 只能在 sandbox service mode 下启用。
   - 若支持混合模式，必须在 `startRuntimeInstance` 按 artifact runtime 动态选择 adapter，并在 Admin 展示混合数据面。

验收：

- `go run ./cmd/gateway plugin features` 能区分 gate 关闭、环境不满足、可启用三种状态。
- sandbox mode apply 失败不改变当前 active data-plane。
- Admin/API/CLI 对 desired、active、data_plane_mode、restart_required 表达一致。

### S1：manifest、artifact 和 package 校验

目标：sandbox artifact 在上传和准入阶段就能被准确识别和拒绝。

工作：

1. 扩展 manifest runtime metadata：
   - `runtime.type: sandbox-process`
   - `runtime.entry`
   - `runtime.protocol`
   - `runtime.os`
   - `runtime.arch`
   - `runtime.abi_version`
2. `.mcgp` 校验：
   - entry 必须存在。
   - entry 必须是普通可执行文件，不能是 symlink、device、script escape。
   - zip slip、绝对路径、特殊文件继续拒绝。
   - entry sha256、package sha256、runtime metadata 入库。
3. OS/arch/protocol/ABI 校验：
   - 不匹配当前 gateway 部署时阻断 enable。
   - promotion import/apply 也必须重复检查目标环境。
4. CLI toolchain：
   - `plugin validate` 支持 sandbox package。
   - `plugin test --profile manifest` 检查 sandbox runtime metadata。
   - source build 如果产出 sandbox binary，需要记录 builder、target、entry hash。

验收：

- 缺少 entry、entry 不可执行、symlink escape、OS/arch mismatch、ABI mismatch 都阻断。
- upload 不执行插件代码。
- `plugin validate` 和服务端 artifact validation 对同一包给出一致错误码。

### S2：Sandbox Control RPC v1

目标：把当前 health/diagnostics/secret RPC 扩展成完整插件生命周期协议。

工作：

1. 定义协议版本：`mc-gateway-sandbox-process/v1`。
2. 增加命令：
   - `handshake`
   - `init`
   - `register`
   - `reload_config`
   - `invoke`
   - `stream_open`
   - `stream_close`
   - `metrics`
   - `drain`
   - `stop`
3. 所有 request/response 必须有：
   - `protocol`
   - `plugin_id`
   - `artifact_id`
   - `runtime_instance_id`
   - `generation`
   - `trace_id`
   - `deadline`
   - `error_code`
4. `register` 返回 extension point、handler ID、fail policy、timeout、schema version 和 declared capabilities。
5. RPC payload 做 size limit、schema validation 和 redaction。
6. 支持 protocol mismatch、unknown command、timeout、bad JSON、oversized payload 的稳定错误码。

验收：

- sandbox 插件启动后必须完成 handshake/init/register 才能进入 loaded。
- register 失败不污染 dispatch table。
- protocol mismatch 和 ABI mismatch 都是 blocking 且可诊断。

### S3：真实 request/response 数据面

目标：先支持不需要 byte stream 的 extension point，让 sandbox 有可用生产闭环。

工作：

1. 在 `sandboxHostedPlugin.Init` 中按 register 结果向 `Gateway` 注册 handler。
2. 实现 sandbox RPC handler bridge：
   - route resolve。
   - rule evaluate。
   - config validate。
   - status ping。
   - provider 查询。
   - event subscriber 投递。
3. 每次调用都要执行：
   - context deadline。
   - panic/exit/timeout 映射。
   - fail-open/fail-closed 策略。
   - low-cardinality metrics。
   - trace summary。
4. 响应必须结构化，不能让插件返回任意 Go 对象。
5. config validate 应在 enable、dry-run、reload、rollback 前执行。

验收：

- 启用 sandbox route/rule/config 插件后，真实 dispatch 调用 sandbox RPC。
- 插件超时、返回 bad response、进程退出时按 fail policy 处理。
- disable 后新请求不再进入 sandbox handler。

### S4：Stream relay 和 protocol-proxy

目标：支持长连接代理类场景，但不改变 `upstream.connect/v1` 语义。

工作：

1. 基于已有 `stream.proxy/v1` 契约实现 sandbox stream relay：
   - initial bytes replay。
   - 双向复制。
   - half-close。
   - deadline。
   - backpressure。
   - cancel。
   - byte accounting。
2. sandbox control RPC 只负责协商 stream ID 和 relay endpoint。
3. gateway 负责 client connection 与 sandbox stream endpoint 的生命周期绑定。
4. 插件进程 crash、drain、disable 时关闭相关 stream。
5. stream relay 不能泄漏 secret、config 明文或未脱敏 metadata。
6. 支持 drain-only，live migration 不作为第一版要求。

验收：

- protocol-proxy sandbox 插件能通过 stream conformance fixture。
- client close、endpoint close、timeout、backpressure、force close 都有测试。
- 插件 crash 不影响 gateway 进程，已有连接按策略关闭或 fallback。

### S5：隔离和资源强制

目标：把 sandbox capability 做成可证明的运行时边界。

工作：

1. Linux namespace：
   - mount namespace。
   - network namespace。
   - pid namespace。
   - uts/ipc namespace。
   - user namespace或明确说明需要外部容器/权限。
2. 文件系统：
   - readonly root。
   - artifact 只读 mount。
   - runtime writable volume。
   - per-plugin quota。
   - path allowlist。
3. 网络：
   - 默认无 host network。
   - egress 必须通过 proxy/firewall/sidecar。
   - external dependencies 和 capability 声明要一致。
4. 进程权限：
   - no new privs。
   - drop capabilities。
   - seccomp profile。
   - 禁止额外 fork/exec 或受控 allowlist。
5. CPU/memory：
   - 优先 cgroup v2。
   - rlimit 可作为补充，但 diagnostics 必须说明实际 enforcement。
6. 进程树清理：
   - stop/kill 能清理子进程。
   - gateway 异常退出后能 orphan cleanup。

验收：

- 未授权文件读写失败。
- 未授权网络访问失败。
- secret/token env 被拒绝。
- CPU/memory 超限只影响当前 sandbox 插件。
- sandbox 插件不能通过 fork/exec 逃逸策略。

### S6：Secret 和外部依赖访问

目标：sandbox 插件能安全访问授权 secret 和外部服务。

工作：

1. `SandboxSecretResponse` 支持短期 secret value 或短期 token：
   - TTL。
   - version。
   - scope。
   - redaction handle。
2. Secret RPC 必须校验：
   - plugin ID。
   - artifact/generation。
   - manifest declared secret。
   - current config scope。
3. 支持 rotation：
   - reload config。
   - previous version grace period。
   - audit 不记录 value。
4. 外部依赖访问：
   - sandbox 插件不能绕过 egress policy。
   - ExternalClient 或 egress proxy 记录 trace、timeout、fail policy。
   - external dependency health-check 与 runtime status 关联。

验收：

- 未声明 secret handle 请求失败。
- secret value 不进入 env、diagnostics、metrics、audit。
- rotation 后新调用使用新版本，旧调用按 grace policy 收敛。
- 未声明 external dependency 的网络访问被阻断。

### S7：Runtime lifecycle 和 generation 收敛

目标：sandbox runtime 的 enable、reload、disable、rollback 和 restart recovery 可解释。

工作：

1. 引入或复用 runtime instance ID。
2. 每个 runtime instance 绑定：
   - plugin ID。
   - artifact ID。
   - desired generation。
   - config hash。
   - runtime limits hash。
   - capability hash。
3. `Prepare`：
   - 校验 artifact、manifest、policy、环境能力和 protocol。
4. `Start`：
   - 启动进程。
   - 完成 handshake/init/register。
   - 构建不可变 dispatch snapshot。
5. `ReloadConfig`：
   - sandbox RPC 校验新 config。
   - 新调用使用新 config snapshot。
   - 旧调用不被污染。
6. `Drain`：
   - 从 dispatch 移除新调用。
   - 等待或取消 active calls/streams。
7. `Stop`：
   - 停止进程。
   - 清理 runtime dir、socket、cgroup、network namespace。
8. restart recovery：
   - 读取 SQLite desired state。
   - 自动恢复 enabled sandbox 插件。
   - 无法恢复时标记 failed，不影响其它插件。

验收：

- 重启 gateway 后 enabled sandbox 插件按 priority 恢复。
- 旧 generation 的异步回调不能覆盖新 generation 状态。
- reload 失败不影响当前 active artifact/config。
- rollback 重新执行当前 policy 和 capability gate。

### S8：治理、准入和供应链

目标：sandbox 与现有 governance/review/supply-chain/promotion 路径一致。

工作：

1. preflight：
   - sandbox runtime gate。
   - environment enforcement check。
   - required capability check。
   - protocol/ABI check。
   - conformance fixture check。
2. governance：
   - high-risk capability 需要 review。
   - warning override 不能绕过无法强制的 capability。
   - denylist/advisory/revoke 影响 enable、rollback、promotion apply。
3. supply chain：
   - artifact signature。
   - SBOM。
   - license policy。
   - external CI provenance。
   - source/artifact hash。
4. promotion/DR：
   - 目标环境必须支持 sandbox runtime 和对应 capabilities。
   - promotion import 不自动启用。
   - DR drill 要验证 artifact、config、policy 和 runtime gate。

验收：

- 无法强制 required capability 时 preflight/governance/promotion apply 都 blocking。
- override 不能绕过 sandbox enforcement failure。
- 目标环境不支持 sandbox 时 promotion apply 失败且不写 enabled desired state。

### S9：Admin、CLI 和运维观测

目标：管理员能看懂当前 sandbox 状态、故障原因和回滚动作。

工作：

1. Admin runtime panel：
   - desired mode。
   - active mode。
   - data-plane mode。
   - restart required。
   - sandbox environment self-check。
2. Plugin detail：
   - runtime type。
   - runtime instance ID。
   - PID。
   - control socket。
   - cgroup/network namespace。
   - active calls/streams。
   - last error。
   - enforcement status。
3. CLI：
   - `plugin runtime features`
   - `plugin runtime status`
   - `plugin runtime apply`
   - `plugin diagnose`
   - `plugin logs/events/metrics`
4. Operations:
   - sandbox stdout/stderr 摘要脱敏。
   - diagnostics package。
   - crash loop quarantine。
   - GC 清理 runtime temp dir、stale socket、stale cgroup。

验收：

- Admin 不会把 gate 关闭或环境不满足的 sandbox 显示为 active data-plane。
- crash loop、capability block、ABI mismatch、secret denial 都有机器可读 reason code。
- diagnostics 不泄漏 secret/config 明文。

### S10：示例、SDK 和 conformance

目标：插件作者有可运行样例，gateway 有可重复验收。

工作：

1. 新增 sandbox SDK 或最小协议 helper：
   - Go 示例。
   - 至少一个非 Go 示例可以作为后续目标。
2. 示例插件：
   - sandbox rule policy。
   - sandbox route resolver。
   - sandbox stream proxy。
3. Conformance fixture：
   - handshake/init/register。
   - route/rule/config request-response。
   - secret denial。
   - file denial。
   - network denial。
   - CPU/memory exceeded。
   - crash loop。
   - stream half-close/backpressure/cancel。
4. CI smoke：
   - Linux sandbox integration test。
   - CLI validate/build/test。
   - Admin API feature fact snapshot。

验收：

- 示例插件能完成 validate、test、package、preflight、enable、dispatch、disable。
- conformance failure 阻断 prod enable。
- 缺少 fixture 在 strict profile 下 blocking。

## 推荐实施顺序

1. S0 事实源、gate 和 service mode apply 模型。
2. S1 manifest/package/toolchain 校验。
3. S2 control RPC v1。
4. S3 request/response 数据面。
5. S5 最小强制隔离闭环。
6. S6 secret 和 external dependency。
7. S7 lifecycle/generation/restart recovery。
8. S9 Admin/CLI/operations。
9. S10 示例和 conformance。
10. S4 stream relay/protocol-proxy。
11. S8 promotion/supply-chain 的跨环境验收加固。

这个顺序先让低风险结构化扩展点生产可用，再进入长连接 stream。不要先做 protocol-proxy，否则隔离、drain、backpressure 和故障处理会一次性耦合太多风险。

## 最小验收命令

每个阶段至少运行：

```bash
git diff --check
go test ./internal/pluginmanager ./cmd/gateway ./plugin/api
go run ./cmd/gateway plugin features
```

涉及 Admin UI 时增加：

```bash
npm run check:admin
npm run test:admin-ui
```

sandbox 数据面进入可启用状态前，必须增加 Linux 集成测试：

```bash
go test ./internal/pluginmanager -run 'TestSandbox' -count=1
go test ./cmd/gateway -run 'TestAdmin.*Plugin|TestPlugin.*Sandbox|TestCapability' -count=1
```

涉及示例插件时增加：

```bash
go run ./cmd/gateway plugin validate examples/plugins/<sandbox-example>
go run ./cmd/gateway plugin test examples/plugins/<sandbox-example> --profile conformance
```

## Feature facts 提升条件

`sandbox-process` 已按以下条件从 reserved 提升为 partial data-plane；后续若要提升为 implemented，仍需重新审视这些条件和更广泛 extension coverage：

1. `ApplyPluginServiceMode` 能进入 sandbox active mode，并在失败时可回退。
2. 至少一个 request/response extension point 通过真实 sandbox RPC 数据面。
3. required capability 能按白名单强制，不能强制的继续 blocking。
4. secret 不通过 env 注入，并且 Secret RPC 不泄漏 value。
5. sandbox crash 不影响 gateway 主进程。
6. enable、reload、disable、rollback、restart recovery 均有测试。
7. Admin/CLI/API/schema export 的状态表达一致。
8. conformance fixture 能在 prod profile 下作为 release gate。

建议第一阶段提升为：

```json
{
  "type": "sandbox-process",
  "implemented": true,
  "maturity": "partial",
  "data_plane": true,
  "requires_restart": true,
  "unsupported_reason": "sandbox-process supports selected request/response extension points under Linux enforcement; protocol-proxy requires stream.proxy/v1 conformance and is not enabled by default"
}
```

只有 stream relay、protocol-proxy、完整 isolation matrix、跨环境 promotion 和 DR 都通过后，才考虑提升到 `implemented`。

## 回滚边界

- 默认路径必须保持 `in-process go-plugin`。
- sandbox gate 可关闭；关闭后不能影响已有 Go plugin。
- sandbox apply 失败必须保留旧 active mode。
- sandbox 插件 enable 失败不能改变 dispatch table。
- sandbox 插件 crash 后只隔离该插件，不影响 Admin 和其它插件。
- promotion import/apply 不能自动启用目标环境不支持的 sandbox artifact。
- 如果新的 RPC/stream 协议出现兼容问题，只回滚 sandbox runtime，不回滚 plugin manager 主路径。

## 完成定义

`sandbox-process` 完全可用不是一个单点开关。完成必须同时满足：

1. 代码路径真实启用 sandbox 数据面。
2. 运行时隔离可证明。
3. 管理面状态可解释。
4. 失败路径可回滚。
5. 示例和 conformance 可重复。
6. feature facts 不再与实际能力冲突。
