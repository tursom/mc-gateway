# 插件系统 Roadmap 优先级审计

本文的目标是给插件系统做完整 roadmap 审计，而不是只判断某一个 runtime 是否完成。runtime 是插件系统的基础能力之一，但完整 roadmap 还包括包格式、构建、Admin 管理闭环、治理门禁、观测运维、扩展生态、仓库分发、供应链、promotion、多实例和灾备。

本次审计按三类问题组织：

1. 阶段目标里明确要求了什么。
2. 当前代码真实实现了什么，哪些只是模型或预留。
3. 后续 roadmap 应该按什么顺序推进。

## 结论修正

之前把 runtime 当成唯一主线是不准确的。更合理的判断是：

- **现有主路径已经相当完整**：`go-plugin + in-process`、`.mcgp` 管理、protocol-proxy、source builder、本地 Admin/CLI、配置/secret/rollback、governance、operations 和部分 extension ecosystem 都已经有真实代码。
- **runtime 仍是重要缺口**，但它不是全部缺口。`go-plugin-process`、sandbox、WASM、跨进程 stream 等属于阶段 8 的未来能力，当前大多是 schema、状态模型或 stub。
- **当前最需要的 roadmap 不是继续堆功能名**，而是把“已可生产使用”、“可用但需加固”、“模型/预留”、“完全未做”分清楚，并为每个阶段补上验收证据。
- **阶段 1-7 的完成度不应被阶段 8 绑架**。如果短期目标是让当前插件系统可上线，优先级应该先压实当前 `in-process go-plugin` 主路径、构建/治理/运维/扩展点的验收；如果目标是解决热卸载、进程隔离或跨语言，再进入 runtime 扩展路线。

## 成熟度标记

| 标记 | 含义 |
| --- | --- |
| 已落地 | 当前代码已经改变真实数据面或管理面，能通过 Admin/CLI/API 调用 |
| 可用但需加固 | 有真实实现，但生产边界、验收、UI 表达或失败路径仍不足 |
| 模型/预留 | 有 schema、状态、API 或测试模型，但没有真实 runtime/data-plane 行为 |
| 未实现 | 阶段文档或设计提到，但当前代码只有 reserved 命令、stub 或没有入口 |

## 当前实现总览

