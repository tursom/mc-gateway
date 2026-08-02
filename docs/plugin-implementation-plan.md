> **Archived:** This document records the superseded pre-v2 plugin design. Current behavior is defined by `docs/plugin-development/extension-points.md`.

# 插件系统阶段实现计划

本文以 [plugin-system-design.md](plugin-system-design.md) 作为最终目标设计文档，把插件系统拆分成多个可上线的实现阶段。每个阶段都必须在结束时保持 gateway 当前可用：可以启动、可以回滚、可以排障，且不会要求后续阶段补齐后才能恢复基本能力。

插件开发工具链作为跨阶段交付项单独设计，见 [plugin-development-toolchain-design.md](plugin-development-toolchain-design.md)。工具链主入口为 `gateway plugin init/build/test`，并需要从第一批 Go plugin 示例开始预留未来 runtime adapter。

## 拆分原则

- 以可用的纵向切片拆分，而不是按数据库、API、UI、SDK 等横向模块拆分。
- 每个阶段都必须有明确的启用路径、失败回退路径和最小运维证据。
- 默认运行路径保持保守：先稳定 `go-plugin + legacy upstream-connect contract`，再增加源码包、治理、观测和未来 runtime。
- 未来能力必须可关闭、可灰度或只做设计预留，不能破坏前一阶段的可用状态。
- `plugin-system-design.md` 是目标状态；阶段文档只描述实现顺序和阶段边界。

## 阶段总览

| 阶段 | 文档 | 阶段结束时可用状态 |
| --- | --- | --- |
| 1 | [Managed Binary Plugin MVP](plugin-implementation-stages/phase-01-managed-binary-mvp.md) | 管理员可以上传二进制 `.mcgp`，通过 Admin API/CLI 加载、启用、禁用、删除可信插件；`legacy upstream-connect contract` route.resolve/v1 provider 可用 |
| 2 | [Protocol Proxy MVP](plugin-implementation-stages/phase-02-protocol-proxy-mvp.md) | 旧版 connection takeover mode 可用，插件可以接管 MC 字节流并实现登录代理示例 |
| 3 | [Source Package Builder](plugin-implementation-stages/phase-03-source-package-builder.md) | 管理员可以上传 source `.mcgp`，受控 builder 产出可加载 artifact，构建失败不影响当前插件 |
| 4 | [Admin UI, Config, Secret, Rollback](plugin-implementation-stages/phase-04-admin-ui-config-secret-rollback.md) | 管理页具备可操作的插件管理闭环，支持配置 schema、secret、配置快照和回滚 |
| 5 | [Governance And Release Gates](plugin-implementation-stages/phase-05-governance-release-gates.md) | 生产启用前有准入策略、review、冲突分析、preflight/self-test、性能门禁和安全公告处理 |
| 6 | [Observability And Operations](plugin-implementation-stages/phase-06-observability-operations.md) | 插件 metrics、events、trace、日志、诊断、background task、plugin_data 和文件资源治理可用 |
| 7 | [Extension Ecosystem](plugin-implementation-stages/phase-07-extension-ecosystem.md) | 在稳定主路径上增加 route/status/provider/event/rule/Admin auth 等扩展点和官方插件能力 |
| 8 | [Future Runtimes And Distribution](plugin-implementation-stages/phase-08-future-runtimes-distribution.md) | 可选引入 `go-plugin-process`、sandbox/WASM、ingress service、仓库、签名和构建期增强；默认路径仍可运行 |

## 功能到阶段映射

下表用于确认 [plugin-system-design.md](plugin-system-design.md) 中的目标能力已经拆入某个阶段。一个能力可能在早期阶段先实现最小可用版本，后续阶段再补齐治理、UI 或生态扩展。

