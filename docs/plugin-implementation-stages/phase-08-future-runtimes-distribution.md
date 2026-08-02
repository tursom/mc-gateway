# 阶段 8：Future Runtimes And Distribution

## 目标

引入最终设计中的未来能力，同时保持前七个阶段的默认路径可用。包括 `go-plugin-process`、sandbox-process、WASM、ingress service、插件仓库、签名、SBOM 漏洞扫描、许可证策略和 build-time instrumentation。

本阶段由多个可选子阶段组成。每个子阶段都必须可以单独启用或回滚，不能要求一次性切换所有 runtime。

## 可用性检查点

阶段结束时必须能做到：

- 默认 `in-process go-plugin` 路径仍然可用。
- 管理后台可以配置插件服务启动模式 desired value，并明确 restart required。
- `go-plugin-process` 至少支持 drain-only。
- sandbox-process/WASM 如果启用，capabilities 能被强制或阻断启用。
- 仓库导入只生成本地 artifact，不自动启用生产流量。
- 签名、SBOM、license 和 advisory 策略能参与准入结果。

## 子阶段 A：go-plugin-process

实现：

- gateway 插件服务启动模式：`in-process`、`go-plugin-process`、`sandbox-process`。
- Admin 系统配置项 `plugin_service.desired_mode`。
- 启动时读取 desired mode，校验后写入 `plugin_service.active_mode` 和 `applied_at`。
- `restart_required` 由 desired/active 差异推导。
- 环境级连接迁移开关：`drain-only`、`fd-live`、`fd-live-shm`。
- plugin-host supervisor。
- 子进程 lifecycle。
- UDS control channel。
- `drain-only` 进程级卸载。
- 可选 `fd-live`。
- 可选 `fd-live-shm`。
- shared memory state schema。
- `quiesce/snapshot/restore` conformance。

默认：

- `in-process`。
- `active_mode` 只由 gateway 启动流程写入。
- `go-plugin-process` 切换需要重启。
- live migration 默认关闭。
- 迁移默认模式为 `drain-only`。

验收：

- Admin 修改 `plugin_service.desired_mode` 后不立即切换当前进程，页面展示 restart required。
- 重启后 gateway 按 desired mode 创建对应 RuntimeAdapter 或 plugin-host supervisor，并更新 active mode。
- `go-plugin-process` 模式下启用 upstream-rewrite。
- disable 后旧 plugin-host drain 并退出。
- 子进程退出后 `.so` 和 Go heap 被 OS 回收。
- crash loop 不影响 Admin 主进程。

## 子阶段 B：sandbox-process

实现：

- control RPC。
- supervisor。
- crash loop policy。
- secret RPC/handle。
- filesystem/network/env/cpu/memory capability enforcement。
- stream relay 或 `stream.proxy/v1`。

验收：

- sandbox 插件崩溃不导致 gateway 崩溃。
- 无法强制 required capability 时阻断启用。
- secret 不通过长期环境变量注入。

## 子阶段 C：WASM

实现：

- WASM host ABI。
- route/rule/config validate extension point。
- memory/time limits。
- no file/no network 默认策略。

验收：

- WASM rule 插件可以返回 allow/deny/rewrite。
- 超时或内存超限只影响当前调用。
- WASM 插件不能访问未授权 secret/network。

## 子阶段 D：ingress service

实现：

- `ingress.service/v1` schema。
- service supervisor。
- listener ownership by gateway。
- port conflict check。
- TLS/secret refs。
- health and drain。

验收：

- 插件声明入口服务后，由 gateway 创建 listener。
- disable 后停止接收新连接并 drain。
- 端口冲突阻断启用。

## 子阶段 E：repository and supply chain

实现：

- official/internal/file/url repository index。
- repository trust policy。
- artifact download to local store。
- signature verification。
- SBOM vulnerability scan。
- license allowlist/denylist。
- advisory feed sync。
- update availability。

验收：

- 仓库候选版本导入后仍需本地 review。
- 仓库删除版本不删除本地 artifact。
- denylist/advisory 仍阻断 rollback 和 promotion apply。

## 子阶段 F：build-time instrumentation

实现：

- instrumentation manifest。
- official/organization CI profile。
- generated diff hash。
- provenance。
- conformance/benchmark/smoke gate。

验收：

- 插桩产物作为 gateway binary 发布，不进入 plugin artifact hot-load lifecycle。
- Admin 展示 instrumentation metadata。
- 插桩影响连接路径时 Runbook 说明回滚方式。

## 明确不做

- 不把 `go-plugin-process` 当成不可信 sandbox。
- 不把 fd 迁移当成跨平台通用能力。
- 不把 WASM 用于完整 MC connection takeover。
- 不让仓库自动启用生产插件。
- 不允许普通上传包携带插桩规则直接改 gateway binary。

## 实现任务

本阶段按子阶段逐个实施。每个子阶段都必须满足：

1. 默认 `in-process go-plugin` 路径不回归。
2. 新 runtime 或供应链能力可通过 feature flag、service mode 或 policy profile 关闭。
3. Admin/API 能展示当前 active 状态、desired 状态和失败原因。
4. promotion import 不能自动启用目标环境不支持的 runtime 或策略。
5. conformance 覆盖新增 manifest 字段、feature key、错误码和回滚路径。

建议顺序：

1. 先实现 service mode 数据模型和 Admin 展示。
2. 再实现 `go-plugin-process` 的 `drain-only`。
3. 再评估 `fd-live` 和 `fd-live-shm`。
4. 然后引入 sandbox-process 和 WASM。
5. 最后引入仓库、签名、SBOM 扫描和 build-time instrumentation。

## 验收

- 未开启任何 future runtime 时，阶段 1 到阶段 7 的插件能力仍通过回归验证。
- service mode 从 `in-process` 切换到 `go-plugin-process` 时明确提示 restart required。
- `go-plugin-process` 子进程 crash 不导致 Admin 主进程退出，并能展示 crash loop 状态。
- sandbox-process required capability 无法强制时，启用被阻断而不是降级为只审计。
- WASM 插件超时、panic 或内存超限只影响当前调用。
- repository import 只生成本地 artifact，并重新进入本地准入、review、enable 流程。
- build-time instrumentation 产物不会出现在 runtime plugin enable/disable 列表中。

## 回滚策略

- runtime mode 切换失败时回到 `in-process`。
- sandbox/WASM 插件失败时禁用对应 plugin，不影响 `go-plugin` 插件。
- repository 功能失败不影响本地 artifact。
- build-time instrumentation 回滚到上一 gateway binary。