| 能力域 | 当前成熟度 | 代码证据 | 主要缺口 |
| --- | --- | --- | --- |
| `.mcgp` binary artifact | 已落地 | `ArtifactStore.ValidateAndStore` 校验 zip、manifest、runtime entry；`Manager.UploadArtifact` 保存 artifact。见 `internal/pluginmanager/artifact.go:57-260`、`internal/pluginmanager/manager.go:368-389` | 需要用端到端 fixture 证明上传、load、enable、disable、重启恢复 |
| `in-process go-plugin` runtime | 已落地 | `RuntimeAdapterLifecycle` 已定义 Validate/Prepare/Start/Health/Reload/Drain/Stop/Diagnostics；`GoPluginAdapter` 通过 lifecycle 路径调用 `plugin.Open()` 和 `Lookup("Plugin")` 实例化插件。见 `internal/pluginmanager/runtime_lifecycle.go`、`internal/pluginmanager/manager.go` | 不能热卸载，插件崩溃仍在主进程边界内，只能靠 recover/timeout 降风险 |
| desired/runtime state 和 dispatch | 已落地 | `SetDesired`、`Load`、`Enable`、`Disable`、`Delete` 推动状态和只读分发快照。见 `internal/pluginmanager/manager.go:593-930` | 需要持续验证失败不污染旧 dispatch table |
| `upstream.connect/v1` dialer mode | 已落地 | `ConnectUpstream` 调用 handler，dialer mode 返回插件提供的 `net.Conn`。见 `internal/pluginmanager/manager.go:993-1070` | 需要保持和 legacy hook 的兼容测试 |
| protocol-proxy mode | 已落地但需加固 | initial data replay、双向 copy、copy-loop panic recovery、active proxy tracking、drain/force close 已有；CLI conformance 的 `conformance.json` 已能声明 protocol-proxy golden scenarios；`protocol/smoke` 和 manager/example 测试已覆盖真实 MC handshake/login/payload backpressure fixture。见 `internal/pluginmanager/manager.go:1073-1105`、`1875-1947`、`1477-1491`、`cmd/gateway/plugin_cli_toolchain.go`、`protocol/smoke` | 仍需要更完整真实 MC smoke fixture、异常路径和跨版本示例验收 |
| source `.mcgp` 和 builder | 可用但需加固 | source 上传创建 build；local-process 和 container builder 都能产出 binary artifact，记录 source/artifact sha、module/provenance、Go version、ABI fingerprint；prod governance 会阻断 local-process、warning 缺失 builder digest、浮动 builder image 或未绑定 plugin API/Go release 的 builder image，并把 pinning/API/Go/平台匹配写入 supply-chain assessment；external CI assessment 已要求签名验证、source/artifact sha、run/builder identity 和 trusted 标记；GC 会保护 queued/running build 的 source package 并可清空 completed build log。见 `internal/pluginmanager/manager.go:441-631`、`internal/pluginmanager/builder.go`、`internal/pluginmanager/governance.go`、`internal/pluginmanager/gc.go` | 仍需官方 release-pinned builder image 发布、完整 CI artifact 发布链和跨环境验收 |
| container builder | 可用但需加固 | `ContainerBuilder.Build()` 通过 `docker run --rm` 只读挂载 source、输出目录并执行 `go build -mod=readonly -buildmode=plugin`，同时记录 builder image digest；prod governance 会要求浮动 image tag 或未绑定 plugin API/Go release 的 image 走 warning override。见 `internal/pluginmanager/builder.go:128-244`、`internal/pluginmanager/governance.go` | 需要官方 builder image 发布和 CI 环境验收 |
| Admin UI 管理闭环 | 已落地但需加固 | 插件列表、详情、上传、配置、secret、rollback、governance、operations、plugin-service 面板已有；runtime service mode 面板已分开展示 desired/active/effective data-plane、adapter、desired/active support、restart/pending 状态和 crash policy；governance 面板已展示 policy strict fixture gate。见 `cmd/gateway/admin_frontend/src/views/plugins.ts` | 仍需补更多 UI 自动化验收和高风险能力失败路径 |
| 配置、secret、rollback | 已落地 | `DryRunConfig` 做 JSON/schema/secret/runtime dry-run，artifact/config rollback 重新走 governance 和 dry-run。见 `internal/pluginmanager/manager.go:621-792` | secret 仍是本地最小 SecretStore，不是外部 KMS |
| governance/release gates | 可用但需加固 | review、warning override、preflight、self-test、benchmark、advisory、本地漏洞库、外部 feed sync/scheduler、conflict、supply-chain issue 接入 enable/rollback；repository import 保存同一治理路径的 admission preview，repository import apply 会重新 dry-run/governance 后只写 disabled desired state；CLI conformance 会用真实 config schema 和 required secret 声明判断 `invalid_config`/`missing_secret` 负向 fixture，并已支持 governance gate golden scenarios；packaged conformance fixture 失败会进入默认 preflight/governance blocking gate；`gateway plugin preflight --require-conformance-fixture` 和服务端 `MC_GATEWAY_PLUGIN_REQUIRE_CONFORMANCE_FIXTURE=true` 已能把缺失 packaged fixture 升级为 `conformance_fixture_missing` blocking。见 `internal/pluginmanager/governance.go`、`internal/pluginmanager/future.go`、`cmd/gateway/plugin_cli_toolchain.go` | 缺失 conformance fixture 默认仍保持兼容不阻断；完整外部 CVE/SBOM 自动扫描链仍未落地 |
| observability/operations | 可用但需加固 | metrics/event queue、trace、background task、data/file store、external runtime、diagnostic/GC 模型；event 低基数字段、custom metric label gate 和 handler trace summary 已有验收，background task 已有 SQLite-backed `per_node`/`singleton`/`sharded` lease 和 retry/cancel 验收，diagnostic package 已有结构化脱敏、runbook section 和默认 7 天 retention GC 验收，PluginDataStore/FileStore 已覆盖 quota、retention、GC dry-run/apply 和 runtime orphan file cleanup，Plugin Service 已有 gateway node heartbeat，插件详情已有 node runtime state 和 partial rollout 展示。见 `internal/pluginmanager/operations.go:1-220` | 外部 exporter、跨类别长期保留策略和更细的多实例验收仍需补 |
| extension ecosystem | 部分已落地 | route/status/middleware/subscriber/provider 注册、timeout/recover/summary 有代码；middleware 已有 connection/handshake ordering、rewrite 传递和 fail-open/fail-closed 验收；event subscriber 已有 at-least-once retry、dead-letter replay/drop 验收；官方 rule/policy 和 extension ecosystem 示例已覆盖常用 route/status/event/provider 场景。见 `internal/pluginmanager/extensions.go:1-220`、`examples/plugins/extension-ecosystem` | 每个 extension point 的独立执行型 conformance、示例和冲突治理还不完整 |
| official rule/policy | 已落地 | `GoPluginAdapter` 对 `official.rule-policy` 走 builtin 特例；官方插件覆盖维护模式、host/upstream rewrite、source CIDR allow/deny、简单限流，以及 status MOTD/favicon/online/max/version/window。见 `internal/pluginmanager/manager.go:85-89`、`plugin/official/rulepolicy` | 这是官方内置插件，不代表通用 `builtin` runtime |
| CLI/toolchain | 可用但需保持边界清晰 | `features`、`init/build/test/preflight/self-test/...`、`contract`、`conformance`、`schema export`、`sign`、promotion dry-run 等命令已有实现，`plugin features` 的 `reserved_commands` 当前为空；feature matrix 明确 promotion 支持 `cross_node_apply_mode=cli_admin_to_admin`，但 repository/operations 仍 `cross_node_apply=false`，extension point matrix 会把 `admin.auth.provider/v1` 和 `ingress.service/v1` 标为 `reserved`/`data_plane=false`，并通过 `ingress` fact block 标出 schema/preflight/conflict gates 已有但 listener lifecycle/data-plane 仍未实现，通过 `sandbox`/`wasm` fact block 标出 capability/contained-validation gates 已有但 supervisor、enforcement、WASM adapter/ABI/cache/fuel 等仍未实现；instrumentation 有 metadata gate 和 gateway/CI artifact digest binding，`conformance.default_release_gate=true`、`package_fixture_failures_block_preflight=true`、`missing_fixture_required=false`、`missing_fixture_policy_gate=true`，strict preflight 可用 `--require-conformance-fixture`；`schema export --section cli` 与 `plugin features.cli.implemented_commands` 共用命令事实源，`schema export --section admin-api` 已给 plugin service state/crash policy/runtime adapter/host/node response 输出 JSON Schema `$defs`，`schema export --section conformance-fixture` 已输出 `conformance.json` 契约。见 `cmd/gateway/plugin_cli_toolchain.go` | CLI 输出需要持续作为 roadmap 事实源，不能让 dry-run 或 reserved backend 能力看起来已经接管数据面 |
| `go-plugin-process` | 部分实现 | service mode、desired/active、host summary、runtime adapter factory、plugin-host 子进程、UDS control、supervisor start/stop、host 内 lifecycle、loaded host crash summary refresh、unexpected clean exit crash classification、service-level last error persistence、persisted configurable crash backoff/window policy、crash-loop auto-isolation、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket handshake orphan cleanup、Linux `/proc` process-table orphan discovery、`upstream.connect/v1` dialer bridge 和 protocol-proxy drain-only stream bridge 已有。见 `internal/pluginmanager/future.go`、`internal/pluginmanager/runtime_lifecycle.go`、`internal/pluginmanager/process_runtime.go` | 跨节点 crash policy 协调、跨平台进程表 orphan discovery 和完整迁移仍未实现 |
| `sandbox-process` | 模型/预留 | runtime/service mode gate 会阻断未启用或无法强制 capability 的 artifact。见 `internal/pluginmanager/manager.go:1593-1627`、`internal/pluginmanager/governance.go:507-519` | 没有 sandbox supervisor、control RPC、OS/container enforcement |
| WASM | 模型/预留 | `WASMRunner` 只是按 behavior 字符串模拟 panic/timeout/memory，并把 validation 限定在 rule/route/config validate 等低风险 extension point。见 `internal/pluginmanager/future.go:188-233` | 没有 wazero/wasmtime loader、host ABI、fuel/memory enforcement |
| `ingress.service/v1` | 模型/预留 | extension point schema 已进入 CLI/features/manifest contract；capability schema validation 已覆盖 `protocol`、`bind`、`port`、TLS secret ref 和 health check 基础字段，preflight/governance 会报告 `ingress_service_schema_valid` 或 `ingress_service_invalid`；governance 已对已启用 ingress 插件声明做 plugin-vs-plugin `ingress_port_conflict` 检查，并会用启动时注入的 TCP/Admin、KCP、QUIC、WebSocket reservation 输出 `ingress_reserved_listener_conflict`；同时继续以 `ingress_service_reserved` 阻断启用 | 没有 gateway-managed listener、TLS/secret 装载、health/drain data-plane；内置服务 reservation 是启动快照，服务运行期变更后需要重启或刷新 manager 才会进入该治理检查 |
| repository/supply chain | 部分模型 | file/url repository import 可把候选 artifact 导入本地 store，不自动创建 desired state，并把 policy hash、risk、advisory/supply-chain 等 admission preview 写入导入记录；本地 repository import apply 可在目标侧 config dry-run 和 governance 通过后写入 disabled desired state；签名 trust store、SBOM 生成、license policy、external CI provenance gate、本地/外部 advisory feed、本地/导入式/外部 vulnerability feed、本地 artifact package mirror 和 Admin-to-Admin 手动 artifact package transfer 已接入治理/运维表达。见 `internal/pluginmanager/future.go`、`internal/pluginmanager/governance.go` | cross-node apply、official/internal repository 同步和完整外部 CVE/SBOM/CI artifact 发布链未完整实现 |
| build-time instrumentation | 模型/预留 | 保存 instrumentation metadata 和 rollback runbook；`available` 状态必须携带 generated diff hash、gateway binary digest、CI artifact digest、passing conformance、benchmark 和 smoke evidence；Admin instrumentation 列表展示 digest binding 和 conformance/benchmark/smoke 证据状态。见 `internal/pluginmanager/future.go`、`cmd/gateway/admin_plugin_handlers.go`、`cmd/gateway/admin_frontend/src/views/plugins.ts` | 没有真实 CI 插桩产物生成和 gateway binary 发布链 |
| promotion/multi-node/DR | 部分模型 | 本地 promotion export/import/diff/drift/dr-drill、target desired-state apply、secret ref target mapping warning、gateway node heartbeat、插件维度 node runtime state、local artifact package mirror、Admin-to-Admin 手动 artifact package transfer 和 partial rollout 展示已有；CLI `plugin apply <bundle> --target-gateway ...` 已能把 bundle artifact package 从源网关传到目标网关后调用目标侧 promotion apply；apply 要求目标侧 config hash 匹配、本地 artifact 存在、dry-run/governance 通过，并且不自动启用 active 流量；Admin 目标侧 DR drill 会验证本地 artifact、config hash、dry-run 和 governance，但不写 desired state。见 `cmd/gateway/plugin_cli_toolchain.go`、`cmd/gateway/plugin_cli_remote.go`、`internal/pluginmanager/future.go` | repository apply 跨节点编排、自动集群级 apply 和自动分发仍未完成 |