| 目标能力 | 阶段 | 拆分说明 |
| --- | --- | --- |
| `.mcgp` binary artifact、manifest 静态校验、artifact 登记 | 1 | 先支持二进制可信 Go plugin，上传不执行代码 |
| Plugin Manager、desired/runtime state、dispatch table、审计 | 1 | 建立正式管理路径，替代探索式 config 插件入口 |
| `legacy upstream-connect contract` route.resolve/v1 provider | 1 | 第一条可用数据路径，覆盖 upstream rewrite、自定义拨号 |
| `legacy upstream-connect contract` connection takeover mode 和 `net.Conn` 接管 | 2 | 支持完整 MC 字节流接管、initial data replay、draining |
| MC 正版/三方登录插件、forwarding、登录后协议处理 | 2 | 由 connection takeover 插件实现，core 不消费认证结果 |
| Minecraft capability manifest、protocol smoke fixture | 2 | 支撑管理页展示和后续发布门禁 |
| source `.mcgp`、builder、构建 provenance | 3 | 源码包构建成 `plugin.so` 后复用阶段 1/2 加载路径 |
| builder 隔离、Go/module/ABI 记录、source/build log GC | 3 | 构建失败不影响 active artifact |
| `gateway plugin init/build/test` 开发工具链 | 1-3，后续扩展 | 阶段 1/2 提供 Go plugin 模板和 harness，阶段 3 收敛 source/binary 打包；后续 runtime 通过 adapter 接入 |
| Admin 页面基础管理闭环 | 4 | 上传、构建状态、加载、启用、禁用、删除、回滚 |
| 配置 schema、配置快照、配置迁移入口 | 4 | 错误配置不切换 active artifact |
| SecretStore、secret version、reload/rotation 基础 | 4 | secret 不在页面、日志、审计中明文展示 |
| artifact rollback、config rollback | 4 | 回滚前重新执行当前基础门禁 |
| admission policy、review、risk、warning override | 5 | 生产启用前可解释和可审计 |
| composition conflict、scope overlap、dispatch plan | 5 | 阻断 connection takeover 重叠、provider 单例冲突等 |
| preflight/self-test、benchmark release gate | 5 | 高风险插件启用前有证据 |
| denylist、quarantine、revoke、安全公告 | 5 | 阻断受影响 artifact 的 enable/rollback |
| metrics、custom metrics、business events、trace | 6 | 提供运行时观测和脱敏摘要 |
| plugin logger、diagnostic package、Runbook 支撑 | 6 | 插件故障可定位、可降级、可导出摘要 |
| background task、ExternalClient、外部依赖治理 | 6 | 周期同步、受控外联、熔断和健康状态 |
| PluginDataStore、PluginFileStore、runtime file GC | 6 | 插件私有数据和文件资源受配额/retention 管理 |
| route resolver/provider、route decision | 7 | 降低动态路由和外部 CMDB 集成成本 |
| status ping、MOTD、维护模式 | 7 | 不必完整 connection takeover 即可定制状态响应 |
| middleware、provider、event subscriber、rule/policy engine | 7 | 补齐 Hook 之外的生产扩展形态 |
| Admin auth provider、外部身份绑定 | 7 | 只影响管理页登录，保留本地 break-glass |
| `go-plugin-process` 服务启动模式、进程级卸载、fd/shm 迁移 | 8 | 未来可选，默认 `in-process` 路径仍可运行 |
| sandbox-process、WASM、capability enforcement | 8 | 面向隔离、跨语言和轻量规则场景 |
| `ingress.service/v1` 自定义入口服务 | 8 | 由 gateway/supervisor 管理 listener，不允许插件任意监听 |
| 插件仓库、签名、SBOM 漏洞扫描、license policy | 8 | 仓库只导入本地 artifact，不自动启用 |
| build-time instrumentation | 8 | 官方/组织 CI 能力，产物是 gateway binary，不是热加载插件 |
| promotion、drift、DR drill | 4-6 | 阶段 4 建立回滚和快照，阶段 5/6 补齐门禁、diff、诊断和演练证据 |

## 全阶段不变量

这些规则从阶段 1 开始就不能被破坏：

- 上传包校验不能执行插件代码。
- 生产路径统一以 `.mcgp` artifact 为单位管理。
- 插件管理写操作必须有审计日志。
- 启用失败不能破坏旧 dispatch table。
- 禁用插件后，新连接不能再进入该插件。
- 已加载 Go plugin 不能承诺真正热卸载；只能逻辑禁用或未来通过 `go-plugin-process` 退出子进程回收。
- MC 正版/三方登录、身份映射、forwarding 和后续协议处理属于 connection takeover 插件，不由 gateway core 拼装。
- 玩家名、UUID、source IP、secret、token、session response 和 packet payload 默认不进入指标标签、审计明文或普通诊断输出。

## 阶段推进规则

进入下一阶段前必须满足：

- 当前阶段文档中的验收项全部通过。
- 已实现能力有最小自动化测试或可重复手动验证步骤。
- 失败路径已验证：加载失败、启用失败、禁用、删除、重启恢复。
- 文档已更新：用户怎么启用、怎么回滚、怎么排障。

如果某阶段出现实现复杂度超出预期，允许拆出子阶段，但子阶段也必须保持“当前可用”。