## 阶段目标和当前状态

| 阶段 | 阶段目标是否明确 | 当前状态判断 | Roadmap 判断 |
| --- | --- | --- | --- |
| 阶段 1：Managed Binary Plugin MVP | 明确要求 binary `.mcgp`、Plugin Manager、desired/runtime state、`upstream.connect/v1` dialer、Admin API/CLI、示例 | 大部分已落地 | 应进入验收加固：端到端 fixture、重启恢复、失败路径、dispatch 不回归 |
| 阶段 2：Protocol Proxy MVP | 明确要求 protocol-proxy、initial data replay、drain/force close、MC capability、`mc-auth-proxy` 示例 | 核心数据面已落地 | 下一步不是再扩展 MC core，而是补 smoke fixture、异常路径和示例验收 |
| 阶段 3：Source Package Builder | 明确要求 source `.mcgp`、builder、provenance、build log、GC；生产推荐 container builder | local-process/container builder 已落地，prod 默认 container，local-process source-built artifact 在治理门禁阻断，provenance 已进入 supply-chain assessment metadata，GC 已覆盖 in-flight source 保护和 completed build log 清理 | 接下来应补 release-pinned builder image、external CI 信任和跨环境验收证据 |
| 阶段 4：Admin UI、配置、Secret、回滚 | 明确要求 UI 管理闭环、schema/dry-run、secret ref、artifact/config rollback | UI/API/管理闭环已落地 | 需要做 UI truthfulness，尤其 runtime service mode 不能暗示未实现能力可用 |
| 阶段 5：Governance And Release Gates | 明确要求 review、risk、conflict、preflight/self-test、benchmark、advisory | 主体已落地 | 需要把治理从“有模型”推进到“release gate 证据”：conformance、fixtures、策略快照验收 |
| 阶段 6：Observability And Operations | 明确要求 metrics/events/trace/logger/diagnostic/background task/data/file/external/GC | 内部模型和 API 已落地较多 | 需要补生产验收：脱敏诊断、队列/GC/配额、外部依赖、任务超时和多实例边界 |
| 阶段 7：Extension Ecosystem | 明确要求 route/status/middleware/provider/event/rule/Admin auth provider 和官方示例 | 多数 extension skeleton 已落地，官方 rule/policy 已有；`auth.provider/v1` 当前只是 provider registration/status，未接入 Minecraft 登录数据面 | 应按常见使用价值收尾，而不是把所有 extension point 同等优先 |
| 阶段 8：Future Runtimes And Distribution | 明确要求 `go-plugin-process`、sandbox/WASM、ingress、repo/sign/SBOM/license、instrumentation | `go-plugin-process` 部分落地，其余大多是模型、gate 或 reserved | 这是未来路线，不应反向否定阶段 1-7，但需要如实标注 partial/reserved |

## 哪些功能是阶段目标里明确标明的

这些属于阶段文档已经明确要求的功能，不是额外发散：

- 阶段 1：binary `.mcgp`、manifest 静态校验、artifact 登记、Plugin Manager、desired/runtime state、dispatch table、`upstream.connect/v1` dialer mode、load/enable/disable/delete、基础审计、`upstream-rewrite` 示例。
- 阶段 2：protocol-proxy mode、initial data replay、双向 copy、active proxy connection、draining/force close、MC capability manifest、`mc-auth-proxy` 示例和 protocol smoke helper。
- 阶段 3：source `.mcgp`、build job、local-process/container builder、build provenance、module summary、build log、source/build/artifact GC。
- 阶段 4：Admin UI、配置 schema、runtime dry-run、sensitive diff、SecretStore、secret version、artifact rollback、config snapshot rollback、权限。
- 阶段 5：policy profile、review、warning override、conflict analysis、preflight/self-test、benchmark gate、denylist/quarantine/revoke/advisory。
- 阶段 6：metrics、events、custom metrics、trace、plugin logger、diagnostic package、background task、PluginDataStore、PluginFileStore、ExternalClient、GC。
- 阶段 7：route resolver/provider、status ping、middleware、provider registry、event subscriber、official rule/policy、Admin auth provider 预留或实现。
- 阶段 8：`go-plugin-process`、sandbox-process、WASM、`ingress.service/v1`、repository、signature、SBOM、license policy、advisory feed、build-time instrumentation。

## 阶段目标没有充分展开但实际需要补的任务

这些不是凭空新增功能，而是为了让阶段目标真正可验收所需的隐含任务：

### 1. Roadmap 事实源和验收口径

- `plugin features` 应成为机器可读事实源，明确区分 implemented、partial、reserved、stub。
- Admin UI 和 CLI 应统一展示“当前真实数据面模式”和“配置/预留模式”。
- 每个阶段需要 phase exit checklist：命令、测试、示例、失败路径、回滚路径。
- reserved CLI 命令必须在文档和 UI 中被标注为未实现，避免误导。

### 2. Conformance 和 contract

- `contract`、`conformance`、`schema export` 已不再停留在 reserved，`conformance.json` 已支持显式 golden fixture 文件。
- 每个 extension point 仍需要真实执行型 golden fixture：输入、输出、错误码、timeout、panic、fail policy。
- protocol-proxy 已有 `initial_data_once`、`panic_recovered`、`timeout_deadline`、`endpoint_close`、`client_close`、`backpressure_large_packet`、`drain_disable_new_connections`、`force_close_draining` 场景声明；真实 MC packet/backpressure fixture 已由 `protocol/smoke`、manager 测试和 `mc-auth-proxy` 示例测试覆盖，后续还需要更多异常路径和跨版本 smoke。
- governance 已有 review、warning override、advisory、supply-chain、rollback、repository apply、promotion apply 场景声明；packaged conformance failure 已接入默认 release gate，缺失 fixture 已有 strict preflight/Manager policy gate，但默认仍兼容不阻断，后续还需要逐步切默认和补真实执行型报告。

### 3. Source build 的生产边界

- container builder 已有真实实现，但还需要发布级 builder image 绑定、digest 策略和 CI 环境验收。
- 构建环境需要证明不泄露 GOPRIVATE/token/secret，不执行包内脚本，不污染 gateway 主进程。
- provenance 已接入 prod governance/preflight 和 supply-chain assessment metadata，后续还要接入 external CI 签名/SBOM 自动扫描链。

### 4. Admin/UI 的真实性

- plugin service mode 面板已展示 `go-plugin-process` 当前支持 partial process data-plane，`sandbox-process` 当前未实现数据面。
- `active_mode`、`desired_mode`、`restart_required`、`implemented_adapter`、`data_plane_mode`、desired/active support 和 crash policy 已分开；desired 与 effective data-plane 不一致时会显示 pending/alert。
- 对 source build、supply-chain、repository、runtime mode 这类高风险能力，UI 应显示“可用/部分/预留”的状态。

### 5. 生产运维边界

- background task 已支持 `task cancel`、manifest `retry`、本地 SQLite lease、singleton lease renew 和 lease lost cancellation 验收，Plugin Service 已支持 gateway node heartbeat，插件详情已展示 node runtime state/partial rollout；仍要补跨类别长期保留和更细的多实例验收。
- metrics/events/trace 已覆盖低基数和脱敏验收：event 字段有低基数限制，custom metric label 必须在 manifest 中声明且拒绝敏感/超长值，handler trace summary 和 custom metric snapshot 会进入 operations/diagnostic 视图。
- diagnostic package 已有结构化脱敏、runbook 和 retention GC 验收：plugin config 按 schema 脱敏，recent operation metadata 单独脱敏，输出保持可解析 JSON，不能包含 packet payload、secret、token、session response，并包含 preflight/disable/rollback/external/GC 操作提示；未过期包 protected，默认 7 天后可由 operations GC 删除文件和仓库记录。
- PluginDataStore/FileStore 已覆盖配额、retention、GC dry-run/apply 和 runtime orphan file cleanup 的端到端验证。
- ExternalClient 在 native plugin 下不是强制网络边界，文档和 governance 必须如实表达。

### 6. Runtime 扩展前置条件

- runtime adapter lifecycle/factory 已落地 Validate/Prepare/Start/Health/Reload/Drain/Stop/Diagnostics，`go-plugin-process` 当前返回 partial process data-plane，sandbox/WASM 仍返回明确 unsupported。
- `plugin-host` 子命令、`mc-gateway-plugin-host/v1` handshake、UDS control channel、supervisor start/stop foundation、host 内 Init/ReloadConfig/Destroy lifecycle、loaded host crash summary refresh、service-level last error persistence、persisted configurable restart backoff/max/window policy、crash-loop auto-isolation、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket handshake orphan cleanup、Linux `/proc` process-table orphan discovery、`upstream.connect/v1` dialer bridge 和 protocol-proxy drain-only stream bridge 已落地；`go-plugin-process` 仍需要跨节点 crash policy 协调和跨平台进程表 orphan discovery。
- sandbox-process 需要把 capability 变成 OS/container 级强约束，而不是只做审计。
- WASM 需要 host ABI、module cache、fuel/time/memory limit 和限定 extension point。
- sandbox/isolated runtime 如果支持 protocol-proxy，必须先定义 `stream.proxy/v1` 或 `upstream.connect/v2`，不能直接复用进程内 `net.Conn` 语义；可信 `go-plugin-process` 当前通过内部 host control stream bridge 承载 drain-only。

## 当前 runtime 到底实现了哪些

### 已实现

- `in-process` service mode。
- `go-plugin` runtime。
- `plugin.Open(plugin.so)` 加载。
- `Plugin` symbol 或 manifest `entry_symbol` 查找。
- `ReloadConfig()`、`Init()`、`Destroy()` 生命周期。
- `upstream.connect/v1` dialer mode。
- `upstream.connect/v1` protocol-proxy mode 在进程内通过 `net.Conn` 接管。
- panic recover、handler timeout、active proxy count、drain 和 force close。
- `official.rule-policy` builtin 特例，但它不是通用第三方 builtin runtime。

### 部分实现或预留

- `go-plugin-process` service mode：能保存 desired/active、显示 host summary、从 loaded host process 刷新 crash summary、对 crash loop 做 service-level last error persistence 和 persisted configurable restart backoff/max/window policy、自动从 dispatch 隔离 crash-loop host、启动真实 plugin-host 子进程、在子进程内加载 Go plugin、清理 supervised/stale control socket、metadata-backed process orphan sweep、无 metadata 的 stale active control socket 和 Linux `/proc` process-table orphan，并通过 UDS relay 支持 `upstream.connect/v1` dialer mode 和 protocol-proxy drain-only；跨节点 crash policy 协调和跨平台进程表 orphan discovery 尚未完成。
- `sandbox-process` runtime：manifest/runtime type 和 gate 存在，但没有 sandbox。
- `wasm` runtime：只有 `WASMRunner` 模拟 timeout/panic/memory，不是真实 WASM。
- runtime-neutral CLI adapter：只有 `go-plugin` 有 build/test adapter，其他 runtime 返回 reserved。
- runtime conformance：命令 reserved，尚不能作为新增 runtime 的准入门槛。

## Roadmap 推荐顺序

这里的顺序按“能让系统更接近可上线状态”和“后续能力依赖”排序，不按阶段号机械排列。

### R0：建立事实源和验收框架

这不是一个功能点，但应该先做。当前最大风险是文档、UI、CLI、状态模型和真实数据面之间的表达不一致。

应做：

1. 更新 `plugin features` 输出，给 runtime、service mode 和 extension point 能力标注 `implemented`、`partial`、`reserved`、`stub`、`data_plane`、`requires_restart`。
2. Admin 插件页已展示真实数据面 runtime，不把 service mode desired value 当作实际运行模式。
3. 增加 roadmap/phase exit checklist，列出每阶段必须跑的命令和 fixture。
4. 把 reserved 命令集中列到文档，明确不是已完成能力。

验收：

- 管理员从 UI/CLI 不会误认为 `go-plugin-process`、sandbox、WASM、sign、promotion 已可用。
- 每个阶段都有可重复验证步骤，而不是只看代码存在。

### R1：压实现有 `in-process go-plugin` 主路径

阶段 1/2 是系统现在真正可用的数据面，应该先把它验收完整。

应做：

1. binary `.mcgp` 上传、load、enable、disable、delete、restart recovery 端到端测试。
2. `upstream-rewrite` binary/source 构建和启用 fixture；示例已覆盖匹配 host dialer rewrite 和非匹配 `api.ErrPass`。
3. protocol-proxy golden 场景声明和运行时测试已覆盖 initial data replay、panic、timeout、endpoint close、client close、backpressure 大包、drain、force close；后续补更多真实 MC 异常路径和跨版本 smoke。
4. `mc-auth-proxy` 示例已覆盖登录失败响应、backend unavailable 分支、fixture_accept backend 转发大包、低基数 auth 事件和 `auth.attempts` metric。
5. dispatch plan 和 active proxy summary 纳入验收。

验收：

- 阶段 1/2 的“可用性检查点”都能由命令或测试证明。
- 插件失败不破坏默认 route，不污染旧 dispatch table。

### R2：完成 Admin/config/secret/rollback 的生产闭环

阶段 4 已经有大量实现，下一步应该修正状态表达并补足失败路径验收。

应做：

1. UI 已对 runtime service mode 明确 desired、active、effective data-plane 和 reserved/partial 状态；artifact/source/build/governance 的更多失败路径仍需继续验收。
2. config dry-run、sensitive diff、secret ref、artifact rollback、config rollback 增加端到端验收。
3. member/admin 权限、审计日志和失败不改变 desired generation 的测试。
4. runtime service panel 只允许设置 future desired mode，不能暗示当前进程已经切换 runtime adapter。

验收：

- 错误配置、缺失 secret、rollback gate 失败都不影响当前 active 插件。
- 审计记录能解释谁改了什么、是否影响 active state。

### R3：source build 从开发能力升级为生产能力

source package 已有 local-process 和 container 路径，prod 默认 container，且 local-process source-built artifact 已被 governance 阻断。container provenance 已能区分 digest-pinned builder image、浮动 tag 和 plugin API/Go release 绑定，并把浮动 tag 或 release 未绑定作为 prod warning/override 条件。external CI binary artifact 已有 assessment gate，要求签名验证、source/artifact sha、run/builder identity 和 trusted 标记；剩余缺口比很多未来 runtime 更靠近当前可上线边界：官方 builder image 发布、完整 CI artifact 发布链和跨环境验收证据。

应做：

1. 发布官方 builder image；当前准入已检查 gateway plugin API version 和 Go version 绑定，但镜像发布流程仍需落地。
2. 把 external CI binary artifact 的签名、source/artifact sha 和 provenance 信任策略从本地 assessment gate 扩展到官方 CI 发布流程。
3. build log 脱敏和环境变量白名单验收。
4. provenance gate 继续覆盖 promotion/repository import 等跨环境路径。
5. build cancel/retry、source/build log/artifact GC 的失败路径和跨环境验证。

验收：

- 生产 profile 下可以禁用 local-process，只允许 container/external CI 产物。
- 构建失败不会改变 active artifact。
- 日志和 provenance 不泄露 secret/token/私有路径。

### R4：把 governance 变成真正 release gate

阶段 5 的模型已经比较完整，下一步是让它成为所有高风险操作的统一门禁。

应做：

1. enable、rollback、repository import admission preview、本地 repository import apply 和本地 target promotion apply 已统一走 governance；repository/promotion apply 只写入目标环境 desired state，不自动启用 active 流量；手动 remote artifact transfer 已落地，CLI promotion 跨网关 apply 已串联 artifact package transfer 和目标侧 apply，repository apply 跨节点编排仍需继续补齐。
2. preflight/self-test/benchmark 结果和 review 指纹绑定 artifact/config/scope/rollout/runtime limits/policy hash；instrumentation metadata 已要求 `available` 状态必须携带 generated diff hash、gateway binary digest、CI artifact digest、passing conformance、benchmark 和 smoke evidence，且 Admin 列表已展示 digest binding 和三类证据状态。
3. conflict analysis 覆盖 protocol-proxy scope、provider singleton、middleware ordering。
4. advisory revoke/quarantine 后验证 upstream dispatch、extension dispatch 和 route cache 被移除，后台任务停止，runtime 进入 draining，且 rollback 阻断。
5. warning override TTL 过期后重新阻断。
6. conformance golden fixtures 已支持 protocol-proxy、route/status/rule/middleware 和 governance gate 场景声明；`invalid_config`/`missing_secret` 已先使用 manifest 证据判断 pass/skip/fail，packaged fixture failure 已接入默认 preflight/governance gate，缺失 packaged fixture 可通过 strict preflight/Manager policy gate 强制阻断，且 instrumentation release metadata 已先接入 conformance/benchmark/smoke pass gate。后续要补真实 fixture 输入执行并逐步把缺失 fixture strict gate 变成默认发布策略。

验收：

- 高风险 protocol-proxy 未 review 不能在 prod 启用。
- advisory 命中 artifact 不能 rollback；quarantine 命中后不会继续从旧 dispatch 或 route cache 接管流量。
- benchmark 超阈值进入 warning/blocking，且 override 行为可审计。

### R5：补齐 observability 和 operations 验收

阶段 6 的实现很多，但需要从“有内部模型”转成“运维能排障和清理”。

应做：

1. metrics/events/trace 已覆盖低基数和脱敏验收，feature matrix 暴露 `event_low_cardinality_gate=true`、`metric_label_gate=true` 和 `trace_summary=true`。
2. diagnostic package 已覆盖生成、结构化脱敏、runbook section 和默认 7 天 retention GC；后续补更统一的跨类别长期保留策略。
3. background task 手动触发、超时、non-reentrant、失败重试、cancel、singleton lease renew 和 lease lost cancellation 已有验收；后续补跨类别长期保留和更细的多实例验收。
4. PluginDataStore/FileStore 的 quota、retention、GC dry-run/apply 已有验收；FileStore 覆盖写入已按净增长计算配额，避免同文件重写误报超限；operations GC apply 已覆盖 expired plugin_data、expired plugin_file、diagnostic package 和 runtime orphan file。
5. ExternalClient 的健康、熔断和错误摘要。

验收：

- 插件问题能从 Admin/API 看到 handler、trace、event、external dependency、task、file/data 状态。
- GC dry-run 能解释将删除什么，apply 能删除过期 data/file 与 orphan runtime file 并写审计。

### R6：按使用价值收尾 extension ecosystem

扩展生态不应和 runtime 抢同一个优先级判断。它的价值在于降低常见需求的插件开发成本。

建议顺序：

1. official rule/policy：维护模式、host rewrite、source CIDR allow/deny、简单限流、upstream rewrite、status MOTD/favicon/online/max/version/window 已有测试覆盖。
2. route provider：外部源刷新、cache、SQLite fallback、decision explain。
3. status ping：MOTD、favicon、online/max、version、maintenance message。
4. event subscriber：best_effort/at_least_once、retry、dead letter、replay/drop 已有行为验收；后续补更完整的持久化死信和跨节点投递策略。
5. middleware：connection/handshake filter、确定排序、fail policy，并把这些行为纳入 conformance fixture 场景声明。
6. provider registry：singleton、priority/fallback、dependency declaration。
7. Admin auth provider：先保留 break-glass，本地账号不能被外部 provider 失败拖垮。

验收：

- 每个 extension point 都有一个最小示例和 conformance fixture。
- `examples/plugins/extension-ecosystem` 已覆盖 route/status/event/provider fixture 行为。
- 插件 disable 后恢复 core 默认行为。
- event subscriber 失败不影响连接路径，死信 replay/drop 可审计。
- Admin Dispatch plan 面板可触发 route refresh、subscriber dead-letter replay/drop，避免这些能力只停在 API。

### R7：实现 `go-plugin-process`，但不要把它当 sandbox

这是 runtime 路线的第一个实际工程目标。它解决 Go plugin 不能卸载、资源回收弱、崩溃影响边界的问题，但不解决不可信代码。

应做：

1. plugin-host 子进程或子命令（已落地）。
2. UDS control channel 和 host protocol version negotiation（已落地）。
3. Start/Init/ReloadConfig/Health/Destroy/Drain/Stop lifecycle 已作为 adapter/control contract 落地，host 内 Init/ReloadConfig/Destroy 已能加载真实 Go plugin。
4. supervisor start/stop 已有 foundation，并已接入 `upstream.connect/v1` dialer 和 protocol-proxy drain-only process data-plane。
5. crash tracking、unexpected clean exit crash classification、loaded host summary refresh、service-level last error persistence、persisted configurable restart backoff/max/window policy、crash-loop auto-isolation、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket cleanup 和 Linux `/proc` process-table orphan cleanup 已有 foundation；跨节点 crash policy 协调和跨平台进程表 orphan discovery 仍需接入。
6. `upstream.connect/v1` dialer mode 和 protocol-proxy drain-only 已支持。

验收：

- 主进程不 `plugin.Open()` 目标 `.so`。
- 子进程退出后 OS 回收 `.so` 和 Go heap。
- plugin-host crash 不导致 Admin/gateway 主进程退出。

### R8：跨进程 stream、sandbox 和 WASM

这批能力应该在 `go-plugin-process` drain-only 和 conformance 基础上推进。

应做：

1. `stream.proxy/v1` 或 `upstream.connect/v2`，定义 half-close、backpressure、deadline、cancel、byte accounting。
2. sandbox supervisor 和 filesystem/network/env/cpu/memory/secret capability enforcement。
3. WASM host ABI、module loader/cache、fuel/time/memory limits。
4. 首批 WASM extension point 限定在 rule/route/config validate 等低风险场景；当前 placeholder validation 已拒绝 `upstream.connect/v1` 等高风险 extension point。
5. `ingress.service/v1` schema 已保留，CLI/preflight 会校验 capability 声明，governance 会检查已启用 ingress 插件之间以及启动时内置服务 reservation 的端口冲突，且启用会被 `ingress_service_reserved` 阻断；listener owner、TLS/secret 装载和 disable drain 仍需实现。

验收：

- required capability 无法强制时阻断启用。
- WASM timeout/trap/memory limit 只影响当前调用。
- sandbox/WASM 不能访问未授权 secret/network/files。
- `ingress.service/v1` 在 listener lifecycle 实现前只能通过 schema/contract/preflight 校验识别，不能启用。

### R9：repository、signature、SBOM、license 和 advisory feed

这部分重要，但它解决的是“拿到什么”和“是否允许进入本地准入”，不能替代 runtime 隔离和治理门禁。

应做：

1. official/internal/file/url repository index。
2. content-addressed local artifact store。
3. signature verify、key rotation、revoke。
4. SBOM parse、本地/导入式漏洞库、license allow/deny。
5. advisory/vulnerability feed sync、rescan 和 opt-in external feed scheduler；完整 CVE/SBOM 自动扫描链后续实现。
6. repository import 后只生成本地 artifact，不自动 enable，并保存 governance admission preview；本地 apply 会重新执行 dry-run/governance 后写入 disabled desired state。

验收：

- 仓库候选版本导入后仍需本地 review/gate，导入记录的 `admission_json` 能展示当前策略下的阻断原因。
- 仓库删除版本不删除本地 artifact。
- denylist/advisory 阻断 rollback、repository import apply 和 promotion apply。

### R10：promotion、多实例和 DR

这应排在 artifact/runtime/governance/operations 稳定之后。

应做：

1. promotion export/import/diff/drift/dr-drill 从 reserved 变成真实 CLI/Admin 能力。
2. target desired-state apply 已可用：bundle 不导出 config 明文，目标环境必须提供配置并通过 config hash、artifact、dry-run 和 governance 校验；成功后只写 desired state。
3. 带 `secret_refs` 的 bundle 会提示目标环境 secret mapping，完整环境覆盖仍需继续实现。
4. 多实例 gateway node heartbeat、插件维度 node runtime state、local artifact package mirror、手动 remote artifact transfer、CLI promotion 跨网关 apply 和 partial rollout 展示已可验证；跨节点自动分发仍未完成。
5. background task 的 per-node、singleton、sharded 和 SQLite lease 已可验证；repository apply 跨节点编排和自动集群级 apply 仍未完成。
6. DR drill 已在不接生产流量时验证 artifact/config/runtime 支持度；CLI 保留离线静态检查，Admin 目标侧 drill 会检查本地 artifact、目标配置 hash、dry-run 和 governance。

验收：

- 目标环境不支持的 runtime 或 policy 不会被 promotion apply 写入 desired state，更不会自动启用 active 流量。
- drift report 能解释实际运行态和期望态差异。

## 近期建议执行队列

如果下一步要继续实现，建议按下面顺序拆任务：

1. **修正事实表达**：更新 `plugin features`、Admin runtime service panel、文档状态表，明确 implemented/partial/reserved/stub。
2. **补阶段 1/2 验收**：binary/source upstream-rewrite、protocol-proxy、mc-auth-proxy fixture、disable/drain/restart recovery。
3. **补阶段 4 验收**：config/secret/rollback UI/API 失败路径和审计。
4. **实现 container builder**：把 source package 从开发可用推进到生产可控。
5. **补 conformance 框架**：至少覆盖 manifest、upstream.connect、protocol-proxy、route/status/rule、governance gate。
6. **压实 governance/operations**：review/preflight/self-test/benchmark/advisory、diagnostic、GC、background task。
7. **按价值收尾 extension ecosystem**：official rule/policy、route provider、status ping、event subscriber。
8. **再进入 runtime 扩展**：`go-plugin-process` drain-only、stream relay、sandbox、WASM。
9. **最后做分发和多环境**：repo/sign/SBOM/license、promotion、multi-node、DR。

## 判断一个功能是否真的完成

后续不要只看表、接口或 UI 是否存在，应按下面标准判断：

1. 是否改变真实数据面或真实管理面。
2. 是否有端到端验证，而不是只有单元模型。
3. 是否验证失败路径：加载失败、构建失败、启动失败、panic、timeout、disable、rollback、restart recovery。
4. Admin、CLI、API 是否表达同一个事实。
5. 是否有 conformance/golden fixture 防止语义漂移。
6. 是否不会把 reserved/stub 能力误导成生产可用能力。

按这个标准，当前 roadmap 的重点不是“runtime 或 extension 二选一”，而是先把已实现主路径做成可验收、可生产、可解释，再逐步推进 future runtime 和分发能力。
