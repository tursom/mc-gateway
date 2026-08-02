> **Archived:** This document records the superseded pre-v2 plugin design. Current behavior is defined by `docs/plugin-development/extension-points.md`.

# 插件系统设计

## 背景

当前 gateway 的插件运行时已经收敛为单一管理路径：

- `plugin/api` 定义了插件接口、Gateway 能力和 hook 类型。
- `internal/pluginmanager` 负责制品加载、实例创建、配置、生命周期和数据面分发；正式 runtime adapter 从已纳管 `.mcgp` 制品中加载 `plugin.so`。
- SQLite 和 Admin 是插件 desired state、配置和制品的唯一管理入口；旧 `[plugin.*]` TOML loader 已移除。
- `HookUpstreamConnect` 是正式上游扩展点；已打包旧插件注册的 `HookUpstream` 由 Plugin Manager 映射到同一只读 dispatch snapshot。

本文描述收敛后的插件系统边界、ABI、管理页操作和生命周期。可信插件通过管理页上传、加载、启用、禁用和删除，开发者基于稳定的 API/ABI 编写自定义插件。

本文扩展了管理页设计中的原第一版边界。插件管理属于后续增强能力，不恢复旧的 `config.toml` 配置模型。

面向管理员的可执行操作流程见 [插件系统使用指南](plugin-usage-guide.md)，面向插件作者的开发流程见 [插件系统开发指南](plugin-development-guide.md)。本文保留目标设计、边界和决策说明。

## 目标

- 插件正式进入 SQLite/Admin 管理体系。
- 管理页支持上传、加载、启用、禁用、切换版本和删除插件。
- 支持可信开发者基于 `plugin/api` 自行编写插件。
- 第一版继续使用 Go `-buildmode=plugin` 的进程内 `.so` 插件。
- 尽量支持热加载：兼容且未加载过的插件可以不重启加载并启用。
- 明确热卸载限制：Go plugin 不能真正从进程中卸载，只能逻辑禁用。
- 通过 manifest source 记录插件 ID、版本、目标平台、构建 Go 版本、SDK/API 版本、声明的 extension point 和配置 schema；源码目录只维护一份 `manifest.yaml/yml/toml/jsonc/json`，`.mcgp` 包内统一物化为 canonical `manifest.json`。
- 插件能力采用 Extension Point 模型，Hook 是其中一种；第一版先落 `legacy upstream-connect contract`。
- `legacy upstream-connect contract` 必须支持插件返回自管 `net.Conn`，用于实现完整 stream endpoint/protocol proxy。
- MC 正版/三方登录、身份映射、forwarding 和登录后的协议处理属于插件业务逻辑，不由 gateway core 拼装。
- 提供示例插件和完整开发、构建、上传、启用文档。

## 设计决策索引

本节用于设计评审时快速确认关键方向是否覆盖。详细方案以各后续章节为准。

| 决策/功能点 | 当前设计结论 | 阶段 | 主要章节 |
| --- | --- | --- | --- |
| 插件信任模型 | 第一版只支持可信 native 插件，不把普通插件当不可信代码运行 | 第一版 | Runtime 与权限声明、安全与运维约束 |
| 插件包格式 | 管理页统一上传 `.mcgp` zip 包，源码/二进制由包内 canonical `manifest.json` 的 `artifact_type` 决定 | 第一版 | 包格式、源码包构建 |
| 自定义扩展名 | 不新增源码包扩展名；同一 `.mcgp` 格式承载 binary/source | 第一版 | 包格式 |
| Go plugin ABI | 仅使用 `.mcgp` 内的 `manifest.json` 表示元数据，并记录 Go/API/SDK/ABI fingerprint | 第一版 | Manifest 元数据、Go Plugin ABI Fingerprint |
| Admin 管理 | 上传、构建、加载、启用、禁用、切换版本、删除、回滚和审计都进入 Admin/SQLite | 第一版 | 数据模型、生命周期、Admin API |
| 热加载 | 兼容且未加载过的 native artifact 尽量支持热加载 | 第一版 | 生命周期、热加载和热卸载 |
| 热卸载 | Go plugin 不能真正卸载，只能逻辑禁用，删除已加载 artifact 后提示重启彻底清理 | 第一版 | 生命周期、Runbook |
| 进程级卸载 | 未来支持 `go-plugin-process`：主进程管理，子进程加载 Go plugin 和处理数据面，退出子进程回收已加载 `.so` | 未来 | Go Plugin Process Runtime |
| 源码包 | 支持 source `.mcgp`，由受控 builder 生成最终 `plugin.so` artifact | 第一版 | 源码包构建、构建环境 |
| 构建环境 | 开发可 local-process，生产默认 container builder；prod 会阻断 local-process source-built artifact，并记录 builder/Go/module/provenance 供 governance 和 supply-chain assessment 使用；external CI 产物已有 provenance assessment gate | 第一版 | 源码包构建、供应链 |
| 沙箱 | 第一版不提供沙箱；sandbox-process/WASM 作为未来 runtime adapter | 未来 | Sandbox Runtime、WASM 模型 |
| Extension Point 模型 | Hook 只是类型之一，统一采用 hook/middleware/provider/event/rule 等 extension point | 第一版模型，部分预留 | Extension Point 设计 |
| `legacy upstream-connect contract` | 第一版主扩展点，支持 route.resolve/v1 provider 和 connection takeover mode，返回 `net.Conn` | 第一版 | Hook、net.Conn 接管契约 |
| MC 正版/三方登录 | 由 connection takeover 插件完整实现，core 不拼装登录流程、不消费认证结果 | 第一版能力 | Minecraft 登录插件职责划分 |
| 登录后逻辑 | configuration/play 阶段代理、身份转发、策略和失败响应都由插件自行处理 | 第一版能力 | Minecraft Auth Proxy 插件设计模板 |
| 功能点覆盖 | 从连接治理、MC 运营、安全风控、外部集成、Admin 扩展和实验能力反推 extension point | 设计覆盖 | 功能点覆盖清单 |
| 游戏侧 `auth.provider/v1` | 仅作为未来插件间复用认证来源，不是 core 登录流水线 | 预留 | Provider |
| Admin SSO | `admin.auth.provider/v1` 只服务管理页 OIDC/LDAP/SSO，和 MC 登录分离 | 预留 | Admin Auth Provider |
| 动态路由 | SQLite route snapshot 是默认真相，动态 route provider 必须产生可解释 route decision | 预留 | 路由解析层 |
| 自定义入口服务 | 第一版插件不能新增 listener；未来通过 `ingress.service/v1` 纳入统一服务管理 | 未来 | 插件自定义入口服务 |
| 观测事件 | 插件可上报脱敏业务事件和自定义指标，core 只展示/转发，不改变 MC 登录结果 | 第一版/预留 | 插件业务事件和自定义指标 |
| Mock/Mixin | mock 只用于测试，mixin/source weaving 不作为普通插件；正式能力应抽象为 provider/middleware/rule | 设计约束 | Mock 和 Mixin 的定位 |
| 非侵入注入 | 借鉴构建期插桩思路，但只作为官方 build-time instrumentation 未来能力 | 未来 | Build-Time Instrumentation |
| 示例插件 | 至少提供 upstream rewrite 和 mc-auth-proxy，覆盖 dialer/connection takeover 能力 | 第一版文档/示例 | 示例插件 |

## 需求覆盖审计

本节把设计讨论中已经明确的要求映射到当前文档结论，作为阶段性完成审计。后续实现阶段如果出现新的代码约束，可以在对应章节更新，而不是重新打开已经收敛的方向。

| 要求 | 当前覆盖结论 | 证据章节 |
| --- | --- | --- |
| 只允许可信插件，用户可按 API/ABI 自行编写 | 第一版仅支持 trusted native `go-plugin`，提供 SDK/API、manifest schema 和 conformance | Runtime 与权限声明、Manifest 元数据、SDK/API 契约治理 |
| 管理页上传、构建、加载、启用/禁用、删除和切换版本 | 全部进入 Admin/SQLite 生命周期，删除已加载 native artifact 后提示重启彻底清理 | 数据模型、生命周期、Admin API、Admin 页面 |
| 热加载尽量支持，热卸载承认 Go plugin 限制 | 兼容且未加载过 artifact 可热加载；已加载 Go plugin 只能逻辑禁用，不能真正卸载 | 生命周期、热加载和热卸载、Runbook |
| 多进程模型实现进程级热卸载 | 预留 `go-plugin-process` runtime，主进程只做管理和 fd 编排，子进程负责数据面；通过退出子进程回收 Go plugin | Go Plugin Process Runtime、第一版默认策略 |
| 插件包不新增源码扩展名 | 统一 `.mcgp` zip，源码/二进制由包内 `manifest.json.artifact_type` 声明 | 插件包格式 |
| 支持源码包和构建环境设计 | source `.mcgp` 经受控 builder 生成 `plugin.so`；开发 local-process，生产默认 container builder，外部 CI 作为后续信任路径 | 源码包构建环境、供应链元数据 |
| Manifest 元数据稳定并记录 Go 构建信息 | 作者只维护一个 manifest source；包内 canonical `manifest.json` 是服务端唯一可信元数据来源，记录 Go/API/SDK/ABI fingerprint、builder 和 provenance | Manifest 元数据、Go Plugin ABI Fingerprint |
| Hook 之外的插件技术方案 | 统一 Extension Point 模型，覆盖 hook、middleware、provider、event subscriber、rule/policy；mock/mixin/monkey patch 不作为生产机制 | Extension Point 设计、Mock 和 Mixin 的定位 |
| 沙箱功能要有未来路线 | 第一版不提供沙箱；预留 sandbox-process、WASM、capability enforcement、stream relay 和 egress 策略 | Sandbox Runtime、Runtime Adapter、第一版默认策略 |
| Alibaba 非侵入 Go 注入的参考价值 | 作为官方/组织 build-time instrumentation 未来能力，不作为普通运行时插件或热加载机制 | Build-Time Instrumentation |
| 功能点要尽可能前置考虑 | 已按连接治理、MC 运营、安全风控、动态路由、外部集成、Admin 扩展、仓库、自定义入口、sandbox/WASM 和构建期增强做覆盖表 | 功能点覆盖清单、插件能力矩阵 |
| MC 正版/三方登录插件能力 | 可以实现，但由 connection takeover 插件完整负责登录、身份映射、forwarding 和后续协议处理；core 不消费认证结果 | Minecraft 登录插件职责划分、Minecraft Auth Proxy 插件设计模板 |
| 游戏侧 `auth.provider/v1` 边界 | 只作为未来插件间复用认证来源，不是 gateway core 登录流水线，也不是第一版登录插件依赖 | Provider、已收敛决策 |
| Admin 外部登录边界 | `admin.auth.provider/v1` 只影响管理页 OIDC/LDAP/SSO，本地 admin break-glass 保留，不影响 MC 连接路径 | Admin Auth Provider、Admin API |
| 示例插件和开发文档 | 规划 upstream rewrite、mc-auth-proxy、status 示例，配套 CLI、harness、conformance 和发布门禁 | 示例插件、开发者体验、测试矩阵 |
| 第一版和未来能力边界 | 文档将第一版主路径、预留 extension point、未来 runtime 和非目标分开，并把开放问题收敛为默认决策 | 非目标、能力边界、第一版默认策略、已收敛决策 |

## 非目标

- 不把插件当作不可信代码运行；第一版没有沙箱。
- 不支持第三方插件市场、签名分发和自动升级。
- 不承诺跨 Go 版本、跨 OS、跨架构加载同一个二进制插件。
- 不承诺完全热卸载已经加载过的 Go 插件代码。
- 不在第一版提供插件上传前的自动安全扫描。
- 不支持 Windows 作为 Go plugin 运行目标；最终支持平台以当前 Go `plugin` 包能力和构建验证为准。
- 第一版不允许插件自行注册新的监听端口或长期入口服务；入口服务仍由 gateway core/Admin 服务模型管理。

## 插件能力矩阵

插件系统的目标不是只做单个 hook，而是让 gateway 在可信代码边界内具备可编程能力。不同能力分为第一版落地、设计预留和未来运行时三层。

| 能力层 | 能做到什么 | 主要 extension point | 阶段 |
| --- | --- | --- | --- |
| 入口传输层 | TCP、KCP、QUIC、WebSocket 入口识别、连接来源、服务名、端口复用分支观测 | `connection.accept/v1`、`connection.filter/v1` | 预留 |
| 连接层 | 新连接过滤、拒绝、限流、来源地址策略、连接级审计 | `connection.filter/v1`、`connection.accept/v1` | 预留 |
| 握手层 | 读取和改写 Minecraft handshake、按 host/protocol 做策略 | `handshake.filter/v1` | 预留 |
| 路由层 | 动态路由、外部路由源、按来源/host/route 元数据选择后端、fallback；按玩家选择后端只属于 connection takeover 插件内部或未来 MC 协议扩展 | `route.resolve/v1`、`route.resolver/v1` | 预留 |
| 上游层 | 自定义拨号、TCP/KCP/QUIC/HAProxy 上游、隧道、代理、服务发现、灰度、蓝绿、故障转移 | `legacy upstream-connect contract` | 第一版 |
| 协议代理层 | 插件自管 `net.Conn`，实现完整 MC 协议代理和登录流程 | `legacy upstream-connect contract` connection takeover mode | 第一版能力 |
| 游戏认证层 | 正版/三方登录、白名单、权限系统、身份映射和后续协议处理 | `legacy upstream-connect contract` connection takeover mode | 第一版由 connection takeover 插件完整实现 |
| Minecraft 状态层 | Server list ping、MOTD、favicon、在线人数展示、版本提示 | `status.ping/v1`、`legacy upstream-connect contract` connection takeover mode | 预留；第一版可由 connection takeover 实现 |
| Minecraft 包处理层 | packet 观测、brand、modded handshake、配置阶段策略、压缩阈值策略 | `minecraft.packet.observe/v1`、`minecraft.packet.filter/v1` | 预留；谨慎开放 |
| Admin 认证层 | 管理页登录接入 LDAP、OIDC、企业 SSO | `admin.auth.provider/v1` | 预留 |
| 策略层 | host rewrite、IP 黑白名单、限流、维护模式、条件路由 | rule/policy engine、middleware、hook | 预留，建议官方插件 |
| 观测层 | 连接事件、路由命中、插件状态、指标、审计转发、告警 | event subscriber、`metrics.collect/v1` | 预留 |
| Admin 扩展层 | 外部用户源、审计 sink、插件配置 UI schema、插件状态页 | provider、manifest schema | 预留 |
| 构建期扩展 | 源码包构建、受控插桩、官方高级插件生成产物 | builder、build-time instrumentation | 源码构建第一版，插桩未来 |
| 自定义入口服务 | 插件提供 TLS、PROXY inbound、自定义 UDP 隧道、Bedrock/Geyser 类协议入口 | `ingress.service/v1`、service provider | 未来能力 |

第一版的实际强能力集中在 `legacy upstream-connect contract`：

- route.resolve/v1 provider 可以替换上游连接创建。
- connection takeover mode 可以让插件接管完整连接字节流。
- connection takeover 插件可以把 Minecraft 登录和登录后的代理链路全部做在插件内部。
- gateway core 不提供 Minecraft 登录流水线；它只提供连接接管、初始握手回放、生命周期、配置、secret、观测和治理支撑。
- gateway core 不消费插件内部的登录结果、身份上下文或后续协议状态；这些内容只由插件用于自己的协议代理和业务逻辑。
- 一旦 connection takeover 插件接管连接，该连接的 Minecraft 语义所有权归插件；gateway core 不再从后续字节流推导玩家名、UUID、认证状态、forwarding 状态或 play 阶段信息。
- 源码包和二进制包统一进入 artifact 管理。
- Admin 负责上传、构建、加载、启用、禁用、删除和审计。

## 功能点覆盖清单

设计阶段需要先按“用户希望用插件做什么”反推能力，而不是只围绕当前已有 hook 收敛。下表列出应被设计覆盖的功能点、推荐实现形态和边界。第一版不必全部实现 extension point，但 manifest、Admin、治理、测试和文档应为这些方向预留清晰位置。

| 功能点 | 用户能做什么 | 推荐实现形态 | 阶段和边界 |
| --- | --- | --- | --- |
| 自定义上游连接 | 私有隧道、SOCKS/HTTP proxy、内网穿透、云厂商专线、自定义 KCP/QUIC/HAProxy 拨号 | `legacy upstream-connect contract` route.resolve/v1 provider | 第一版主路径；插件返回真实 `net.Conn` |
| 完整协议代理 | 自行实现 MC 登录、configuration/play 代理、forwarding、后端选择和失败响应 | `legacy upstream-connect contract` connection takeover mode | 第一版能力；core 不解释登录结果 |
| 正版/三方登录 | Mojang/Yggdrasil、Yggdrasil-like、自定义账号系统、混合认证、profile cache | connection takeover 插件内部模块，未来可复用 `auth.provider/v1` | 第一版由插件完整实现；`auth.provider/v1` 只做未来插件间复用 |
| 玩家维度策略 | 白名单、黑名单、ban、会员、权限、风控、玩家分流、玩家 sticky | connection takeover 插件内部实现 | 第一版 core 不维护玩家身份上下文 |
| 连接安全治理 | IP/CIDR 黑白名单、来源限流、连接频率控制、基础 anti-bot、地域策略 | `connection.filter/v1`、rule/policy engine | 预留；第一版可由 connection takeover 或官方规则插件覆盖部分场景 |
| 握手治理 | host rewrite、host alias、protocol version gate、legacy ping 兼容、非法 handshake 拒绝 | `handshake.filter/v1`、`status.ping/v1` | 预留；第一版完整控制可走 connection takeover |
| 状态页和维护模式 | MOTD、favicon、online/max players、版本提示、维护窗口、按 host 展示不同状态 | `status.ping/v1` 或 connection takeover | 预留；需要轻量化时再实现专用 status hook |
| Modded 兼容 | Forge/Fabric/FML handshake、modded backend 选择、modpack 提示和协议透传 | connection takeover，未来 `minecraft.packet.*` | 第一版 core 不理解 modded protocol |
| Packet 观测和过滤 | brand 观测、packet 统计、特定 packet 限速、debug packet logging、协议审计 | `minecraft.packet.observe/v1`、`minecraft.packet.filter/v1` | 预留；filter 高风险，默认不进第一版 |
| 动态路由 | 从 CMDB、Kubernetes、Consul、Nacos、HTTP API 取后端，按健康或权重 fallback | `route.resolve/v1`、`route.resolver/v1`、后台任务缓存 | 预留；第一版可用 `legacy upstream-connect contract` + 插件缓存 |
| 灰度和实验 | 蓝绿、金丝雀、按 source/host/route tag/时间窗口分流、快速回滚 | scope/rollout + `legacy upstream-connect contract` 或 route provider | 第一版支持 source IP sticky；玩家 sticky 由 connection takeover 自己实现 |
| 外部依赖集成 | 会员、风控、权限、告警、日志、对象存储、消息队列、配置中心 | `ExternalClient`、background task、event subscriber | 第一版提供声明、治理和受控 client；native 无法强制阻止绕过 |
| 数据同步任务 | 周期同步路由、白名单、封禁列表、证书/资源、远端配置和缓存预热 | background task + PluginDataStore/FileStore | 第一版设计支持 interval/manual；cron 预留 |
| 观测导出 | 自定义指标、业务事件、trace、审计 sink、外部 SIEM/Prometheus/OTel | plugin event/custom metric/event subscriber/exporter | 第一版提供摘要和 API；外部 exporter 可逐步落地 |
| Admin 管理扩展 | 插件配置表单、状态面板、文档链接、诊断入口、受控 action | 声明式 Admin UI + action API | 第一版不运行插件 HTML/JS |
| 运维动作 | 刷新缓存、探测 backend、清理 profile cache、生成诊断包、手动同步 | plugin action、manual background task | 第一版需权限、confirm token、timeout、审计和脱敏 |
| Admin 外部登录 | OIDC、LDAP、企业 SSO、账号绑定、权限映射 | `admin.auth.provider/v1` | 预留；只影响管理页，不影响 MC 连接 |
| 插件仓库 | 官方/组织仓库、版本发现、离线导入、候选版本对比 | repository index + local import | 未来；不能绕过本地 review 和 enable |
| 进程级热卸载 | 主进程保留管理面，子进程运行 Go plugin 数据面；升级时 drain 或迁移连接 | `go-plugin-process` runtime、fd passing、shared memory migration | 未来；管理后台配置服务启动模式，默认单进程 |
| 自定义入口 | Bedrock/Geyser、TLS termination、PROXY inbound、自定义 UDP 隧道、sidecar 入口 | `ingress.service/v1` + supervisor/service model | 未来；第一版不允许插件自行监听端口 |
| 跨语言和不可信插件 | Python/JS/Rust 插件、低信任规则、强资源隔离 | sandbox-process、WASM | 未来；`legacy upstream-connect contract` 的 `net.Conn` 语义不直接复用 |
| 构建期增强 | 官方观测插桩、安全治理插桩、统一错误/trace 注入 | build-time instrumentation | 未来官方/组织 CI 能力，不是普通热加载插件 |

这些功能点对应的设计含义：

- 第一版必须把 `legacy upstream-connect contract` 做成足够强的 stream endpoint，否则 MC 登录、协议代理和高级上游能力都无法成立。
- 涉及玩家身份、登录结果、packet payload 和 session response 的能力默认归 connection takeover 插件内部所有；core 只接收脱敏事件、指标和健康摘要。
- 常见运维需求应优先沉淀成官方 rule/policy 插件、声明式配置或 Admin action，减少用户为简单规则写 Go 源码包。
- 未来新增 extension point 时，应优先补齐“更低成本、更低风险”的专用入口，而不是削弱 connection takeover 插件的完整接管能力。
- sandbox、WASM、ingress service 和 build-time instrumentation 是不同技术路线，不能混成同一个插件 ABI。

## 典型插件场景

### 动态上游和灰度发布

插件通过 `legacy upstream-connect contract` 在 route.resolve/v1 provider 下接管上游连接：

- 从 Kubernetes、Consul、Nacos、HTTP API 或云厂商 API 获取后端。
- 按 host、来源 IP、时间窗口、权重或后端健康状态选择 upstream。
- 实现蓝绿发布、灰度发布、故障转移和多区域 fallback。
- 使用自定义 tunnel 或代理协议连接私有网络中的后端。

### Minecraft 登录和协议代理

插件通过 `legacy upstream-connect contract` 在 connection takeover mode 下返回自管 `net.Conn`：

- 解析 handshake、login start 和后续 Minecraft 协议。
- 实现正版 Yggdrasil 登录。
- 实现三方 Yggdrasil-like 登录。
- 支持多认证源混合，例如不同 host 使用不同认证源或外部账号系统。
- 做白名单、黑名单、会员状态、外部权限或计费校验。
- 登录失败时返回协议级 kick message。
- 认证成功后使用 Velocity modern forwarding、BungeeCord forwarding 或自定义 forwarding 把身份传给后端。
- 认证成功后继续处理 configuration/play 阶段代理、观测和策略。

这类插件的定位是完整的 Minecraft 协议代理，不是 gateway core 内置认证流程的辅助回调。正版登录、三方登录、身份映射、后端选择、forwarding 和登录后的协议处理都应由插件自己实现；gateway core 只提供稳定的连接接管点、初始握手包回放、生命周期、配置、secret 和观测能力。

因此，不能把这类插件设计成“向 core 提供认证结果”的 auth callback。插件接管后，gateway core 只看到一个普通 `net.Conn`，不会解释登录成功、权限、UUID、profile、forwarding 状态或 play 阶段 packet。插件如果需要把这些结果展示给管理页，只能通过脱敏业务事件、自定义指标、health 和诊断摘要上报，不能让 core 反过来参与登录决策。

反向设计需要禁止：

- core 调用插件拿到 `AuthResult` 后再由 core 继续处理 login/configuration/play。
- core 维护全局玩家身份上下文，并把玩家名、UUID 或权限作为后续路由、灰度、限流的通用输入。
- core 根据插件上报的 `auth.success`、`auth.failure` 事件改变连接结果。
- connection takeover 插件只实现 session 校验，剩余 Minecraft 登录协议由 core 拼装。

如果需要按玩家维度做策略，应由已经解析登录协议的 connection takeover 插件在插件内部实现，或由未来专门的 Minecraft 协议 extension point 显式暴露；第一版 core 不把玩家身份作为通用 extension point 输入。

### Minecraft 状态和协议增强

除登录外，插件还可能用于 Minecraft 协议层运维：

- 自定义 server list ping 响应。
- 动态 MOTD、favicon、在线人数和版本文本。
- 根据来源、host、时间窗口展示维护状态。
- 按 protocol version 提示升级或拒绝旧客户端。
- 识别 Forge/Fabric/FML 等 modded handshake，并选择对应后端。
- 观测 brand、configuration 阶段和 play 阶段关键 packet。
- 对特定 packet 做审计、限流或拒绝。

第一版不需要 gateway core 内置这些 Minecraft 协议能力。需要完整控制时，插件可以用 connection takeover mode 自行实现。后续如果要降低插件开发成本，可以引入专门的 `status.ping/v1`、`minecraft.packet.observe/v1` 和 `minecraft.packet.filter/v1`。

### 规则化运维能力

官方 rule/policy engine 插件可以把常见需求做成配置：

- host rewrite。
- upstream rewrite。
- 来源 IP 黑白名单。
- 简单限流。
- 维护模式。
- 按 host、来源 IP、时间窗口和 route tag 套策略。

这类能力应优先做成官方插件或内置策略引擎，避免普通管理员为简单规则编写 Go 代码。

### 观测、审计和告警

插件通过 event subscriber 和 metrics extension point 接入外部系统：

- 将连接事件、路由命中和插件自行产生的登录结果写入外部日志系统。
- 向 Prometheus、OpenTelemetry 或自定义监控系统导出指标。
- 将插件错误、构建失败、认证失败、后端不可用推送到告警渠道。
- 把审计日志同步到企业审计平台。

### Admin 和企业系统集成

后续 provider 扩展可以让插件接入企业内部平台：

- Admin 登录接入 LDAP、OIDC、企业 SSO。
- 路由配置来自外部 CMDB 或配置中心。
- 审计日志写入外部 sink。
- 插件配置 schema 驱动管理页渲染表单。

### 构建期高级扩展

源码包 builder 后续可以支持受控 build-time instrumentation。它适合官方或高级插件：

- 观测插桩。
- 安全治理插桩。
- 性能统计。
- 对没有显式 extension point 的位置做受控编译期增强。

这类能力不支持热加载，启用或禁用通常需要重新构建并重启，不进入第一版主路径。

## 技术选型

第一版采用 Go 进程内插件：

```text
插件源码
  -> go build -buildmode=plugin
  -> .mcgp 上传包
  -> Admin 上传
  -> SQLite 记录 artifact 和插件配置
  -> Plugin Manager 加载 .so
  -> 注册 extension point handler
  -> gateway 调用扩展点
```

选择 Go plugin 的原因：

- 和当前探索代码一致，改造成本低。
- 插件可以直接使用 Go 标准库和 gateway 暴露的 typed API。
- `legacy upstream-connect contract` 第一版需要返回 `net.Conn`，进程内插件能自然表达这个能力。

需要接受的代价：

- 插件与 gateway 在同一进程，插件 panic、死锁、资源泄漏会影响 gateway。
- 二进制 ABI 对 Go 版本、模块版本、GOOS/GOARCH、构建标签非常敏感。
- `plugin.Open` 加载后的代码不能真正卸载。
- 同一路径的插件二进制不能当作全新版本反复加载。

如果未来需要不可信插件、跨语言插件或强隔离，应设计第二种 ABI，例如外部进程 gRPC 插件。但它很难直接返回 `net.Conn`，需要重新设计转发模型，不作为第一版方案。

## 插件包格式

管理页上传的插件包统一使用 `.mcgp`，本质是 zip 包。包内内容由 canonical `manifest.json` 决定，不通过扩展名区分源码包和二进制包。开发目录可以维护 `manifest.yaml`、`manifest.yml`、`manifest.toml`、`manifest.jsonc` 或 `manifest.json`，但构建进入 `.mcgp` 时必须统一物化为根目录 `manifest.json`。

包类型由两个字段表达：

- `artifact_type` 表示上传包内容：`binary` 或 `source`。
- `runtime.type` 表示运行时类型：第一版为 `go-plugin`，后续可以扩展为 `sandbox-process` 或 `wasm`。

二进制包示例：

```text
upstream-rewrite.mcgp
  manifest.json
  plugin.so
  README.md              # 可选
```

源码包示例：

```text
upstream-rewrite.mcgp
  manifest.json
  go.mod
  go.sum                 # 可选，但推荐
  main.go
  internal/...
  README.md              # 可选
```

包内 `manifest.json` 是加载前可读取的可信元数据，用于避免必须执行插件代码才能知道基础信息。上传阶段只解析 zip 和 manifest，不执行插件代码。

二进制包 manifest 示例：

```json
{
  "schema_version": "mc-gateway.plugin/v1",
  "id": "upstream-rewrite",
  "name": "Upstream Rewrite",
  "version": "0.1.0",
  "description": "Rewrite selected upstream targets before dialing.",
  "artifact_type": "binary",
  "runtime": {
    "type": "go-plugin",
    "entry": "plugin.so",
    "entry_symbol": "Plugin"
  },
  "api_version": "plugin-api/v1",
  "sdk_module": "github.com/tursom/mc-gateway/plugin/api",
  "sdk_module_version": "v0.1.0",
  "gateway_version_constraint": ">=0.1.0",
  "go_version": "go1.24.4",
  "go_os": "linux",
  "go_arch": "amd64",
  "supply_chain": {
    "source": "manual-upload",
    "homepage": "https://example.com/upstream-rewrite",
    "license": "MIT",
    "sbom": "sbom.spdx.json",
    "signature": {
      "type": "none"
    }
  },
  "extension_points": [
    { "type": "hook", "key": "legacy upstream-connect contract" }
  ],
  "features": {
    "required": [
      "extension.upstream.connect.v1",
      "sdk.secret_store.v1"
    ],
    "optional": [
      "sdk.tracer.v1",
      "sdk.plugin_events.v1",
      "sdk.background_tasks.v1",
      "sdk.external_client.v1"
    ]
  },
  "capabilities": {
    "extension_points": ["legacy upstream-connect contract"],
    "network": { "outbound": ["tcp:*:*"] },
    "filesystem": { "read": [], "write": [] },
    "env": []
  },
  "runtime_limits": {
    "handler_timeout_ms": 3000,
    "max_concurrent_calls": 128,
    "max_active_connection_sessions": 1024,
    "failure_threshold": {
      "window_seconds": 60,
      "max_error_rate": 0.2,
      "max_consecutive_panics": 3
    }
  },
  "config_schema": {
    "type": "object",
    "properties": {
      "match_host": { "type": "string" },
      "upstream": { "type": "string" }
    }
  }
}
```

源码包 manifest 示例：

```json
{
  "schema_version": "mc-gateway.plugin/v1",
  "id": "upstream-rewrite",
  "name": "Upstream Rewrite",
  "version": "0.1.0",
  "description": "Rewrite selected upstream targets before dialing.",
  "artifact_type": "source",
  "runtime": {
    "type": "go-plugin",
    "entry": "plugin.so",
    "entry_symbol": "Plugin"
  },
  "build": {
    "type": "go",
    "entry": ".",
    "go_version": "go1.24.4",
    "cgo_enabled": false,
    "tags": [],
    "vendor_required": false,
    "output": "plugin.so"
  },
  "api_version": "plugin-api/v1",
  "sdk_module": "github.com/tursom/mc-gateway/plugin/api",
  "sdk_module_version": "v0.1.0",
  "gateway_version_constraint": ">=0.1.0",
  "supply_chain": {
    "source": "manual-upload",
    "homepage": "https://example.com/upstream-rewrite",
    "license": "MIT",
    "sbom": "sbom.spdx.json",
    "signature": {
      "type": "none"
    }
  },
  "extension_points": [
    { "type": "hook", "key": "legacy upstream-connect contract" }
  ],
  "features": {
    "required": [
      "extension.upstream.connect.v1"
    ],
    "optional": [
      "sdk.tracer.v1",
      "sdk.plugin_events.v1",
      "sdk.background_tasks.v1",
      "sdk.external_client.v1"
    ]
  },
  "capabilities": {
    "extension_points": ["legacy upstream-connect contract"],
    "network": { "outbound": ["tcp:*:*"] },
    "filesystem": { "read": [], "write": [] },
    "env": []
  },
  "runtime_limits": {
    "handler_timeout_ms": 3000,
    "max_concurrent_calls": 128,
    "max_active_connection_sessions": 1024
  },
  "config_schema": {
    "type": "object",
    "properties": {
      "match_host": { "type": "string" },
      "upstream": { "type": "string" }
    }
  }
}
```

二进制包上传后可以直接登记为 artifact。源码包上传后必须先进入 builder，构建出 `plugin.so` 后再登记为 artifact。加载阶段始终只加载最终产物 `plugin.so`；元数据以已校验入库的包内 `manifest.json` 为准。

开发环境可以允许直接上传 raw `.so`，但生产推荐只接受 `.mcgp`。直接上传 `.so` 时，加载前只能展示文件名、大小和 sha256；生产路径仍应使用 `.mcgp` 提供 `manifest.json`。

### 包校验

上传 `.mcgp` 时必须先完成静态校验，不能执行包内任何代码或脚本。

校验项：

- 文件总大小上限，默认 64 MiB，可配置。
- zip entry 数量上限，默认 2048，避免 zip bomb。
- 解压后总大小上限，默认 256 MiB。
- 单文件大小上限，非运行文件默认 16 MiB。
- entry path 必须是相对路径，不能包含 `..`、绝对路径、空路径或平台分隔符绕过。
- 拒绝 symlink、hardlink、device file、FIFO 等特殊文件。
- 必须包含 `manifest.json`，且只能有一个根 manifest。
- `manifest.json` 必须是 UTF-8 JSON，大小受限，例如 256 KiB。
- `artifact_type=binary` 必须包含 runtime entry，例如 `plugin.so`。
- `artifact_type=source` 必须包含 `go.mod` 和 build entry。
- 包内文件 sha256 可以与 `checksums.txt` 比对；第一版可以只记录不强制。
- manifest 中声明的 SBOM、signature、README、LICENSE 路径必须存在时才显示为 available。

上传阶段只生成 upload package sha256。binary artifact 的最终 ID 应以 `plugin.so` sha256 为准；source package 的最终 artifact ID 应以构建产物 `plugin.so` sha256 为准。

## Runtime 与权限声明

`runtime.type` 用于描述插件运行模型。第一版只实现 `go-plugin`，但 manifest 需要提前留出扩展空间。

| Runtime | 说明 | 第一版策略 |
| --- | --- | --- |
| `go-plugin` | Go `-buildmode=plugin` 进程内可信插件 | 实现 |
| `go-plugin-process` | 主进程管理，子进程加载 Go plugin 并处理数据面，用进程退出实现 `.so` 回收 | 未来可选服务启动模式 |
| `sandbox-process` | 独立进程插件，通过 RPC 通信，适合不完全可信或跨语言场景 | 预留 |
| `wasm` | WASM/WASI 插件，适合纯规则、路由和策略逻辑 | 预留 |
| `build-time-instrumentation` | 构建期插桩或代码注入，适合官方高级能力 | 预留 |

`build-time-instrumentation` 不是运行时插件。它的产物应是新的 gateway binary 或经过构建流水线生成的官方 artifact，不能通过管理页热加载，也不能像 `go-plugin` 一样 disable 后立即从进程中移除。它的设计价值在于把“非侵入增强”收敛到可审计构建步骤，而不是允许运行期 monkey patch。

capabilities 用于声明插件期望访问的能力。第一版 native plugin 无法强制隔离这些权限，但仍应记录、展示和审计。未来 sandbox runtime 可以按同一声明做强制限制。

示例：

```json
{
  "capabilities": {
    "extension_points": [
      "legacy upstream-connect contract"
    ],
    "protocols": {
      "ingress_transports": ["tcp", "websocket"],
      "upstream_protocols": ["tcp", "haproxy"],
      "requires_minecraft_branch": true,
      "supports_tcp_admin_port_reuse": true,
      "real_ip_forwarding": ["haproxy-proxy-protocol-v1"]
    },
    "network": {
      "outbound": [
        "tcp:*.example.com:25565",
        "https://sessionserver.mojang.com"
      ]
    },
    "filesystem": {
      "read": [],
      "write": ["plugin-data/"]
    },
    "env": [],
    "admin": {
      "api": false
    }
  }
}
```

capabilities 的使用规则：

- 上传时解析并展示 capabilities，管理员启用前必须能看到。
- native `go-plugin` 第一版只做声明和审计，不承诺运行时强制限制。
- sandbox-process 和 wasm runtime 必须把 capabilities 作为强制权限边界。
- 插件实际注册的 extension point 必须是 manifest 声明的子集。
- capabilities 变化需要重新确认，启用中的插件应标记 `restart_required` 或要求重新启用。
- `protocols.ingress_transports` 表示插件声明支持的客户端入口传输；未声明时默认按 `["tcp"]` 保守处理。
- `protocols.upstream_protocols` 表示插件理解或会主动使用的 backend 连接协议；使用 `custom` 时必须在 README/Runbook 说明实际拨号方式。
- `supports_tcp_admin_port_reuse=false` 时，插件不应在 TCP/Admin 共享入口的 Minecraft 分支全局启用，除非 scope 明确排除复用入口。
- `real_ip_forwarding` 声明插件会写入或依赖的真实 IP 转发方式，避免和 route 的 `haproxy://` 上游重复。

### 功能协商

`capabilities` 表示插件想访问的权限或外部能力；`features` 表示插件依赖的 gateway/SDK 功能。二者必须分开，避免把“能否调用某个 API”和“是否允许访问网络/secret”混为一谈。

manifest 可以声明：

```json
{
  "features": {
    "required": [
      "extension.upstream.connect.v1",
      "sdk.secret_store.v1"
    ],
    "optional": [
      "sdk.tracer.v1",
      "sdk.plugin_events.v1",
      "sdk.background_tasks.v1",
      "sdk.external_client.v1",
      "admin.declarative_ui.v1"
    ]
  }
}
```

feature 命名建议：

| 前缀 | 示例 | 说明 |
| --- | --- | --- |
| `extension.*` | `extension.upstream.connect.v1` | extension point 可用性 |
| `sdk.*` | `sdk.secret_store.v1`、`sdk.plugin_events.v1`、`sdk.external_client.v1` | SDK/Gateway 接口能力 |
| `runtime.*` | `runtime.go_plugin.v1`、`runtime.go_plugin_process.v1`、`runtime.sandbox_process.v1` | runtime adapter 能力 |
| `admin.*` | `admin.declarative_ui.v1`、`admin.actions.v1` | Admin/API 管理能力 |
| `minecraft.*` | `minecraft.protocol_capability.v1` | gateway 理解的 MC 元数据能力，不代表 core 实现登录 |

协商规则：

- `required` feature 缺失时，上传可以成功但 enable 必须阻断，并返回 `feature_missing`。
- `optional` feature 缺失时，插件仍可启用，但 Plugin Manager 应在 `PluginRuntimeInfo` 中标记为 unavailable。
- 插件不能在 runtime 中假设 optional feature 一定存在；调用前必须查询或处理 unavailable 错误。
- feature 名称必须稳定；破坏性变化需要新 feature key，而不是改变旧 key 语义。
- gateway release 应公布 supported features 列表，CLI compat 和 Admin artifact detail 都应展示 required/optional 的满足情况。
- feature 协商不能替代 Go ABI 校验；Go/API/GOOS/GOARCH 不兼容仍然阻断加载。

推荐 SDK 查询接口：

```go
type FeatureSet interface {
    Has(feature string) bool
    Missing(required []string) []string
}

type PluginRuntimeInfo struct {
    PluginID          string
    ArtifactID        string
    Version           string
    GatewayVersion    string
    APIVersion        string
    Features          FeatureSet
    Unavailable       []string
}
```

可选 API 的返回规则：

- 如果 gateway 不支持某个 optional feature，对应 SDK helper 应返回 `api.ErrFeatureUnavailable`。
- 如果插件未在 manifest 中声明某 optional feature，却调用对应 API，gateway 可以允许但应记录 compat warning；未来可收紧为拒绝。
- `ErrFeatureUnavailable` 不应触发插件熔断，除非插件把 optional feature 当作必需业务路径使用并返回普通 error。
- Admin 页面应把 required feature 缺失显示为 blocking，把 optional feature 缺失显示为 warning。

`runtime_limits` 用于声明插件建议的运行时限制。Admin 可以采用默认值，也可以在插件配置中覆盖。实际生效值应展示在插件状态页，并写入审计日志。

可落地的 native plugin 应用层限制：

- handler timeout。
- max concurrent handler calls。
- max active connection takeover connections。
- background task timeout 和不可重入。
- event subscriber queue length。
- log rate 和单条日志大小。
- plugin data 单条大小和总容量。
- 熔断阈值、panic 阈值和自动禁用策略。

native `go-plugin` 不能可靠强制的限制：

- 单个插件 CPU 使用量。
- 单个插件内存使用量。
- 插件创建 goroutine 的总数。
- 插件直接访问进程环境变量、文件系统或网络的能力。
- 插件调用 `os.Exit`、修改全局状态或影响 Go runtime 的能力。

这些强隔离能力需要 `sandbox-process`、container runtime、WASM 或 OS-level sandbox。第一版 UI 必须把 capabilities 标记为“声明和审计”，不要暗示它已经是强制权限边界。

### Runtime Adapter

Plugin Manager 内部应通过 runtime adapter 隔离不同运行时，避免把 Go plugin 的假设写死到所有插件模型里。

推荐抽象：

```go
type RuntimeAdapter interface {
    Load(ctx context.Context, artifact Artifact, plugin PluginRecord, gateway Gateway) (Plugin, error)
}

type RuntimeAdapterLifecycle interface {
    ValidateArtifact(ctx context.Context, artifact Artifact) error
    Prepare(ctx context.Context, artifact Artifact, plugin PluginRecord) (PreparedRuntime, error)
    Start(ctx context.Context, prepared PreparedRuntime, artifact Artifact, plugin PluginRecord, gateway Gateway) (RuntimeInstance, error)
    HealthCheck(ctx context.Context, instance RuntimeInstance) RuntimeHealth
    ReloadConfig(ctx context.Context, instance RuntimeInstance, configJSON string) error
    Drain(ctx context.Context, instance RuntimeInstance) error
    Stop(ctx context.Context, instance RuntimeInstance) error
    Diagnostics(ctx context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics
}
```

不同 runtime 的能力边界：

| Runtime | 可以支持 | 不适合支持 |
| --- | --- | --- |
| `go-plugin` | typed Go API、返回 `net.Conn`、connection takeover、低改造成本 | 不可信插件、强资源隔离、跨语言 |
| `go-plugin-process` | Go 插件数据面隔离、进程级卸载、可选 fd/shm 连接迁移 | 跨语言 ABI、强不可信隔离、跨平台 fd 迁移 |
| `sandbox-process` | 强隔离、跨语言、可重启、可回收资源 | 直接返回进程内 `net.Conn`、低延迟 hot path |
| `wasm` | 规则、路由、配置校验、轻量策略 | 长连接 protocol proxy、任意网络访问、复杂 Go SDK |
| `build-time-instrumentation` | 官方插桩、静态增强、生成代码 | 动态热加载、普通用户插件 |

runtime adapter 设计规则：

- extension point 声明必须标注支持哪些 runtime。
- `legacy upstream-connect contract` connection takeover mode 支持 `go-plugin` in-process 和 `go-plugin-process` drain-only stream bridge。
- `go-plugin-process` 需要新的服务启动模式，不能在运行中从单进程无缝切换；管理后台可以保存 desired mode，并提示重启后生效。
- sandbox-process 如果要实现协议代理，需要改成进程间 stream relay，而不是返回 Go `net.Conn`。
- wasm 适合 `route.resolve/v1`、rule/policy engine 和配置校验，不作为第一版连接代理方案。
- Admin 页面必须展示 runtime type 和该 runtime 下 capabilities 是否强制执行。
- 同一个 `.mcgp` manifest 可以声明 runtime type，但不能在一个 artifact 中混用多个 runtime。

当前代码状态：`in-process + go-plugin` 已通过 `RuntimeAdapterLifecycle` 运行；`go-plugin-process` 已有 `plugin-host` 同 binary 子命令、`mc-gateway-plugin-host/v1` handshake、UDS control channel、supervisor start/stop foundation、host 内 Init/ReloadConfig/Destroy lifecycle、loaded host crash summary refresh、persisted configurable restart backoff/max/window policy、crash-loop auto-isolation、per-node crash isolation、service-level last error persistence、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket handshake orphan cleanup、Linux `/proc` process-table orphan discovery、`legacy upstream-connect contract` route.resolve/v1 provider 的跨进程 UDS relay，以及 connection takeover drain-only stream bridge。fd/live 迁移、sandbox enforcement、完整不可信隔离和非 Linux process-table orphan discovery 仍是未来工作。

### Go Plugin Process Runtime，部分实现和未来能力

`go-plugin-process` 是 `go-plugin` 的多进程运行形态。它的目标不是跨语言或运行不可信插件，而是把 Go plugin 的 `.so` 加载放到可退出的子进程中，让主进程只负责管理、listener、生命周期、fd 编排和观测聚合。这样可以通过退出子进程实现进程级卸载，绕开 Go runtime 在同一进程内不能 unload plugin 的限制。

基本模型：

```text
gateway main process
  -> owns listeners / Admin / SQLite / desired state
  -> starts plugin-host process
  -> asks plugin-host to create upstream dialer connection
  -> later passes accepted connection fd or stream endpoint for connection takeover
  -> supervises health, drain, restart and migration

plugin-host process
  -> plugin.Open(plugin.so)
  -> owns plugin instance and Go globals
  -> runs upstream dialer data plane first
  -> later runs connection takeover data plane through a stream/fd protocol
  -> exits to release loaded .so and Go heap
```

服务启动模式应作为 gateway 级配置，而不是单个插件随意选择。管理后台可以提供：

| Mode | 说明 | 默认策略 |
| --- | --- | --- |
| `in-process` | 主进程直接 `plugin.Open`，使用 `go-plugin` runtime | 第一版默认，性能最好，不能真正热卸载 |
| `go-plugin-process` | 主进程不加载 `.so`，为 Go 插件启动 plugin-host 子进程 | 部分可用：`legacy upstream-connect contract` route.resolve/v1 provider 和 connection takeover drain-only；live migration 仍是未来能力 |
| `sandbox-process` | 独立进程或容器运行跨语言/隔离插件 | 未来能力，和 `go-plugin-process` 分开 |

管理后台应提供一个 gateway 级系统配置项，用来决定服务下一次启动时采用哪种插件服务模式。该配置不是插件 manifest 的一部分，也不能由单个插件在运行时覆盖。

建议配置模型：

```json
{
  "plugin_service": {
    "desired_mode": "go-plugin-process",
    "active_mode": "in-process",
    "restart_required": true,
    "connection_migration": {
      "default_mode": "drain-only",
      "allow_fd_live": false,
      "allow_fd_live_shm": false
    },
    "updated_by": "admin",
    "updated_at": "2026-06-26T00:00:00Z",
    "applied_at": null
  }
}
```

字段含义：

- `desired_mode` 是管理员希望 gateway 下一次启动采用的模式。
- `active_mode` 是当前进程实际启动时采用的模式，只能由 gateway 启动流程写入。
- `restart_required` 由 `desired_mode != active_mode` 或迁移能力配置变化推导，不应由用户手工编辑。
- `connection_migration.default_mode` 决定 `go-plugin-process` 下默认卸载/升级策略，初始必须是 `drain-only`。
- `allow_fd_live` 和 `allow_fd_live_shm` 是环境级开关；即使插件 manifest 声明支持 live migration，环境未开启时也不能使用。
- `updated_by`、`updated_at`、`applied_at` 用于审计和排障。

启动收敛流程：

1. gateway 启动时从 SQLite/Admin 系统配置读取 `plugin_service.desired_mode`。
2. 校验当前平台、feature gate、二进制能力和环境策略是否支持该模式。
3. 校验通过后创建对应 RuntimeAdapter 或 plugin-host supervisor。
4. 成功进入服务状态后写入 `active_mode`、`applied_at` 和 runtime capability 摘要。
5. 如果 desired mode 不可用，生产环境应启动失败并给出明确错误；开发环境可以允许显式 fallback 到 `in-process`，但必须写审计和健康告警。

配置规则：

- 该配置影响 gateway 服务启动方式，必须在 Admin 中明确标为 restart required。
- 从 `in-process` 切到 `go-plugin-process` 需要重启 gateway 后生效；不承诺运行中迁移 runtime mode。
- 只有 gateway 启动流程可以把 `desired_mode` 提升为 `active_mode`；Admin 修改配置只保存 desired state。
- `go-plugin-process` 仍然只运行可信 Go 插件；它提供进程级资源回收，不等于完整沙箱。
- `go-plugin-process` 可以逐步支持 CPU/memory/cgroup 限制，但权限模型仍弱于专门的 sandbox-process。
- Admin 应展示当前 active mode、desired mode、是否需要重启、plugin-host 状态、子进程 PID、crash loop、fd 迁移能力和最近迁移结果。
- promotion bundle 可以携带 desired service mode，但导入目标环境时必须按环境策略确认，不能自动切换生产 gateway 的启动模式。

卸载和升级流程分两档：

| 迁移模式 | 行为 | 适用 |
| --- | --- | --- |
| `drain-only` | 主进程停止给旧 plugin-host 分配新连接，旧连接自然结束或管理员 force close，随后退出旧子进程 | 默认，简单可靠 |
| `fd-live` | 旧 plugin-host 在安全点把 client/backend fd 交回主进程，主进程启动新 plugin-host 后转交 fd | 透明转发、简单 dialer |
| `fd-live-shm` | 在 `fd-live` 基础上，用共享内存迁移用户态 buffer 和插件状态快照 | 高性能 connection takeover，高复杂度 |

fd 交接规则：

- 本机 Unix 优先使用 Unix domain socket + `SCM_RIGHTS` 传递 fd。
- fd 只表示内核 socket，不包含 Go `net.Conn` 包装、deadline、用户态 buffer、parser 状态、加密/压缩状态或业务状态。
- QUIC stream、KCP、WebSocket、TLS wrapper、`net.Pipe` 和自定义 conn 不一定能抽象为可传递 fd；这些场景需要 stream relay 或禁止 live migration。
- fd 迁移必须定义所有权：同一时刻只能有一个进程读写该 fd；交接期间必须暂停读写并确认 ack。
- 迁移失败时按策略 fallback：继续旧进程 drain、关闭连接或回滚到旧 artifact。

共享内存规则：

- 共享内存只用于迁移稳定格式的数据，不能共享 Go pointer、map、chan、interface、goroutine 或 `net.Conn`。
- 可放入共享内存的数据包括 pending read/write buffer、ring buffer、packet parser offset、协议 phase、trace/connection metadata、deadline、业务状态快照和校验和。
- connection takeover 插件必须显式实现 `quiesce/snapshot/restore` 契约，并声明 state schema version。
- MC 协议迁移应优先发生在安全点，例如 packet boundary、压缩帧边界、登录完成后或插件声明的可恢复阶段。
- encryption/compression/profile cache/forwarding 状态只有在插件能稳定序列化时才能迁移；否则必须 drain-only。

示例 manifest：

```json
{
  "runtime": {
    "type": "go-plugin-process",
    "entry": "plugin.so",
    "connection_migration": {
      "mode": "fd-live-shm",
      "safe_point": "packet_boundary",
      "state_schema_version": 1
    }
  },
  "features": {
    "required": [
      "runtime.go_plugin_process.v1",
      "runtime.connection_migration.fd_live_shm.v1"
    ]
  }
}
```

SDK 可以预留迁移接口：

```go
type MigratableConnection interface {
    Quiesce(ctx context.Context, connectionID string) (MigrationCheckpoint, error)
    Snapshot(ctx context.Context, checkpoint MigrationCheckpoint, w MigrationStateWriter) error
    Restore(ctx context.Context, snapshot MigrationStateReader, fds MigratedFDSet) error
}
```

设计边界：

- `go-plugin-process` 是进程级 hot unload，不是 Go plugin 原生 unload。
- `drain-only` 应作为默认迁移模式；`fd-live` 和 `fd-live-shm` 只对显式声明并通过 conformance 的插件开放。
- live migration 的复杂度主要在插件业务状态，不在 fd 传递本身。
- 第一版不实现该 runtime，但 Admin/API/manifest 可以预留 service mode、runtime type 和迁移能力字段，避免未来破坏数据模型。

### Build-Time Instrumentation，未来能力

非侵入式 Go 代码注入方案对本设计有参考价值，但应定位为未来的官方构建期能力。其典型做法是替换或包装 `go build`，在编译阶段读取规则，对目标函数的入口、出口或错误返回路径插入 trampoline/hook 调用，再生成最终二进制。它适合“统一治理”和“官方增强”，不适合开放给普通用户作为运行时插件机制。

对阿里云非侵入 Go 注入思路的判断：

- 有参考价值的是“规则化注入点 + 构建期织入 + trampoline/hook 统一入口”这个模型，尤其适合观测、安全和治理类横切能力。
- 不应直接照搬为普通插件系统，因为它改变的是最终 binary，无法满足管理页上传、热加载、启用/禁用和按插件 lifecycle 收敛的要求。
- 它可以作为未来官方发行版或企业内部分发版的构建增强能力，和 `.mcgp` runtime plugin 并行存在。
- 如果它暴露给用户，也应以受控源码构建 profile 或组织 CI policy 形式出现，而不是让上传包在 gateway 节点任意改写源码。

可借鉴点：

- 使用声明式规则描述注入点，例如 package、receiver、function、enter/exit/error path。
- 在构建期生成或改写代码，使业务代码无需手工调用 SDK。
- 通过统一 hook/trampoline 收敛观测、限流、审计、panic 保护和指标采集。
- 将规则、生成产物、构建器版本、Go 版本、源码 sha256 和生成代码 diff 纳入 provenance。
- 在 CI/staging 运行 conformance、benchmark 和回归测试后再发布。

适合场景：

- 官方发行版默认观测插桩，例如 handler latency、panic recover、slow path trace。
- 安全审计插桩，例如 secret 读取、外部依赖调用、危险 action 执行。
- 统一治理插桩，例如 timeout、并发限制、熔断和指标上报。
- 对还没有稳定 extension point 的内部路径做临时实验性观测。
- 生成样板代码，例如 extension point registration、manifest metadata、schema glue。

不适合场景：

- 用户上传后热加载的业务插件。
- MC 正版/三方登录 connection takeover；这类能力仍应走 `legacy upstream-connect contract` 或未来 stream proxy。
- 修改 gateway 内部业务语义，例如改路由表写入、绕过权限、改 Admin session 签发。
- 在生产节点上临时织入代码并直接替换运行中进程。
- 绕过公开 API/ABI 访问内部未文档化结构。

治理要求：

- 只允许官方或组织受信构建流水线启用，默认不接受普通用户 `.mcgp` 携带插桩规则直接生效。
- 插桩规则必须版本化，作为源代码或 release artifact 的一部分接受 review。
- 构建结果必须记录 instrumentation manifest、builder identity、source sha256、generated diff hash、Go version 和 SDK/API version。
- 插桩后的 gateway binary 必须重新跑单元测试、conformance、benchmark 和协议 smoke test。
- 当前实现登记 instrumentation metadata 时，`available` 状态必须携带 `generated_diff_hash`、`conformance.ok=true`、`benchmark.ok=true` 和 `smoke.ok=true`；失败或未通过的 release evidence 只能以 `blocked` 状态保留，不能作为可发布元数据。
- 管理页只能展示当前 binary 的 instrumentation metadata 和能力声明；不能把它当作可 enable/disable 的插件。
- 如果插桩影响连接路径、Admin 权限、secret 或外部依赖，必须在 release notes 和 Runbook 中说明回滚方式。

与插件系统的关系：

- build-time instrumentation 可以为 extension point 自动补充观测和保护，但不能改变 extension point 契约。
- 如果一个注入点逐渐稳定，应优先抽象成正式 hook/middleware/provider，再让普通插件使用。
- 如果一个能力必须可热加载、可禁用、可灰度，应实现为 runtime plugin，而不是插桩。
- 插桩产物属于 gateway binary 供应链，不属于 `plugin_artifacts` 的运行时加载模型。

### Sandbox Runtime，未来能力

`sandbox-process` 和 `wasm` 的目标不是替代第一版 `go-plugin` 主路径，而是在后续支持更强隔离、跨语言和可回收运行时资源。它们必须作为新的 runtime adapter 接入，不应改变已发布的 `go-plugin` ABI。

设计目标：

- 插件崩溃不导致 gateway 进程崩溃。
- 可以按插件重启、停止和回收资源。
- capabilities 从声明和审计升级为可强制权限边界。
- 支持非 Go 语言或不同 Go 版本构建的插件。
- 允许组织运行不完全可信但经过准入的插件。

非目标：

- 第一版不实现 sandbox runtime。
- 不保证 sandbox-process 能达到进程内 Go plugin 的 hot path 延迟。
- 不让跨进程插件直接返回 gateway 进程内的 `net.Conn`。
- 不把 WASM 用作完整 Minecraft connection takeover 的默认方案。

#### Sandbox Process 模型

`sandbox-process` 建议由 gateway 启动或托管一个插件子进程：

```text
gateway
  -> RuntimeAdapter(sandbox-process)
  -> plugin supervisor
  -> plugin process
  -> control RPC
  -> optional stream relay
```

控制面 RPC 负责：

- handshake：插件 ID、version、API version、runtime protocol version。
- init：传入 config、capabilities、runtime limits 和 secret handles。
- register：返回 extension point、handler ID 和 handler metadata。
- reload config。
- health check。
- metrics scrape。
- shutdown/drain。

数据面按 extension point 分级：

| Extension Point 类型 | sandbox-process 支持方式 |
| --- | --- |
| request/response hook | RPC 调用，传递结构化 request/response |
| provider | RPC 调用，gateway 管理超时、熔断和 fallback |
| event subscriber | 异步队列 + RPC 投递，允许丢弃策略 |
| middleware | 只适合结构化、有限 payload 的场景 |
| connection takeover | 需要 stream relay，不使用 `net.Conn` 返回值 |

stream relay 候选方案：

| 方案 | 优点 | 缺点 |
| --- | --- | --- |
| Unix domain socket | 本机低开销、权限可控、适合 Linux | 跨平台和容器网络需要额外处理 |
| 本地 TCP | 实现简单、跨平台 | 端口管理和本机访问控制更复杂 |
| gRPC streaming | 协议统一、便于跨语言 | 长连接字节流开销较高，背压和半关闭语义复杂 |
| socket passing | 接近原生连接语义 | 实现复杂，跨平台和语言支持差 |

如果 sandbox-process 支持 connection takeover，应定义新的 extension point 版本，例如 `legacy upstream-connect contract` 或 `stream.proxy/v1`：

```go
type StreamProxyRequest struct {
    ConnectionID    string
    TraceID         string
    SourceAddr      string
    ServerHost      string
    RawServerHost   string
    ProtocolVersion int
    NextState       int
    InitialData     []byte
    RouteID         string
    RouteTags       []string
}

type StreamProxyResult struct {
    Decision   string // handle/pass/reject
    StreamID   string
    RejectCode string
    Metadata   map[string]string
}
```

gateway 负责把 client connection 与 sandbox stream relay 连接起来，并执行：

- 初始 handshake bytes 回放。
- 双向复制和背压。
- deadline、idle timeout 和最大连接时长。
- half-close 语义映射；不支持时退化为 Close。
- 字节数、耗时、错误摘要和 active stream 指标。
- 插件进程退出时关闭相关 stream。

插件进程负责：

- 在返回 `StreamID` 前准备好 relay 读端。
- 处理协议状态机、后端连接、认证、forwarding 和关闭。
- 遵守 gateway 传入的 runtime limits。
- 通过 control RPC 上报健康、指标和低基数错误。

#### Sandbox 权限强制

sandbox-process 的 capabilities 应尽量映射到 OS 或容器隔离能力：

| Capability | 强制方式 |
| --- | --- |
| filesystem read/write | chroot、mount namespace、容器 volume、只读根文件系统 |
| outbound network | egress proxy、防火墙规则、network namespace、sidecar policy |
| env | 白名单环境变量 |
| secret | gateway 发放短期 secret handle 或按需 RPC，不注入全量环境变量 |
| CPU/memory | cgroup、container limits、进程 supervisor |
| process | 禁止额外 fork/exec 或通过 sandbox profile 限制 |

权限强制失败时不应降级为“只审计”。如果 runtime 声明为 `sandbox-process`，但当前部署无法强制某项必需 capability，应阻断启用并展示明确原因。

secret 传递原则：

- 默认不把 secret 明文放入环境变量。
- 优先由 gateway 提供 Secret RPC：插件按 secret name 请求，gateway 检查授权、审计并返回短期值。
- 对长期连接需要的 secret，插件可以缓存，但必须支持 reload/rotation。
- secret RPC 日志和指标不记录 secret value。

#### WASM 模型

`wasm` runtime 适合纯计算或轻量策略插件：

- route resolve。
- host rewrite。
- allow/deny policy。
- config validate。
- 简单 event transform。
- admin-side schema helper。

WASM 第一阶段不适合：

- 完整 Minecraft 登录代理。
- 长连接双向 stream proxy。
- 任意 TCP/UDP 网络访问。
- 大量内存或复杂 goroutine 模型。
- 直接复用 Go SDK 中依赖 `net.Conn` 的接口。

WASM host API 应小而稳定：

- 读取 request 结构。
- 返回 decision/result。
- 读取有限 config。
- 读取授权 secret，默认不支持。
- 记录结构化日志。
- 导出低基数 metrics。

WASM 的 capabilities 可以更容易强制：默认无文件、无网络、无环境变量、有限内存和执行时间。需要网络能力时应优先通过 gateway provider/RPC 代理，而不是开放 WASI 任意 socket。

#### 运行时选择和迁移

同一个插件能力是否能从 `go-plugin` 迁移到 sandbox，取决于 extension point 是否有跨进程语义：

| 能力 | `go-plugin` | `sandbox-process` | `wasm` |
| --- | --- | --- | --- |
| upstream dialer | 直接返回 `net.Conn` | 可通过 gateway-owned dialer provider 或 stream relay | 不建议 |
| connection takeover | 第一版主路径 | 需要 `stream.proxy/v1` | 不建议 |
| route resolve | 可支持 | 适合 | 适合 |
| event sink | 可支持 | 适合 | 适合轻量 transform |
| rule/policy | 可支持 | 可支持 | 最适合 |
| auth provider | 可支持 | 可支持，注意 secret 和 timeout | 仅适合纯校验或远程调用代理 |

迁移原则：

- 不把 `legacy upstream-connect contract` 的 `(net.Conn, error)` 强行映射到 sandbox-process。
- 新增跨进程 extension point version，而不是改变 v1 语义。
- manifest 可以声明同一插件源码支持多个 runtime，但每个 artifact 只能有一个 runtime type。
- Admin 应展示相同插件在不同 runtime 下的能力差异、性能预算和权限强制状态。
- promotion bundle 导入时必须检查目标环境是否支持 artifact 声明的 runtime。

#### Supervisor 和故障处理

sandbox-process 需要 supervisor：

- 启动进程并建立 control channel。
- 心跳和健康检查。
- 限制重启频率，避免 crash loop。
- 收集 stdout/stderr 摘要并脱敏。
- 进程退出时标记相关 plugin runtime state 为 failed/degraded。
- 对正在处理的 stream 执行关闭或 fallback 策略。

故障策略：

| 场景 | 默认策略 |
| --- | --- |
| 插件进程启动失败 | 启用失败，旧 dispatch table 不变 |
| control RPC 超时 | 当前调用失败并计入熔断 |
| 插件进程 crash | 移除 handler，连接失败或 fallback，按策略重启 |
| stream relay 断开 | 关闭对应 client/backend 连接 |
| crash loop | 自动隔离插件并要求管理员处理 |

#### 兼容性和打包

sandbox-process artifact 需要额外 metadata：

```json
{
  "runtime": {
    "type": "sandbox-process",
    "entry": "bin/plugin",
    "protocol": "mc-gateway-sandbox-rpc/v1",
    "os": "linux",
    "arch": "amd64"
  }
}
```

WASM artifact 示例：

```json
{
  "runtime": {
    "type": "wasm",
    "entry": "plugin.wasm",
    "abi": "mc-gateway-wasm/v1",
    "wasi": false
  }
}
```

包校验需要扩展：

- `sandbox-process` entry 必须是普通文件，不能是脚本链到包外路径。
- `wasm` entry 必须通过 WASM module 校验。
- runtime protocol/ABI version 必须在 gateway 支持范围内。
- capabilities 必须能被目标 runtime 强制；不能强制时阻断启用。

### 依赖声明

插件可以声明对 gateway、runtime、extension point 或其他插件的依赖。

示例：

```json
{
  "dependencies": {
    "gateway": ">=0.1.0",
    "plugin_api": "plugin-api/v1",
    "extension_points": [
      "legacy upstream-connect contract"
    ],
    "plugins": [
      { "id": "official-rule-engine", "version": ">=0.1.0", "optional": true }
    ]
  }
}
```

依赖规则：

- 上传时做基础校验。
- 启用前检查必需依赖是否存在且启用。
- 插件依赖变更需要重新确认。
- 禁用或删除被依赖插件时，管理页必须提示受影响插件。
- 依赖不能形成循环；检测到循环时启用失败。

### 外部依赖声明和治理

`dependencies` 描述插件对 gateway、runtime、extension point 或其他插件的依赖；外部服务依赖应单独声明。登录插件、动态路由插件、审计 sink、告警插件和仓库插件通常会访问 session server、CMDB、配置中心、HTTP API、消息队列或对象存储。若这些依赖只隐藏在代码里，Admin 无法做发布门禁、健康判断、超时治理和数据合规提示。

manifest 可以增加 `external_dependencies`：

```json
{
  "external_dependencies": [
    {
      "id": "mojang-session",
      "type": "http",
      "purpose": "auth_session_verify",
      "endpoints": ["https://sessionserver.mojang.com"],
      "required": true,
      "timeout_ms": 1500,
      "max_concurrent": 64,
      "retry": {
        "max_attempts": 1,
        "backoff_ms": 100
      },
      "circuit_breaker": {
        "window_seconds": 60,
        "max_error_rate": 0.2
      },
      "fail_policy": "fail_closed",
      "secret_refs": [],
      "data_classes": ["player_name", "player_uuid", "source_ip_hash"],
      "health_check": {
        "enabled": true,
        "interval_seconds": 30,
        "timeout_ms": 1000
      }
    }
  ]
}
```

字段规则：

| 字段 | 说明 |
| --- | --- |
| `id` | 插件内稳定 ID，用于日志、指标、健康和诊断 |
| `type` | `http`、`tcp`、`grpc`、`database`、`message_queue`、`object_storage`、`custom` |
| `purpose` | 用途，例如 auth、route、audit、metrics、repository、entitlement |
| `endpoints` | 目标地址或域名；可支持环境覆盖 |
| `required` | 缺失或不可用时是否阻断启用 |
| `timeout_ms` | 单次调用超时预算 |
| `max_concurrent` | 对该外部依赖的并发上限 |
| `retry` | 重试次数和 backoff；连接路径默认不建议多次重试 |
| `circuit_breaker` | 外部依赖级熔断配置 |
| `fail_policy` | `fail_open`、`fail_closed`、`degraded`、`fallback` |
| `secret_refs` | 访问该依赖需要的 secret 引用 |
| `data_classes` | 会发送到外部系统的数据类型 |
| `health_check` | 是否进入插件健康探测 |

治理规则：

- external dependency 必须和 capabilities.network.outbound 对齐；声明了 endpoint 但 capabilities 未声明网络访问时，上传或启用应提示不一致。
- native `go-plugin` 无法强制阻止访问未声明 endpoint，但 Admin 必须展示声明和风险；sandbox runtime 必须强制 egress 策略。
- required dependency 缺失 secret、endpoint 无效或 health check not_ready 时应阻断启用，除非插件显式声明 fail open 且管理员确认。
- 连接路径上的外部调用必须有 timeout 和并发上限。
- 默认不建议在连接路径做多次重试；重试会放大延迟和外部服务压力。
- 外部依赖熔断应独立于插件整体熔断，避免一个 session server 故障导致所有非相关 handler 降级。
- dependency 状态变化应影响插件 HealthStatus，并写入低基数指标。
- 官方插件和示例插件应优先通过 gateway 提供的 external dependency client 访问外部服务，而不是自行创建无治理的 `http.Client` 或 `net.Dialer`。

### 受控外部调用 Client

`external_dependencies` 只解决“声明和门禁”，还需要 SDK 提供受控调用入口，才能把 timeout、并发、retry、熔断、trace、metrics 和脱敏规则落到实际调用上。登录插件访问 Mojang session server、三方 Yggdrasil-like endpoint、会员或风控 API 时，应由插件自己决定何时调用、如何解释响应以及如何返回 Minecraft 协议结果；gateway 只提供受控调用能力，不消费认证结果。

SDK 可以提供：

```go
type ExternalClient interface {
    DoHTTP(ctx context.Context, dependencyID string, req *http.Request, opts ExternalRequestOptions) (*http.Response, error)
    DialContext(ctx context.Context, dependencyID string, network string, address string) (net.Conn, error)
    Status(ctx context.Context, dependencyID string) (ExternalDependencyStatus, error)
}

type ExternalRequestOptions struct {
    Operation   string
    DataClasses []string
    Timeout     time.Duration
    Idempotent  bool
}

type ExternalDependencyStatus struct {
    ID           string
    State        string // ready/degraded/not_ready
    CircuitState string // closed/open/half_open
    LastErrorKind string
    LastCheckedAt time.Time
}
```

接口规则：

- `dependencyID` 必须存在于 manifest `external_dependencies`；未声明时返回 `api.ErrExternalDependencyNotDeclared`。
- 请求 URL、host、scheme、network 和 address 必须匹配 dependency `endpoints`；不匹配时返回 `api.ErrExternalDependencyEndpointDenied`。
- 实际 timeout 取 `ctx deadline`、manifest `timeout_ms` 和 `opts.Timeout` 中最严格者。
- 并发上限按 dependency ID 计算，超过时返回 `api.ErrExternalDependencyBusy` 或按 fail policy 处理。
- retry 只在 manifest 允许且请求可判定为幂等时执行；连接路径默认不做多次重试。
- 熔断打开时返回 `api.ErrExternalDependencyCircuitOpen`，并写入 dependency 级指标和 trace span。
- response body 默认不记录；诊断只保存 status code class、error kind、duration、dependency ID 和 operation。
- `DataClasses` 必须是 manifest 声明的子集；超出声明时返回校验错误或记录 policy warning。
- HTTP header 中的 secret 应由插件通过 `SecretStore` 获取并自行设置，gateway 只负责脱敏和不记录；未来可增加受控 header 注入 helper。
- 默认不向第三方外部依赖注入 W3C `traceparent`；只有 manifest 或环境策略允许时才注入。
- native `go-plugin` 仍可绕过该 client 自行访问网络，因此第一版把它作为官方插件约束、conformance 要求和观测治理入口；sandbox-process 或 sidecar egress runtime 才能强制所有外联经过该 client。

错误码建议：

| 错误 | 说明 |
| --- | --- |
| `api.ErrExternalDependencyNotDeclared` | dependency ID 未声明 |
| `api.ErrExternalDependencyEndpointDenied` | 请求目标不在声明 endpoint 范围内 |
| `api.ErrExternalDependencyTimeout` | 单次调用超时 |
| `api.ErrExternalDependencyBusy` | dependency 并发或队列已满 |
| `api.ErrExternalDependencyCircuitOpen` | dependency 熔断打开 |
| `api.ErrExternalDependencyUnavailable` | health not_ready 或底层网络不可达 |

MC 登录插件使用方式：

- `mojang-session` 调用 `https://sessionserver.mojang.com` 的 session 校验。
- `third-party-yggdrasil` 调用三方 session endpoint 和 profile endpoint。
- `entitlement-api` 调用会员、白名单、权限或风控 API。
- 插件根据返回内容自行决定登录成功、kick reason、身份映射、forwarding 和后续协议处理。
- 插件通过业务事件和自定义指标上报 `auth.success`、`auth.failure`、`session_timeout` 等结果；gateway 不消费这些事件来改变连接结果。

失败策略：

| 策略 | 适合场景 | 行为 |
| --- | --- | --- |
| `fail_closed` | 安全、认证、权限、付费校验 | 拒绝或断开当前请求 |
| `fail_open` | 观测、非关键告警、可选审计 | 跳过依赖并继续主流程 |
| `degraded` | 可降级能力，例如辅助 profile enrich | 标记 degraded，返回基础结果 |
| `fallback` | 多 endpoint 或缓存可用 | 使用备用 endpoint、缓存或默认值 |

数据和隐私：

- `data_classes` 必须说明会发往外部系统的数据，例如 player_name、uuid、source_ip、host、profile_properties、session_hash。
- 管理页应展示外部数据传输提示，帮助管理员做合规评估。
- 指标标签不能包含 endpoint 完整 URL 中的 token、玩家名或高基数字段。
- 诊断包不能包含外部 API token、完整 response 或玩家隐私原文。

健康检查：

- 插件整体 HealthCheck 可以聚合 external dependency 状态。
- Plugin Manager 可以提供外部依赖状态展示，但不直接代替插件业务探测。
- health check 应使用轻量 endpoint 或 HEAD/ping，不应触发真实写操作。
- health check 失败摘要应包含 dependency ID、error kind 和最近时间，不包含 secret 或完整 response。

环境迁移：

- endpoint、fail policy、timeout、max_concurrent 和 secret refs 都可能随环境变化。
- promotion import 应展示 external dependency diff，并要求目标环境确认 endpoint 和 secret mapping。
- 离线或内网环境可以将 dependency 标记为 unavailable，插件必须有明确 fail policy 才能启用。

### 组合约束声明

除了依赖，插件还需要声明它和其他插件的组合约束。依赖描述“需要谁”，组合约束描述“不能和谁同时处理同一范围”或“必须排在谁前后”。

manifest 可以增加 `composition`：

```json
{
  "composition": {
    "exclusive_extension_points": [
      {
        "key": "legacy upstream-connect contract",
        "mode": "connection takeover",
        "reason": "only one protocol proxy can own the stream"
      }
    ],
    "conflicts_with": [
      {
        "plugin_id": "official.mc-auth-proxy",
        "scope": "overlap",
        "reason": "two auth proxies cannot both own login flow"
      }
    ],
    "before": ["official.upstream-rewrite"],
    "after": ["official-rule-engine"],
    "provides": [
      { "key": "auth.provider/v1", "name": "yggdrasil-session" }
    ],
    "consumes": [
      { "key": "auth.provider/v1", "optional": true }
    ]
  }
}
```

组合约束规则：

- `exclusive_extension_points` 表示同一 scope 内只能有一个插件实际处理该 extension point。
- `conflicts_with` 可以按 plugin ID、capability、extension point 或 provider name 声明冲突。
- `before`/`after` 只能影响同一 extension point 内的排序；不能跨 extension point 制造隐式依赖。
- `provides`/`consumes` 用于 provider 类能力，必须和 dependencies 一起校验。
- 组合约束变化需要重新 review；已启用插件应标记需要重新启用或重新发布 dispatch table。

scope 重叠检查：

- host 完全相同或通配覆盖视为重叠。
- route ID 相同视为重叠。
- route tag 相交视为可能重叠。
- source CIDR 有交集视为重叠。
- protocol version 集合相交视为重叠。
- transport、service name 或 upstream protocol 集合相交视为重叠。
- 空 scope 视为全局匹配，和任何 scope 重叠。

静态分析不能证明不重叠时，应返回 `potential_conflict`，由管理员确认或收窄 scope。connection takeover、认证、packet filter 和 provider 单例类冲突默认应阻断，而不是只提示。

### Secret 引用

插件配置中不能直接保存密钥明文。登录插件、外部 API 插件和 forwarding 插件应通过 secret reference 获取敏感值。

manifest 可以声明需要的 secret：

```json
{
  "secrets": [
    {
      "name": "velocity_forwarding_secret",
      "description": "Velocity modern forwarding shared secret",
      "required": true,
      "type": "shared_secret",
      "rotation": {
        "strategy": "dual_read",
        "grace_period": "10m",
        "reload": "hot"
      }
    },
    {
      "name": "external_api_token",
      "description": "Token for external entitlement API",
      "required": false,
      "type": "api_token",
      "rotation": {
        "strategy": "replace",
        "reload": "reload_required"
      }
    }
  ]
}
```

`config_json` 只保存引用：

```json
{
  "forwarding_secret_ref": "velocity_forwarding_secret"
}
```

secret 规则：

- secret 不写入 manifest、构建日志、审计日志明文或普通 `config_json`。
- Admin 页面只显示 secret 是否已配置、最后更新时间和引用关系。
- 插件只能读取自己声明并被管理员授权的 secret。
- secret 更新后，相关插件按 manifest `rotation.reload` 执行 hot reload、reload required、restart required 或 manual。
- 删除插件时，管理页应询问是否删除该插件专属 secret。

secret 类型建议：

| 类型 | 示例 | 处理规则 |
| --- | --- | --- |
| `shared_secret` | Velocity forwarding secret、Webhook secret | 支持双读或替换轮换 |
| `api_token` | 外部会员、风控、告警 API token | 默认 replace，日志严格脱敏 |
| `private_key` | mTLS client key、签名 key | 默认 reload/restart required，不能进入诊断 |
| `public_key` | 三方 Yggdrasil-like 公钥 | 可 hot reload |
| `certificate` | CA bundle、client certificate | 支持证书到期提示和 reload |

rotation 策略：

| 策略 | 说明 | 适合场景 |
| --- | --- | --- |
| `replace` | 新值替换旧值，插件 reload 后只读新值 | API token、公钥 |
| `dual_read` | grace period 内新旧值都可验证，写出使用新值 | forwarding secret、签名校验 |
| `dual_write` | grace period 内向外部系统同时写新旧凭据或双通道 | 少见，需插件显式支持 |
| `manual` | 只记录需要人工处理，不自动 reload | 后端也必须同步修改的 secret |

reload 策略：

| 策略 | 行为 |
| --- | --- |
| `hot` | Plugin Manager 调用插件 secret reload 接口，不切换 artifact |
| `reload_required` | 标记 pending reload，管理员确认后执行 reload |
| `restart_required` | native plugin 已加载状态无法安全更新时提示重启 |
| `manual` | 只提示 Runbook，不自动调用插件 |

secret rotation 不应直接改变已有 connection takeover 连接的协议状态。对长连接，默认只影响新连接；插件如果支持 per-connection key refresh，必须在 manifest 中声明。

## 供应链元数据

第一版不做第三方插件市场、强制签名和自动升级，但 `.mcgp` 格式应预留供应链信息，方便后续引入可信分发。

推荐包内可选文件：

```text
plugin.mcgp
  manifest.json
  plugin.so 或源码文件
  README.md
  LICENSE
  sbom.spdx.json
  checksums.txt
  signature.sig
```

manifest `supply_chain` 字段用于记录：

- 来源：manual upload、GitHub release、内部仓库、官方仓库。
- 作者和主页。
- 许可证。
- 源码仓库和 commit。
- SBOM 文件路径。
- 签名类型和签名文件。
- 漏洞扫描结果摘要。

第一版处理策略：

- 上传时解析并展示许可证、来源、SBOM 是否存在、签名状态。
- 如果签名存在，可以记录但不强制验证。
- 如果 SBOM 存在，可以保存并展示依赖摘要。
- 如果 README、Runbook 或数据处理声明存在，应作为 artifact 文档元数据保存并在 Admin 展示。
- 不阻断未知来源插件，但管理页明确提示 native 插件是可信代码。

### 插件文档和支持元数据

插件包不应只有代码和 manifest。管理员在启用可信 native plugin 前，需要知道插件做什么、访问什么数据、出问题如何回滚、由谁维护。文档元数据应进入 manifest，而不是只依赖 README 自由文本。

manifest 可以增加 `documentation`：

```json
{
  "documentation": {
    "readme": "README.md",
    "license_file": "LICENSE",
    "changelog": "CHANGELOG.md",
    "runbook": "docs/RUNBOOK.md",
    "support": {
      "owner": "platform-team",
      "contact": "mc-gateway@example.com",
      "issue_url": "https://example.com/issues/mc-auth-proxy",
      "severity": "production"
    },
    "data_handling": {
      "sends_data_external": true,
      "external_data_classes": ["player_name_hash", "uuid_hash", "source_ip_prefix", "host_hash"],
      "retention_summary": "auth events kept for 7 days in external SIEM",
      "exportable_data": ["route_cache"],
      "privacy_notes": "player identifiers are hashed before event export"
    },
    "operations": {
      "rollback": "disable plugin or rollback to previous artifact; existing connection takeover connections drain",
      "secret_rotation": "rotate velocity_forwarding_secret with dual-read grace period",
      "known_failure_modes": ["session_timeout", "backend_dial_failed", "forwarding_failed"]
    },
    "compatibility": {
      "minecraft_versions": "1.19.4-1.21.1 by protocol number",
      "backend_requirements": ["Velocity modern forwarding enabled"],
      "go_plugin_notes": "requires exact Go toolchain match"
    },
    "risk_notes": [
      "Uses package-level cache for public key material only",
      "Does not support hot unload because runtime.type is go-plugin"
    ]
  }
}
```

README 最低要求：

- 插件用途、能力边界和 extension point。
- 配置字段、默认值、reload/restart 影响。
- secret 引用和轮换方式。
- external dependencies、endpoint、fail policy 和发送的数据类型。
- 运行期文件、plugin_data、缓存和数据保留策略。
- 日志、事件、指标、trace 的隐私处理。
- 常见故障、排障步骤、禁用和回滚方式。
- 兼容性范围，例如 Go/SDK 版本、Minecraft protocol version、backend 要求。
- 升级、降级和破坏性变更说明。

文档门禁：

- 开发模式可以缺 README，但进入 `review_required` 或 `restricted` 策略时，缺 README 至少是 `warning`。
- connection takeover、访问 secret、声明 external dependencies、写入 plugin_data、导出事件到外部系统或启用 packet rewrite 的插件，缺 README 或 data handling 声明应进入 `review_required`。
- `restricted` 模式可以配置为缺 README、LICENSE、Runbook、support contact 或 data handling 声明时阻断 enable。
- 文档声明与 manifest 结构化字段冲突时，以结构化字段为准，并把冲突列为发布门禁 warning。
- 文档更新属于 artifact 变化，需要重新生成 artifact sha256 和 policy evaluation。

Admin 展示规则：

- README/Runbook 只按安全 Markdown 子集渲染，不执行 HTML、JavaScript、远程图片或 iframe。
- 内部仓库 URL、support contact 和 issue URL 按权限展示。
- 数据处理声明应和 `external_dependencies.data_classes`、plugin_data schema、event/metric schema 做交叉检查。
- 诊断包可以包含 README、Runbook、manifest 和脱敏策略结果，方便离线排障。

### SBOM 和 License 治理

SBOM 和 license 不是第一版必须阻断启用的前提，但需要从一开始作为可机器读取的治理输入，否则后续无法可靠做安全公告匹配和跨环境 promotion 风险评估。

推荐规则：

- `supply_chain.license` 使用 SPDX license expression；未知时写 `NOASSERTION`，不能省略。
- 包内 `LICENSE` 文件用于人工查看，manifest 中的 license 字段用于策略判断。
- SBOM 第一版优先支持 SPDX JSON；CycloneDX 可以作为未来能力。
- SBOM 中依赖建议包含 package URL、module path、version、checksum 和 license。
- 源码包构建时，builder 应记录 `go list -m -json all`、`go version -m` 和最终 binary build info，用于生成或校验 SBOM 摘要。
- 二进制包自带 SBOM 时，上传阶段保存摘要；缺失时可以从 `go version -m` 提取弱依赖线索，但不能等同完整 SBOM。

license policy 建议支持：

| 策略项 | 说明 |
| --- | --- |
| `allowed` | 允许的 SPDX 表达式或 license ID |
| `denied` | 阻断的 license ID，例如组织不接受的 copyleft license |
| `review_required` | 需要审批但不默认阻断的 license |
| `allow_unknown` | 是否允许 `NOASSERTION` 或无法识别的 license |
| `apply_to_transitive` | 是否检查 SBOM 中的传递依赖 license |

SBOM 风险处理：

- 缺失 SBOM 在 `permissive` 模式只提示，在 `review_required` 模式要求确认，在 `restricted` 模式可以阻断高风险插件。
- SBOM dependency 命中安全公告时，按 advisory policy 进入 notify、review、block、quarantine 或 revoke。
- SBOM 解析失败不应静默忽略；需要进入 policy result，并在 Admin 展示解析错误。
- 管理员对 license 或 SBOM 风险的 override 必须有原因、有效期、适用范围和审计日志。
- promotion import 需要展示源环境与目标环境 license/advisory 策略差异，避免同一个 artifact 在生产被新策略阻断时才暴露问题。

未来增强：

- 支持官方信任根和组织内信任根。
- 支持签名验证，例如 cosign/minisign/gpg 之一。
- 支持 SBOM 漏洞扫描。
- 支持许可证策略，例如禁止未知许可证或 copyleft 许可证。
- 支持组织级 allowlist/denylist。
- 支持从插件仓库导入 artifact。

## 插件准入、审批和撤销策略

trusted plugin 不能只靠口头约定。第一版即使不强制签名，也需要把“谁可以进入生产、谁确认风险、发现问题如何撤销”设计成可审计流程。

### 准入策略模型

准入策略是组织级配置，不属于单个插件 manifest。默认可以宽松，生产环境可以收紧：

```json
{
  "mode": "permissive",
  "profile": "dev",
  "allowed_sources": ["manual-upload", "internal", "official"],
  "denied_plugin_ids": [],
  "denied_artifact_sha256": [],
  "allowed_plugin_prefixes": [],
  "required_review_for": [
    "connection takeover",
    "capabilities_changed",
    "unknown_source",
    "missing_sbom",
    "unsigned",
    "major_upgrade"
  ],
  "blocked_capabilities": [],
  "license_policy": {
    "allow_unknown": true,
    "allowed": [],
    "denied": [],
    "review_required": [],
    "apply_to_transitive": false
  },
  "documentation_policy": {
    "require_readme_for_risky_plugin": true,
    "require_runbook_for_takeover": true,
    "require_data_handling_for_external_data": true
  },
  "override_policy": {
    "max_ttl_seconds": 604800,
    "require_reason": true,
    "require_second_approver_for_critical": false
  }
}
```

策略模式：

| 模式 | 说明 |
| --- | --- |
| `permissive` | 只提示风险，不阻断上传或启用；适合开发环境 |
| `review_required` | 高风险项必须管理员确认；适合 staging |
| `restricted` | 未满足组织策略时阻断启用；适合生产 |

策略可以按环境 profile 分层：

| Profile | 默认倾向 | 典型用途 |
| --- | --- | --- |
| `dev` | `permissive`，允许 raw `.so` 和缺文档 warning | 本地开发、快速验证 |
| `staging` | `review_required`，要求 README、preflight、smoke 和风险确认 | 上线前验证 |
| `prod` | `restricted`，高风险缺口阻断，override 有 TTL 和审计 | 生产流量 |

策略判断输入：

- plugin ID、version、artifact sha256。
- source、repository、author、homepage、commit。
- signature、SBOM、license、provenance、README/Runbook/data handling 元数据。
- runtime type、artifact type、Go/API/GOOS/GOARCH。
- capabilities、extension points、runtime limits。
- 是否 connection takeover、是否 source package、是否 major upgrade。
- 是否命中 organization allowlist/denylist。

第一版支持策略评估、本地/导入式漏洞库和治理展示，不强制接入外部漏洞库或签名验证服务。即便签名不强制，artifact sha256 和来源仍必须记录并参与审计。

策略落库和快照：

- admission policy 应有 `policy_id`、`version`、`profile`、`updated_by`、`updated_at`。
- 每次 policy evaluation 都记录 policy snapshot hash，审批和 rollback 只能引用当时的 snapshot。
- 策略变更后，不应立即重写历史 review；新的 enable、rollback、repository import admission preview、repository import apply、promotion apply 和 artifact switch 必须使用当前策略重新评估。
- 策略从宽松切到严格时，已启用插件先标记 `policy_drift` 或 `review_required`，是否自动隔离由组织策略决定。
- policy API 返回机器可读 reason code，例如 `missing_readme`、`license_denied`、`sbom_parse_failed`、`capability_blocked`。

warning override：

- override 必须绑定 artifact sha256、config hash、scope hash、risk reason 和 policy snapshot。
- override 必须有操作者、原因、创建时间、过期时间和审计事件。
- override 到期后不能继续启用、回滚、repository import apply 或 promotion apply；已启用插件进入 `review_required` 或按策略隔离。
- critical 风险默认不允许 override；如果组织开启，必须明确写入 `override_policy`。
- override 不允许绕过 Go/API/ABI 不兼容、denylist/revoke、缺失必需 secret 或 sandbox 必需权限无法强制的问题。

### 风险分级

启用前应把风险分为可解释等级：

| 等级 | 示例 | 默认动作 |
| --- | --- | --- |
| `info` | 有 SBOM、license 明确、capabilities 未变化 | 展示 |
| `warning` | 未签名、未知 license、source package 构建、minor upgrade | 要求确认 |
| `high` | connection takeover、secret ref 变化、scope 扩大、capabilities 变化 | 二次确认，可要求审批 |
| `critical` | artifact sha256 被 denylist 命中、Go/API 不兼容、缺失必需 secret | 阻断启用 |

风险评级应写入发布门禁结果，并在 repository import admission preview、promotion import、artifact switch、enable 和 rollback 时重新计算。rollback 也不能绕过 denylist；如果旧 artifact 已被撤销，只允许管理员执行隔离恢复流程，不应重新接入真实流量。

### 审批流程

审批不是第一版必须实现的复杂工作流，但数据模型和 API 应预留：

- 上传 artifact 后进入 `uploaded` 或 `staged` 状态。
- 发布门禁生成 review item。
- 管理员确认风险后记录 reviewer、时间、策略快照和确认理由。
- 高风险插件可以要求双人审批，未来能力。
- 审批只对 artifact sha256、plugin config hash、scope hash 和 runtime limits hash 有效；任一项变化都需要重新审批。

审批记录不保存 secret 明文，只保存 secret ref 是否变化。

建议表：

```sql
CREATE TABLE plugin_reviews (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    config_hash TEXT NOT NULL DEFAULT '',
    scope_hash TEXT NOT NULL DEFAULT '',
    rollout_hash TEXT NOT NULL DEFAULT '',
    runtime_limits_hash TEXT NOT NULL DEFAULT '',
    risk_level TEXT NOT NULL,
    policy_snapshot_json TEXT NOT NULL DEFAULT '{}',
    decision TEXT NOT NULL,              -- approved/rejected/expired
    reason TEXT NOT NULL DEFAULT '',
    reviewed_by TEXT NOT NULL DEFAULT '',
    reviewed_at INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0
);
```

安全公告和匹配结果建议单独保存，避免每次列表查询都重新扫描 SBOM：

```sql
CREATE TABLE plugin_security_advisories (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL,
    title TEXT NOT NULL,
    advisory_json TEXT NOT NULL,
    default_action TEXT NOT NULL DEFAULT 'notify',
    active INTEGER NOT NULL DEFAULT 1,
    published_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    imported_at INTEGER NOT NULL,
    imported_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE plugin_advisory_matches (
    advisory_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    match_type TEXT NOT NULL,            -- sha256/version/sbom/source/unknown
    action TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open', -- open/acknowledged/mitigated/ignored
    reason TEXT NOT NULL DEFAULT '',
    matched_at INTEGER NOT NULL,
    acknowledged_by TEXT NOT NULL DEFAULT '',
    acknowledged_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (advisory_id, artifact_id)
);
```

match status 规则：

- `open` 表示仍需处理。
- `acknowledged` 表示管理员已知晓但风险未消除。
- `mitigated` 表示已升级、禁用、隔离或轮换 secret。
- `ignored` 只允许在 advisory policy `allow_override=true` 时使用，并需要有效期和原因。

### Allowlist、Denylist 和隔离

组织级 allowlist/denylist 可以作用于：

- plugin ID。
- artifact sha256。
- source repository。
- signer identity，未来能力。
- license。
- extension point 或 capability。

denylist 命中时：

- 未启用插件：阻断 load/enable。
- 已启用插件：进入 `quarantined` 或 `disabled_by_policy` 状态，新连接不再调用该插件。
- connection takeover 插件已有连接进入 draining；管理员可以强制关闭。
- 管理页展示撤销原因、影响范围、是否需要重启和回滚建议。
- 写入审计日志。

allowlist 不应单独代表安全。allowlist 只说明来源被组织接受，仍然需要版本、sha256、Go/API、capabilities 和配置门禁。

### 紧急撤销

发现插件恶意、被篡改、泄露 secret、导致大面积故障时，需要一条比普通禁用更强的撤销路径：

1. 管理员将 artifact sha256 或 plugin ID 加入 denylist。
2. Plugin Manager 立即从 dispatch table 移除相关 handler。
3. 新连接不再进入该插件。
4. 对 connection takeover 插件，按策略 drain 或 force close 现有连接。
5. 标记相关 plugin secrets 为 rotation required。
6. 阻止 rollback 到被撤销 artifact。
7. 在 promotion/drift/report 中标记该 artifact 已撤销。
8. 写入 `plugin_policy_revoke` 审计事件。

如果 Go plugin 代码已经加载到进程中，撤销不能从内存真正卸载代码。UI 必须提示需要重启才能彻底移除已加载 native code；重启前只能保证 dispatch table 不再调用它。

### 安全公告和漏洞响应

denylist 适合处理明确要阻断的 artifact，但供应链治理还需要表达“某个版本有漏洞、建议升级、是否必须隔离”的安全公告。公告可以来自官方仓库、组织内部仓库、本地导入文件或管理员手工创建。

公告模型：

```json
{
  "id": "MCGSA-2026-0001",
  "source": "official",
  "severity": "critical",
  "title": "mc-auth-proxy forwarding secret disclosure",
  "published_at": 1782400000,
  "updated_at": 1782403600,
  "affected": [
    {
      "plugin_id": "official.mc-auth-proxy",
      "version_range": "<1.2.3",
      "artifact_sha256": [],
      "dependency": {
        "purl": "pkg:golang/example.com/authlib",
        "version_range": "<0.4.5"
      }
    }
  ],
  "fixed_versions": [
    {
      "plugin_id": "official.mc-auth-proxy",
      "version": "1.2.3",
      "artifact_sha256": "sha256:..."
    }
  ],
  "policy": {
    "default_action": "quarantine",
    "allow_override": false,
    "requires_secret_rotation": true,
    "requires_restart": true
  },
  "references": [
    "https://example.com/advisories/MCGSA-2026-0001"
  ]
}
```

匹配规则：

- 优先按 artifact sha256 精确匹配。
- 其次按 plugin ID + SemVer range 匹配。
- 如果有 SBOM，可按 package URL、module path 和 dependency version 匹配。
- source package 可以按 source sha256、repository URL、commit 和 module list 匹配。
- 无法确认是否受影响时标记 `unknown_affected`，进入 warning 或 review required。

公告动作：

| Action | 行为 |
| --- | --- |
| `notify` | 只展示告警和升级建议 |
| `review_required` | 下次 enable、rollback、repository import apply、promotion apply 需要审批 |
| `block_new_enable` | 阻断新启用和回滚，但不影响当前已启用实例 |
| `quarantine` | 已启用插件从 dispatch table 移除，新流量不再进入 |
| `revoke` | 等价紧急撤销，加入 denylist 并提示重启 |

响应规则：

- policy evaluation 必须把 active advisories 纳入风险评级。
- rollback 不能回到受 `block_new_enable`、`quarantine` 或 `revoke` 影响的 artifact。
- promotion import/apply 必须展示源环境和目标环境 advisory 差异。
- fixed version 可用时，Admin 页面应展示升级路径和 compat 风险。
- advisory 要求 secret rotation 时，相关 plugin secret 标记 rotation required。
- advisory 要求 restart 时，已加载 native plugin 必须提示重启才能彻底清理。
- 管理员 override 只能用于 `notify` 或允许 override 的 `review_required`，必须填写原因、有效期并写审计日志。

第一版支持本地 advisory JSON 导入/手工创建，本地/外部 advisory feed sync，以及本地/导入式/外部 vulnerability 数据库按 SBOM dependency rescan 并接入 enable/rollback gate。feed scheduler 只在部署方显式配置时运行；官方/内部仓库同步和完整外部 CVE/SBOM 自动漏洞扫描链作为后续增强。

### 策略和运行时的边界

准入策略不能替代 sandbox：

- 它不能限制已加载 native plugin 访问进程权限。
- 它不能保证源码包没有恶意逻辑。
- 它不能修复 Go plugin 无法热卸载的问题。

它能提供的是：生产前阻断、风险确认、可审计决策、紧急下线、secret 轮换提示和跨环境传播风险提示。

## 插件仓库和版本策略

第一版只要求手动上传 `.mcgp`。后续可以在不改变 artifact 模型的前提下增加插件仓库。

### 仓库模型

插件仓库只负责发现和下载，不直接启用插件：

```text
repository
  -> plugin index
  -> artifact download
  -> local plugin_artifacts
  -> admin review
  -> load / enable
```

仓库类型：

| 类型 | 说明 |
| --- | --- |
| official | 项目维护的官方插件仓库 |
| internal | 组织内部插件仓库 |
| file | 离线目录或本地文件仓库 |
| url | 指定 URL 下载 `.mcgp` |

仓库 index 至少包含：

- plugin ID
- name
- version
- artifact URL
- sha256
- runtime type
- api version
- Go version / GOOS / GOARCH
- capabilities 摘要
- 准入状态：pending、approved、rejected、blocked、quarantined
- risk level
- license
- signature metadata

仓库 index 建议使用稳定 JSON schema，便于 official/internal/file/url 仓库共用同一实现：

```json
{
  "schema_version": 1,
  "repository": {
    "id": "official",
    "name": "MC Gateway Official Plugins",
    "generated_at": 1782400000,
    "base_url": "https://plugins.example.com/"
  },
  "plugins": [
    {
      "id": "official.mc-auth-proxy",
      "name": "Minecraft Auth Proxy",
      "summary": "Protocol proxy for official and third-party Yggdrasil login",
      "versions": [
        {
          "version": "1.2.0",
          "channel": "stable",
          "artifact_type": "binary",
          "runtime": { "type": "go-plugin" },
          "api_version": "v1",
          "gateway_version_constraint": ">=0.3.0 <0.5.0",
          "go_version": "go1.24.4",
          "go_os": "linux",
          "go_arch": "amd64",
          "sha256": "sha256:...",
          "size_bytes": 1048576,
          "download_url": "artifacts/official.mc-auth-proxy/1.2.0/linux-amd64/plugin.mcgp",
          "manifest_url": "artifacts/official.mc-auth-proxy/1.2.0/manifest.json",
          "signature": {
            "type": "none"
          },
          "sbom_url": "artifacts/official.mc-auth-proxy/1.2.0/sbom.spdx.json",
          "license": "Apache-2.0",
          "created_at": 1782400000
        }
      ]
    }
  ]
}
```

index 规则：

- `schema_version` 必须向后兼容；破坏性变化需要新 URL 或新 schema version。
- `download_url` 可以是相对 `base_url` 的路径，也可以是绝对 URL；导入前必须规范化并展示最终来源。
- `sha256` 必须是 artifact 包 sha256；下载后不匹配必须拒绝导入。
- index 中的 manifest、license、SBOM 和 signature 只作为候选元数据；本地导入后仍以 `.mcgp` 内实际文件为准。
- 同一个 repository 下同一 plugin/version/platform 不应出现多个不同 sha256；如果出现，标记为 repository conflict。
- `channel` 只用于展示和筛选，例如 `stable`、`beta`、`dev`；不能代替本地 review。
- `gateway_version_constraint`、Go 版本和平台不匹配时，候选版本应显示为不可导入或导入后不可启用。

仓库信任策略：

```json
{
  "trust_policy": {
    "allowed_plugin_prefixes": ["official.", "com.example."],
    "allowed_channels": ["stable", "beta"],
    "require_signature": false,
    "allowed_signers": [],
    "allowed_licenses": ["Apache-2.0", "MIT"],
    "blocked_plugin_ids": [],
    "blocked_sha256": []
  }
}
```

信任策略规则：

- 仓库信任策略只决定候选版本是否允许导入为本地 artifact。
- 导入后的 artifact 仍必须走本地 manifest 校验、准入策略、review、load 和 enable。
- 仓库 index 被篡改时，sha256 校验应阻止错误 artifact 落库；如果仓库同时篡改 sha256，只能依赖签名、组织 allowlist 和人工 review。
- `require_signature=true` 时，导入阶段必须验证签名；验证失败不能创建 `plugin_artifacts`。
- 仓库删除某个版本不应自动删除本地 artifact；本地 GC 由管理员策略控制。
- 自动检查更新可以提示可用版本，但第一版不自动下载、不自动切换、不自动启用。

### 版本策略

插件版本使用 SemVer。版本策略：

- patch 版本默认可兼容。
- minor 版本需要重新校验 manifest、capabilities 和 config schema。
- major 版本默认视为可能不兼容，需要管理员确认。
- `plugin_api` version 不兼容时禁止启用。
- Go version、GOOS、GOARCH 不匹配时禁止加载。

更新策略：

| 策略 | 说明 |
| --- | --- |
| manual | 只提示新版本，不自动下载 |
| download-only | 自动下载 artifact，但不切换 |
| staged | 下载并构建，等待管理员手动切换 |
| automatic | 自动切换，第一版不建议支持 |

第一版建议只做 `manual`，最多预留 `download-only`。

### 离线环境

离线环境需要支持：

- 手动上传 `.mcgp`。
- 从本地目录导入 artifact。
- 使用 vendor 源码包构建。
- 禁止公网 GOPROXY。
- 不依赖在线签名验证服务。

## Manifest 元数据

插件包的元数据只来自 `.mcgp` 根目录的 canonical `manifest.json`。上传、准入、构建、兼容性检查和 Admin 展示都必须使用这份静态 manifest；gateway 不通过执行插件代码读取元数据。源码目录可以使用 YAML、TOML、JSONC 或 JSON 作为唯一 manifest source，但进入 `.mcgp` 前必须规范化为 `manifest.json`。

Go plugin 只需要导出一个 factory 符号：

```go
// Plugin 是 SDK API，用于创建插件实例。
func Plugin() api.Plugin
```

`Plugin` 使用 `func() api.Plugin`。只有当以下静态校验通过后，gateway 才会 `plugin.Open` 并断言调用它：

- manifest schema 版本受支持。
- `go_os`、`go_arch` 与当前 gateway 一致。
- `go_version` 默认要求与 gateway 构建 Go 版本精确一致。
- `api_version` 在 gateway 支持范围内。
- `sdk_module` 与 gateway 使用的 API module path 一致。
- `sdk_module_version` 在允许范围内。
- manifest `features.required` 都被当前 gateway 支持。
- `abi_fingerprint` 与当前 gateway 支持的 ABI fingerprint 匹配，或处于明确允许的兼容集合。
- sha256 与上传记录一致。

Go 插件实际能否加载仍以 `plugin.Open` 为准。即使 manifest 看起来兼容，Go runtime 仍可能因为依赖包版本、构建标签或 toolchain 差异拒绝加载。

### Go Plugin ABI Fingerprint

Go `plugin` 的真实兼容性不只取决于 `go_version`。同一 Go 版本下，如果 `plugin/api` 或共享依赖的 module 版本、build tags、CGO、GOAMD64 等构建设置不同，也可能在 `plugin.Open` 时失败。第一版应引入 `abi_fingerprint`，把兼容性从“看几个字段”升级为可解释的构建身份。

manifest 建议增加：

```json
{
  "abi": {
    "type": "go-plugin/v1",
    "fingerprint": "sha256:...",
    "go_version": "go1.24.4",
    "toolchain": "go1.24.4",
    "go_os": "linux",
    "go_arch": "amd64",
    "go_amd64": "v1",
    "cgo_enabled": false,
    "build_tags": [],
    "trimpath": true,
    "sdk_module": "github.com/tursom/mc-gateway/plugin/api",
    "sdk_module_version": "v0.1.0",
    "shared_modules": [
      {
        "path": "github.com/tursom/mc-gateway/plugin/api",
        "version": "v0.1.0",
        "sum": "h1:..."
      }
    ],
    "build_settings": {
      "-buildmode": "plugin",
      "-mod": "vendor"
    }
  }
}
```

fingerprint 输入建议：

| 字段 | 说明 |
| --- | --- |
| `go_version/toolchain` | `runtime.Version()` 和 builder toolchain |
| `GOOS/GOARCH/GOAMD64/GOARM64` | 目标平台和微架构级别 |
| `CGO_ENABLED` | 是否启用 CGO；第一版默认禁用 |
| `build_tags` | 影响编译条件的 tag，排序后进入 hash |
| `plugin/api` module | SDK module path、version、sum |
| shared modules | gateway 与插件都可能加载的共享 module，至少包含 plugin API 及其直接依赖 |
| build settings | `-buildmode=plugin`、`-trimpath`、`-mod` 等关键设置 |

fingerprint 规则：

- 使用 canonical JSON 计算 sha256。
- `build_tags`、`shared_modules` 按稳定排序。
- source package 构建产物的 fingerprint 由 builder 生成，不信任源码包 manifest 自报。
- binary package 上传时可以读取 manifest 和 `go version -m` 交叉校验；不一致时进入 blocking。
- gateway release 应公布自身支持的 `abi_fingerprint` 或兼容集合。
- 如果 fingerprint 不匹配，默认阻断 load；开发模式可以允许管理员 override，但仍以 `plugin.Open` 结果为准。

兼容检查层级：

| 层级 | 失败处理 |
| --- | --- |
| manifest 字段缺失或 schema 错误 | 上传失败 |
| Go/API/GOOS/GOARCH 明确不兼容 | load/enable 阻断 |
| `abi_fingerprint` 不匹配 | 默认阻断，开发模式可 override |
| `plugin.Open` 失败 | runtime_failed，记录 Go runtime 错误摘要 |

Admin 和 CLI 应展示 fingerprint diff，例如 Go patch 版本、CGO、build tag、SDK module 或 shared module 哪一项不同，而不是只显示“ABI 不兼容”。

### 命名规范

插件 ID、handler ID、task ID 和 extension point key 必须稳定、可读、可排序。

推荐规则：

| 名称 | 规则 | 示例 |
| --- | --- | --- |
| plugin ID | 小写字母、数字、点、短横线；建议反域名或组织前缀 | `official.upstream-rewrite`、`com.example.mc-auth` |
| handler ID | 插件内唯一，小写字母、数字、点、短横线 | `default`、`auth.proxy` |
| background task ID | 插件内唯一 | `sync-routes`、`refresh-cache` |
| secret name | 小写字母、数字、下划线、短横线 | `velocity_forwarding_secret` |
| extension point key | `<domain>.<action>/v<version>` | `legacy upstream-connect contract` |

规则：

- plugin ID 一旦发布不应变更。
- 同一 plugin ID 的不同版本必须表示同一插件的兼容演进。
- 官方插件建议使用 `official.*` 前缀。
- 第三方或内部插件建议使用组织域名前缀，避免命名冲突。
- 文件路径不能直接信任 plugin ID，落盘前仍要做路径安全处理。
- handler ID、task ID 只在插件内部唯一，但日志和指标中会与 plugin ID 组合使用。

### API 兼容和废弃策略

兼容性对象包括：

- manifest `schema_version`。
- plugin API version。
- extension point key/version。
- request/response struct。
- config schema version。
- runtime adapter contract。

兼容规则：

- `schema_version` 不兼容时拒绝上传。
- plugin API 不兼容时拒绝加载或启用。
- extension point 新增字段只能追加，不能改变已有字段含义。
- handler 函数签名变化必须发布新的 extension point version。
- request struct 新字段必须有零值语义。
- response 新枚举值必须有默认处理策略。
- 新增 optional SDK/API 能力应分配 feature key，并允许旧插件忽略。
- required feature 缺失必须阻断 enable，不应等到 runtime panic 或 nil interface。
- 废弃 extension point 至少保留一个 minor release 的兼容期。
- 管理页应提示插件使用 deprecated API，但不应立即阻断，除非该 API 已被移除。
- 示例插件和测试 harness 必须覆盖当前推荐 API。

插件降级：

- 切换到旧 artifact 前必须检查旧 artifact 是否支持当前 config_version。
- 如果旧 artifact 不支持当前配置，需要选择历史配置快照或手动编辑。
- plugin data schema 如果由插件自行管理，降级风险由插件文档说明。

## SDK/API 契约治理

插件系统一旦允许用户编写插件，`plugin/api`、manifest schema 和 extension point request/response 就成为平台契约。契约治理的目标是：gateway、SDK、示例插件和文档一起演进，避免某次 gateway 改动静默破坏已发布插件。

### 契约资产

需要把以下内容视为契约资产：

- `manifest.json` schema。
- `plugin/api` public Go interface。
- extension point key、version、调用模式、request/response 字段和错误语义。
- config schema UI hint。
- runtime adapter contract。
- feature key 和功能协商规则。
- Admin API 中和插件开发/诊断相关的稳定错误码。
- CLI 的 manifest validate、package、inspect、compat 输出字段。

每个契约资产都应有版本和变更规则。文档、SDK 代码、测试 fixture 和示例插件必须引用同一套版本，不能各自维护含义不同的副本。

### 契约文件

建议维护机器可读契约文件，作为测试和文档生成的来源：

```text
plugin/contracts/
  manifest.schema.json
  extension-points/
    upstream.connect.v1.json
    status.ping.v1.json
  errors.json
  features.json
  config-ui-hints.json
```

`extension-points/upstream.connect.v1.json` 至少描述：

- key 和 version。
- type：hook/middleware/provider/event。
- mode：first-match/all/chain/async。
- 支持 runtime：`go-plugin`、未来 `sandbox-process`、`wasm`。
- request 字段名、类型、是否可选、隐私等级和零值语义。
- response 或 error 语义。
- 超时、并发、dry-run、fallback 默认策略。
- 是否允许 connection takeover。

`features.json` 至少描述：

- feature key。
- 所属范围：extension、sdk、runtime、admin、minecraft。
- 首次支持的 gateway/API 版本。
- 是否可选。
- 缺失时的 compat 行为。
- 对应的 SDK interface/helper 或 Admin API。

契约文件不是替代 Go 类型，而是用于：

- 生成文档。
- 驱动 manifest validate。
- 驱动 conformance 测试。
- 检查字段只追加不破坏。
- 为 Admin 页面展示 extension point 帮助信息。

### Conformance Suite

需要提供 gateway conformance suite，用来证明当前 gateway 仍支持已承诺的插件契约。它和普通单元测试不同，测试对象是公开契约而不是内部实现。

conformance suite 应覆盖：

- manifest schema 向后兼容。
- manifest schema 读取和错误处理。
- Go/API/GOOS/GOARCH 兼容错误码。
- required/optional feature 协商和 `ErrFeatureUnavailable`。
- extension point 注册和未声明 extension point 拒绝。
- handler ordering、priority、ErrPass、ErrBlocked 和 panic recover。
- `legacy upstream-connect contract` request 字段、initial data 只读语义和 connection takeover 初始回放。
- config schema 校验、config migration 和 `ReloadConfig()` dry-run。
- SecretStore 授权、缺失 secret、轮换和脱敏。
- Admin API 稳定错误码。
- CLI validate/inspect/compat 输出的必要字段。

conformance suite 应包含两类 fixture：

| Fixture | 用途 |
| --- | --- |
| source fixture | 验证源码包构建、SDK 编译和示例代码 |
| binary fixture | 验证已构建 `.so` 或 `.mcgp` 在目标 Go 版本下的 ABI 行为 |

由于 Go plugin ABI 对 Go 版本敏感，binary fixture 必须按 gateway release 的 Go 版本构建。不同 Go 版本的 fixture 不应混用。

### 示例插件作为契约测试

示例插件不只是文档材料，也应作为契约测试：

- `upstream-rewrite` 是最小 route.resolve/v1 provider fixture。
- `mc-auth-proxy` 是 connection takeover 能力上限 fixture。
- 未来 `mc-status-motd` 是结构化 Minecraft status extension fixture。
- rule/policy 示例用于验证 config schema UI hint 和 rule engine 边界。

每个示例插件应具备：

- source `.mcgp` 打包。
- binary `.mcgp` 打包。
- manifest validate。
- compat check。
- unit/harness test。
- 至少一个 Admin API 或 CLI 加载/启用 dry-run 测试。

示例插件如果不能随 gateway 当前 main 分支构建，应视为插件 API 回归，而不是普通文档损坏。

### 兼容性检查流程

gateway 或 SDK 发布前需要执行：

1. 生成或校验契约文件。
2. 对比上一 release 的契约文件。
3. 检查所有变更是否符合兼容规则。
4. 构建示例插件 source package。
5. 使用当前 release builder 生成 binary `.mcgp`。
6. 运行 conformance suite。
7. 运行 Admin API 错误码和 CLI 输出 golden test。
8. 生成兼容性报告。

兼容性报告应列出：

- 新增 extension point。
- deprecated API。
- removed API。
- request/response 字段变化。
- manifest schema 变化。
- config schema hint 变化。
- SDK public symbol 变化。
- 需要插件作者行动的事项。

如果检测到不兼容变更但没有新的 major version、extension point version 或迁移说明，发布应被阻断。

### SDK 发布节奏

建议把 gateway release 和 plugin SDK release 绑定：

- gateway 发布时记录支持的 `plugin_api` 版本范围。
- SDK module 使用 SemVer。
- patch 只修复 bug，不改变 public interface。
- minor 可以新增 interface、helper、字段和 extension point，但不能改变已有语义。
- major 才允许破坏性调整。
- gateway 至少支持当前 minor 和上一个 minor 的 SDK 插件，除非 Go ABI 或安全撤销阻断。

SDK 发布物：

- Go module tag。
- `manifest.schema.json`。
- extension point contract files。
- CLI 兼容性检查工具。
- 示例插件 source。
- release note 和迁移指南。

### 弃用和移除

弃用流程：

1. 在契约文件中标记 deprecated。
2. 在 SDK Go doc 中标记 Deprecated。
3. 管理页和 CLI compat 显示告警。
4. 示例插件迁移到新 API。
5. 至少保留一个 minor release 的兼容窗口。
6. 移除时必须升级 major 或 extension point version，并提供迁移说明。

禁止行为：

- 在同一个 extension point version 中改变字段含义。
- 删除 request 字段或改变字段类型。
- 改变 `ErrPass`、`ErrBlocked` 等公共错误语义。
- 改变 first-match/all/chain 调用模式。
- 改变默认 fail open/fail closed 策略而不发布新 version。

## 数据模型

插件持久化分为 artifact 和 plugin 两层。

Artifact 表示一次上传的二进制产物，同一个插件可以有多个版本 artifact。

```sql
CREATE TABLE plugin_artifacts (
    id TEXT PRIMARY KEY,                 -- sha256
    plugin_id TEXT NOT NULL,
    version TEXT NOT NULL,
    file_name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    size_bytes INTEGER NOT NULL,
    artifact_type TEXT NOT NULL DEFAULT 'binary',
    source_sha256 TEXT NOT NULL DEFAULT '',
    build_id TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    supply_chain_json TEXT NOT NULL DEFAULT '{}',
    documentation_json TEXT NOT NULL DEFAULT '{}',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    api_version TEXT NOT NULL DEFAULT '',
    sdk_module_version TEXT NOT NULL DEFAULT '',
    abi_fingerprint TEXT NOT NULL DEFAULT '',
    abi_json TEXT NOT NULL DEFAULT '{}',
    go_version TEXT NOT NULL DEFAULT '',
    go_os TEXT NOT NULL DEFAULT '',
    go_arch TEXT NOT NULL DEFAULT '',
    license TEXT NOT NULL DEFAULT '',
    repository_id TEXT NOT NULL DEFAULT '',
    repository_url TEXT NOT NULL DEFAULT '',
    uploaded_by TEXT NOT NULL DEFAULT '',
    admission_status TEXT NOT NULL DEFAULT 'pending',
    risk_level TEXT NOT NULL DEFAULT 'unknown',
    policy_result_json TEXT NOT NULL DEFAULT '{}',
    benchmark_json TEXT NOT NULL DEFAULT '{}',
    benchmark_report_path TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_plugin_artifacts_plugin_id
    ON plugin_artifacts(plugin_id, created_at);
```

源码包需要记录构建任务，便于管理页展示构建状态和排查问题。

```sql
CREATE TABLE plugin_builds (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    version TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_path TEXT NOT NULL,
    artifact_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,                -- queued/running/succeeded/failed/canceled
    builder_type TEXT NOT NULL DEFAULT '',
    builder_image TEXT NOT NULL DEFAULT '',
    go_version TEXT NOT NULL DEFAULT '',
    go_os TEXT NOT NULL DEFAULT '',
    go_arch TEXT NOT NULL DEFAULT '',
    cgo_enabled INTEGER NOT NULL DEFAULT 0,
    tags_json TEXT NOT NULL DEFAULT '[]',
    module_list_json TEXT NOT NULL DEFAULT '[]',
    abi_fingerprint TEXT NOT NULL DEFAULT '',
    abi_json TEXT NOT NULL DEFAULT '{}',
    log_path TEXT NOT NULL DEFAULT '',
    log_excerpt TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    started_at INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_plugin_builds_plugin_id
    ON plugin_builds(plugin_id, created_at);
```

Plugin 表示管理页里的一个插件槽位和期望状态。

```sql
CREATE TABLE plugins (
    id TEXT PRIMARY KEY,
    artifact_id TEXT NOT NULL,
    desired_state TEXT NOT NULL DEFAULT 'disabled',
    policy_state TEXT NOT NULL DEFAULT 'normal',
    enabled INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 100,
    scope_json TEXT NOT NULL DEFAULT '{}',
    rollout_json TEXT NOT NULL DEFAULT '{}',
    dry_run INTEGER NOT NULL DEFAULT 0,
    config_json TEXT NOT NULL DEFAULT '{}',
    config_version INTEGER NOT NULL DEFAULT 1,
    desired_generation INTEGER NOT NULL DEFAULT 1,
    restart_required INTEGER NOT NULL DEFAULT 0,
    deleted_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (artifact_id) REFERENCES plugin_artifacts(id)
);
```

`enabled` 可以作为第一版兼容字段或查询加速字段，但正式语义应以 `desired_state` 和 `policy_state` 为准：

- `desired_state` 表示管理员期望：`disabled`、`enabled`、`deleted`。
- `policy_state` 表示策略覆盖：`normal`、`review_required`、`blocked`、`quarantined`、`revoked`。
- 当 `policy_state` 不是 `normal` 时，Plugin Manager 必须先执行策略判断，再决定是否允许 runtime 收敛到 `desired_state`。
- `deleted_at>0` 表示插件槽位已逻辑删除；已加载 native code 和文件清理由 runtime/GC 在重启后继续完成。
- Admin API 返回时可以继续给前端提供 `enabled` 布尔值，但必须同时返回 `desired_state`、`policy_state` 和阻断原因。

第一版建议一个 plugin ID 只有一个启用实例。后续如果要允许同一插件多实例，需要把 `plugins.id` 从 plugin ID 改为 plugin instance ID，并增加：

```sql
plugin_id TEXT NOT NULL,
instance_id TEXT NOT NULL,
display_name TEXT NOT NULL DEFAULT '',
```

多实例适合：

- 同一个插件用不同配置处理不同 scope。
- 同一个 connection takeover 插件服务不同认证源。
- 同一个 event sink 插件写入不同外部系统。

多实例约束：

- instance ID 必须稳定并出现在日志、指标、审计和 dispatch table。
- plugin data、secret、config snapshot 默认按 instance namespace 隔离。
- 依赖声明默认依赖 plugin ID；如果依赖特定实例，需要显式声明 instance ID。
- first-match hook 排序需要使用 priority、plugin ID、instance ID 三元组。
- 第一版如果不支持多实例，管理页应阻止创建第二个实例，并在未来能力中保留扩展点。

插件可以拥有少量私有状态，避免所有插件都自行管理文件或外部数据库。

```sql
CREATE TABLE plugin_data (
    plugin_id TEXT NOT NULL,
    key TEXT NOT NULL,
    value_json TEXT NOT NULL,
    schema_version INTEGER NOT NULL DEFAULT 1,
    data_class TEXT NOT NULL DEFAULT 'operational',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (plugin_id, key)
);
```

`plugin_data` 适合保存小型状态，例如游标、缓存元数据、上次同步时间。大量数据、连接日志和长期指标不应写入该表，应由插件写入自己的外部存储或通过 event/metrics 系统输出。

数据字段规则：

- `schema_version` 表示该 key 的数据 schema 版本，用于插件升级和回滚检查。
- `data_class` 描述数据分类，例如 `operational`、`cache`、`cursor`、`profile_cache`、`exportable_config`。
- `size_bytes` 用于容量统计和配额检查。
- `expires_at=0` 表示不过期；非 0 时可由 GC 清理。
- value 必须是 JSON，不保存 secret 明文、大日志、完整 packet payload 或外部 API response 原文。

插件可以在 manifest 声明数据 schema：

```json
{
  "plugin_data": {
    "schemas": [
      {
        "key_prefix": "profile_cache/",
        "schema_version": 2,
        "data_class": "profile_cache",
        "retention": "7d",
        "exportable": false,
        "max_items": 10000,
        "max_bytes": 10485760
      },
      {
        "key_prefix": "sync_cursor/",
        "schema_version": 1,
        "data_class": "cursor",
        "retention": "keep",
        "exportable": true,
        "max_items": 100,
        "max_bytes": 65536
      }
    ]
  }
}
```

数据配额：

- 第一版应有全局默认配额，例如每插件 16 MiB、单 key 256 KiB。
- 插件 manifest 可以声明期望配额，但管理员可以覆盖。
- 超过配额时 `Set()` 返回明确错误，不应无限增长 SQLite。
- `cache`、`profile_cache` 等可丢弃数据可以由 GC 优先清理。
- `cursor`、`operational` 等数据默认保留，删除前需要管理员确认。

数据迁移：

```go
type PluginDataMigrator interface {
    MigrateData(ctx context.Context, store PluginDataStore, fromVersion int, toVersion int) error
}
```

迁移规则：

- artifact 切换前如果 manifest `plugin_data.schemas` 版本高于当前数据版本，应先执行数据迁移 dry-run 或实际迁移。
- 数据迁移失败不得切换 active artifact。
- 破坏性迁移必须管理员确认，并创建数据快照或备份点。
- 回滚到旧 artifact 前必须检查旧 artifact 是否能读取当前 data schema。
- 如果旧 artifact 不支持当前 data schema，管理员可以选择数据快照恢复、清理可丢弃数据或放弃回滚。

导入导出：

- promotion bundle 默认不导出 `plugin_data`。
- 只有 `exportable=true` 且 data class 允许迁移的数据可以导出，例如同步游标或小型规则状态。
- `profile_cache`、session cache、玩家隐私数据和外部 API response 默认不导出。
- 导出前必须按 data_class 做脱敏和大小限制。
- 导入时需要检查 schema_version、plugin ID、artifact 兼容性和环境标识。

删除插件：

- 管理页删除插件时必须询问是否保留 `plugin_data`。
- 保留数据时应标记为 orphaned，并允许后续清理。
- 删除数据时需要展示 data_class、key 数量、总大小和是否包含 exportable 数据。

GC 和保留：

- expired data 可以由后台 GC 清理。
- GC 必须遵守 data_class 和 retention。
- 清理操作写审计日志。
- GC 不应清理当前迁移、回滚或诊断正在引用的数据快照。

插件 secret 独立存储，不和普通配置混在一起。

```sql
CREATE TABLE plugin_secrets (
    plugin_id TEXT NOT NULL,
    name TEXT NOT NULL,
    current_version TEXT NOT NULL DEFAULT '',
    previous_version TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'api_token',
    rotation_state TEXT NOT NULL DEFAULT 'stable',
    not_before INTEGER NOT NULL DEFAULT 0,
    not_after INTEGER NOT NULL DEFAULT 0,
    value_ciphertext BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (plugin_id, name)
);
```

第一版可以先使用本机文件权限和数据库访问控制保护 secret；如果引入外部 KMS 或系统密钥环，`value_ciphertext` 应保存加密后的密文和必要的 key metadata。

如果需要保留 grace period 内的新旧 secret，建议增加版本表：

```sql
CREATE TABLE plugin_secret_versions (
    plugin_id TEXT NOT NULL,
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    value_ciphertext BLOB NOT NULL,
    state TEXT NOT NULL,                 -- active/previous/expired/revoked
    not_before INTEGER NOT NULL DEFAULT 0,
    not_after INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (plugin_id, name, version)
);
```

版本规则：

- `current_version` 是插件默认读取的版本。
- `previous_version` 只在 `dual_read` 或 `dual_write` grace period 内有效。
- `rotation_state=rotating` 时，Admin 页面必须展示剩余 grace period、影响插件和是否需要 reload。
- grace period 结束后，旧版本进入 `expired`，`GetVersion` 不再返回明文。
- 删除 secret 前必须检查是否仍被 enabled 插件引用。

如果未来实现 `admin.auth.provider/v1`，需要保存外部身份到本地 Admin 用户的绑定关系。该表属于 Admin 登录控制面，不属于 Minecraft 玩家身份存储。

```sql
CREATE TABLE admin_external_identities (
    provider_id TEXT NOT NULL,
    subject_hash TEXT NOT NULL,
    username TEXT NOT NULL,
    subject_display TEXT NOT NULL DEFAULT '',
    claims_summary_json TEXT NOT NULL DEFAULT '{}',
    groups_json TEXT NOT NULL DEFAULT '[]',
    mapping_version INTEGER NOT NULL DEFAULT 1,
    last_login_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider_id, subject_hash)
);

CREATE INDEX idx_admin_external_identities_username
    ON admin_external_identities(username);
```

绑定规则：

- `username` 必须引用现有本地 Admin 用户，或由受控 JIT provisioning 创建。
- `subject_hash` 使用部署级 salt 或不可逆 HMAC；不保存外部 subject 明文，除非管理员显式允许并满足审计要求。
- `claims_summary_json` 和 `groups_json` 只保存低基数摘要，不保存完整 ID token、access token、LDAP entry 或 refresh token。
- 删除本地用户时必须撤销或禁用关联的 external identity，并清理该用户 session。
- break-glass 本地 admin 不依赖该表；备份恢复时即使外部身份绑定不可用，本地 admin 仍可登录。

如果第一版只做手动上传，可以不创建插件仓库表，`repository_id` 和 `repository_url` 保持为空。后续支持仓库时建议增加：

```sql
CREATE TABLE plugin_repositories (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,                  -- official/internal/file/url
    url TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    trust_policy_json TEXT NOT NULL DEFAULT '{}',
    update_policy TEXT NOT NULL DEFAULT 'manual',
    last_checked_at INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE plugin_repository_cache (
    repository_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    version TEXT NOT NULL,
    index_json TEXT NOT NULL,
    artifact_sha256 TEXT NOT NULL DEFAULT '',
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (repository_id, plugin_id, version)
);
```

仓库表只记录发现和下载信息，不改变“本地 artifact 经过管理员确认后才能加载或启用”的原则。

长操作需要统一记录，避免 Admin 页面只能靠轮询多个业务表推断进度。构建、启用、回滚、promotion import、artifact GC、plugin_data GC、仓库检查和 DR drill 都可以映射为 operation。

```sql
CREATE TABLE plugin_operations (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,                -- queued/running/succeeded/failed/canceled
    progress INTEGER NOT NULL DEFAULT 0,
    message TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '{}',
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    started_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_plugin_operations_idempotency
    ON plugin_operations(type, target_type, target_id, idempotency_key)
    WHERE idempotency_key <> '';
```

operation 规则：

- 短操作可以同步完成，但仍可选择创建 operation 记录用于审计和进度展示。
- 长操作 API 应返回 `operation_id`，前端通过 operation 查询状态。
- `idempotency_key` 由客户端或服务端生成，用于防止刷新页面或网络重试重复创建构建、导入或启用任务。
- `progress` 只表示粗略百分比，不应作为业务状态真相；业务状态仍以 artifact/build/plugin state 为准。
- cancel 采用协作取消；已进入 commit 阶段的 enable/rollback/repository import apply/promotion apply 不能强行中断，只能等待完成后再回滚。
- retry 默认创建新的 operation，保留旧 operation 结果；如果复用同一 build source，应在 result 中关联原 operation。
- operation 结果不能包含 secret、完整日志、完整 packet payload 或敏感 config。
- operation 完成后应写审计日志，并在 result 中保存相关 artifact ID、build ID、generation 或 report ID。

插件业务事件默认只保存最近摘要，用于 Admin 排障和诊断包。长期审计仍走 audit log，长期指标走 metrics 后端。

```sql
CREATE TABLE plugin_event_recent (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    name TEXT NOT NULL,
    severity TEXT NOT NULL,
    subject_hash TEXT NOT NULL DEFAULT '',
    attributes_json TEXT NOT NULL DEFAULT '{}',
    trace_id TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_plugin_event_recent_plugin_time
    ON plugin_event_recent(plugin_id, created_at);
```

事件摘要规则：

- 只保存脱敏、低基数、大小受限的 `attributes_json`。
- `subject_hash` 使用部署级 salt，不能反推出玩家名、UUID 或 IP。
- 按插件设置 ring buffer 或时间保留，例如每插件最近 1000 条或 24 小时。
- 超出保留策略直接清理，不参与 promotion bundle。

event subscriber、audit sink 和 webhook 类插件需要可观测的投递状态。第一版可以不提供持久化可靠队列，但应预留投递摘要表，避免排障时只能看到 subscriber 内部日志：

```sql
CREATE TABLE plugin_event_deliveries (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL,
    subscriber_plugin_id TEXT NOT NULL,
    subscription_id TEXT NOT NULL,
    status TEXT NOT NULL,                -- queued/delivered/failed/dropped/dead_letter
    attempts INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER NOT NULL DEFAULT 0,
    last_error_kind TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_plugin_event_deliveries_subscriber
    ON plugin_event_deliveries(subscriber_plugin_id, updated_at);
```

投递记录只保存低基数状态，不复制完整事件 payload。事件 payload 仍以 `plugin_event_recent` 的脱敏摘要或内存队列为准。

动态路由插件需要可解释的最近决策摘要，避免管理员只看到最终 backend 却不知道是 SQLite route、route provider 还是 upstream 插件做出的选择：

```sql
CREATE TABLE plugin_route_decisions_recent (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL DEFAULT '',
    handler_id TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    requested_host_hash TEXT NOT NULL DEFAULT '',
    sqlite_route_hit INTEGER NOT NULL DEFAULT 0,
    sqlite_upstream_protocol TEXT NOT NULL DEFAULT '',
    decision TEXT NOT NULL,              -- use_sqlite/override/fallback/reject/pass
    effective_upstream_protocol TEXT NOT NULL DEFAULT '',
    reason_code TEXT NOT NULL DEFAULT '',
    cache_status TEXT NOT NULL DEFAULT '',
    transport TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_plugin_route_decisions_recent_plugin_time
    ON plugin_route_decisions_recent(plugin_id, created_at);
```

route decision 摘要规则：

- `requested_host_hash` 使用部署级 salt；Admin 可在当前连接上下文里展示明文 host，但长期 recent 表默认不保存明文。
- upstream address 默认不保存明文，只保存 protocol 和低基数 reason；诊断包需要明文时必须按权限脱敏导出。
- 只保存最近窗口，例如每插件最近 1000 条或 24 小时。
- SQLite route 修改仍以 audit log 为准；route decision recent 只是运行时排障证据。

后台任务状态可以作为运行时状态实时读取，但最近运行摘要需要持久化，便于重启后排障和审计：

```sql
CREATE TABLE plugin_background_task_runs (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    trigger_type TEXT NOT NULL,           -- schedule/manual/run_on_start
    status TEXT NOT NULL,                 -- running/succeeded/failed/skipped/canceled/timed_out
    started_at INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    error_kind TEXT NOT NULL DEFAULT '',
    result_summary_json TEXT NOT NULL DEFAULT '{}',
    operation_id TEXT NOT NULL DEFAULT '',
    node_id TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_plugin_task_runs_recent
    ON plugin_background_task_runs(plugin_id, task_id, started_at);
```

保存规则：

- 只保存最近摘要，默认按每任务最近 N 次或时间窗口保留。
- `result_summary_json` 必须脱敏，不能包含 secret、token、完整外部 response、完整 packet payload 或玩家隐私原文。
- 手动触发任务应关联 `operation_id` 和 actor。
- schedule 自动触发任务不应无限增长 DB，过期记录由 GC 清理。

插件如果需要文件型资源，应把“包内只读资源”和“运行时可写数据”分开管理，避免插件随意在 artifact 目录写入状态，导致 sha256 校验、回滚和 GC 语义失效。

```sql
CREATE TABLE plugin_file_resources (
    plugin_id TEXT NOT NULL,
    namespace TEXT NOT NULL,             -- resource/data/cache/tmp/log/diagnostic
    path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    data_class TEXT NOT NULL DEFAULT 'operational',
    retention TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (plugin_id, namespace, path)
);

CREATE INDEX idx_plugin_file_resources_plugin_ns
    ON plugin_file_resources(plugin_id, namespace);
```

该表只记录 gateway 通过 SDK 或 GC 可治理的文件资源摘要，不要求枚举 artifact 包里的每个文件。native `go-plugin` 仍可绕过 SDK 直接访问文件系统，因此第一版不能把它当强隔离；sandbox runtime 才能把目录权限真正强制化。

运行时状态不作为真相写入 SQLite，接口实时从 Plugin Manager 读取：

```json
{
  "id": "upstream-rewrite",
  "enabled": true,
  "loaded": true,
  "active_artifact_id": "sha256:...",
  "desired_artifact_id": "sha256:...",
  "restart_required": false,
  "extension_points": [
    { "type": "hook", "key": "legacy upstream-connect contract" }
  ],
  "error": ""
}
```

建议插件文件落盘路径：

```text
<plugin_dir>/artifacts/<plugin_id>/<sha256>/plugin.so
<plugin_dir>/artifacts/<plugin_id>/<sha256>/manifest.json
<plugin_dir>/artifacts/<plugin_id>/<sha256>/resources/
<plugin_dir>/runtime/<plugin_id>/data/
<plugin_dir>/runtime/<plugin_id>/cache/
<plugin_dir>/runtime/<plugin_id>/tmp/
<plugin_dir>/runtime/<plugin_id>/logs/
<plugin_dir>/runtime/<plugin_id>/diagnostics/
```

`plugin_dir` 默认放在 SQLite 数据库同级目录下的 `plugins/`，也可以通过启动期环境变量 `MC_GATEWAY_PLUGIN_DIR` 覆盖。

目录语义：

| 目录 | 读写 | 内容 | 保留策略 |
| --- | --- | --- | --- |
| `artifacts/...` | gateway 写，插件只读 | `plugin.so`、manifest、README、LICENSE、SBOM、包内资源 | 随 artifact 保留和 GC |
| `runtime/<plugin>/data/` | 插件可写 | 大于 `plugin_data` KV 的结构化文件、小型数据库、可迁移状态 | 默认保留，删除插件时询问 |
| `runtime/<plugin>/cache/` | 插件可写 | 可重建缓存，例如 profile cache、route cache、session cache 摘要 | 可按 retention/配额自动清理 |
| `runtime/<plugin>/tmp/` | 插件可写 | 临时文件、下载中间文件、action 运行中产物 | 启动和 disable 后可清理 |
| `runtime/<plugin>/logs/` | gateway/插件可写 | 插件专用日志片段或外部库日志 | 按日志保留策略清理 |
| `runtime/<plugin>/diagnostics/` | gateway/插件可写 | 管理员触发的诊断包中间产物 | 默认短期保留，导出前脱敏 |

目录规则：

- artifact 目录必须是 content-addressed，不允许插件写入；任何写入都会破坏校验和回滚语义。
- package 内静态资源应放在 `resources/`，由 manifest 声明入口、大小和用途。
- 运行时文件路径必须规范化，拒绝绝对路径、`..`、symlink escape、特殊文件和平台分隔符绕过。
- SDK 返回的路径必须限定在当前插件 namespace；插件不能读取其他插件 runtime 目录。
- cache/tmp 可以自动清理；data 默认不自动删除，除非管理员确认或 manifest 声明可丢弃。
- 日志和诊断目录不能保存 secret、token、完整 session response 或完整 packet payload。
- native plugin 下这些规则主要由 SDK、文档、示例、conformance 和 Admin GC 约束；sandbox-process 下应映射为只读 artifact mount 和可写 runtime volume。

## 多实例和集群部署

第一版可以按单实例 gateway 设计：SQLite、plugin artifact 目录和 Plugin Manager 都在同一个节点上。若后续运行多个 gateway 实例，需要提前保证模型可以扩展。

### 单实例主路径

单实例模式下：

- SQLite 是 desired state 真相来源。
- 本地 `plugin_dir` 保存 artifact、source package 和 manifest。
- 本地 Plugin Manager 负责加载、启用、禁用和 runtime state。
- 源码包 builder 可以是 local-process 或 container。
- Admin 页面展示的是当前节点状态。

### 多实例扩展模型

多实例模式需要把 desired state 和 runtime state 分开：

```text
shared desired state
  -> plugin_artifacts metadata
  -> artifact storage
  -> node-local Plugin Manager
  -> node runtime status
```

建议原则：

- desired state 仍由一个管理面写入，例如共享数据库或控制平面 API。
- artifact 文件应放在共享对象存储、共享文件系统，或由控制面分发到各节点 content-addressed 本地目录。
- 每个 gateway 节点独立执行 `plugin.Open`，不能共享已加载的 Go plugin 代码。
- runtime state 是节点本地状态，不能覆盖全局 desired state。
- Admin 页面需要区分全局 desired state 和每个节点 runtime state。
- 某个节点加载失败不应自动回滚全局 desired state，但应显示 partial rollout failed。

### 构建任务协调

源码包构建在多实例模式下不能让所有节点重复构建同一个 artifact。可选策略：

| 策略 | 说明 |
| --- | --- |
| single builder | 只有控制面或指定 builder 节点执行构建 |
| lease-based worker | 多个 worker 抢占 build job lease，只有持有 lease 的 worker 构建 |
| external CI | 源码包不在 gateway 内构建，由 CI 产出二进制 `.mcgp` |

构建结果必须以 artifact sha256 为准。节点只下载和加载已经登记成功的 artifact。

### 节点状态

当前实现已经有两层节点状态。网关节点心跳表用于 Plugin Service 状态页和 CLI/API 展示当前已知节点：

```sql
CREATE TABLE plugin_nodes (
    node_id TEXT PRIMARY KEY,
    hostname TEXT NOT NULL DEFAULT '',
    pid INTEGER NOT NULL DEFAULT 0,
    service_mode TEXT NOT NULL DEFAULT 'in-process',
    data_plane_mode TEXT NOT NULL DEFAULT 'in-process',
    status TEXT NOT NULL DEFAULT 'online',
    started_at INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL
);
```

`GET /admin/api/plugin-service` 返回 `nodes`，每个节点包含 `node_id`、`service_mode`、`data_plane_mode`、`heartbeat_at` 和 `stale`。这只说明 gateway 节点是否近期上报，不代表某个插件 artifact 已经在该节点成功加载。

插件维度 runtime state 使用 `plugin_node_states`：

```sql
CREATE TABLE plugin_node_states (
    node_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    desired_generation INTEGER NOT NULL DEFAULT 0,
    loaded INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 0,
    runtime_state TEXT NOT NULL DEFAULT '',
    health TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (node_id, plugin_id)
);
```

插件维度节点状态只用于展示和诊断，不作为启用真相。当前节点会在 load、enable、disable、delete、reconcile failure 时更新状态；插件详情返回 `rollout_status` 和 `node_runtime_states`，用于展示 partial rollout failure。二进制上传和 source build 产物会在节点本地 content-addressed 目录保留可分发 `.mcgp` 包，`rollout_status.artifact_distribution_*` 展示本地包状态；Admin-to-Admin 手动 artifact package 传输已可用，CLI promotion 跨网关 apply 可串联源网关 package download、目标网关 upload 和目标侧 promotion apply。跨节点自动分发、repository apply 编排和自动集群级 apply 仍未实现。

### 多实例发布语义

多实例发布需要支持：

- 分批节点启用，例如先 canary 节点，再全量节点。
- 节点级 rollback。
- 某些节点需要重启才能清理已加载 Go plugin。
- 管理页展示每个节点的 artifact、health、active calls、active proxy connections 和 restart required。
- 对 connection takeover 插件，draining 是节点本地过程；全局禁用需要等待所有节点 draining 完成或管理员强制关闭。

第一版如果只支持单实例，文档和 UI 应明确写出；但数据模型中的 artifact、desired/runtime state、content-addressed 路径和 node state 预留应避免后续重构。

## 源码包构建环境

源码包解决的是构建一致性问题，不解决运行时隔离问题。

```text
source + go-plugin
  -> 构建阶段由 builder 隔离执行
  -> 产出 plugin.so
  -> 加载和启用后仍是 native trusted plugin
```

gateway 主进程不应直接执行 `go build`。源码包上传后创建 build job，由独立 builder 负责构建：

```text
Admin 上传 .mcgp
  -> gateway 解析 manifest
  -> 写入 plugin_builds(status=queued)
  -> builder 获取源码包
  -> 固定命令 go build -buildmode=plugin
  -> 输出 plugin.so、构建日志和构建元数据
  -> gateway 校验产物 manifest 和 sha256
  -> 写入 plugin_artifacts
```

第一版建议支持两类 builder：

| Builder | 用途 | 说明 |
| --- | --- | --- |
| `local-process` | 开发和本地测试 | gateway 启动受控子进程，隔离较弱 |
| `container` | 生产环境 | 使用固定 builder image，限制资源和环境 |

生产环境推荐 container builder。builder image 应与 gateway release 绑定，避免 Go plugin ABI 不一致：

```text
ghcr.io/tursom/mc-gateway-plugin-builder:release-<gateway-release>-<plugin-api>-go<go-version>-<goos>-<goarch>@sha256:<digest>
```

示例：

```text
ghcr.io/tursom/mc-gateway-plugin-builder:release-v0-1-0-plugin-api-v1-go1.25.0-linux-amd64@sha256:<digest>
```

当前实现会在 prod governance/preflight 中检查 source build 的 container provenance：缺失 builder image digest、builder image 未使用 `@sha256:` digest-pinned 引用，或 builder image tag/path 未绑定当前 gateway release、plugin API 版本、Go 版本和 GOOS/GOARCH，都会产生 warning，非 preview enable 需要 warning override。官方 builder image 由 `.github/workflows/plugin-builder-image.yml` 发布；workflow 每个平台输出 release-bound tag 和 digest-pinned 文本 artifact。`AssessSupplyChain` 会记录 `builder_image_pinned`、`builder_image_go_version_bound`、`builder_image_api_version_bound`、`builder_image_release_bound`、gateway Go version、builder Go version match 和 GOOS/GOARCH match。

external CI 产出的 binary `.mcgp` 不在 gateway 内执行构建，因此必须通过 `provenance.json` 和 supply-chain assessment 提供可审计 metadata。目标环境必须看到顶层 `signature.verified=true`、顶层 SBOM metadata、`external_ci.source_sha256`、`external_ci.artifact_sha256`、`external_ci.run_id`、`external_ci.builder_id`、`external_ci.attestation`、`external_ci.sbom`、`external_ci.release_provenance` 和 `external_ci.trusted=true`。`artifact_sha256` 或可选 `package_sha256` 与本地 artifact 不匹配会直接产生 blocking issue；缺失 provenance、签名未验证或 trusted 标记缺失会阻断 enable、rollback、repository apply 和 promotion apply。

构建环境必须固定以下维度：

- gateway version
- Go toolchain version
- `plugin/api` module path 和版本
- GOOS/GOARCH
- GOAMD64/GOARM64 等微架构级别
- build tags
- CGO 设置
- 关键 build settings，例如 `-trimpath`、`-mod`、`-buildmode`
- shared module graph，用于生成 ABI fingerprint

第一版构建命令固定为：

```sh
go build -buildmode=plugin -trimpath -o "$OUTPUT/plugin.so" "$ENTRY"
```

不执行插件包里的任意构建脚本。后续如果支持自定义构建步骤，必须重新评估构建沙箱风险。

构建隔离要求：

- 构建使用独立临时工作目录。
- 源码目录只读挂载。
- output 目录独立挂载，只允许导出 `plugin.so`、构建日志和构建元数据。
- 清理环境变量，只保留白名单。
- 构建超时，例如 60-120 秒。
- 限制 CPU、内存、进程数、文件描述符和日志大小。
- 默认 `CGO_ENABLED=0`。
- 默认不把 gateway 的数据库、插件目录、密钥和运行时环境变量暴露给 builder。
- 网络访问由管理员配置，推荐只允许访问 Go module proxy；高安全环境可以要求源码包携带 `vendor/` 并关闭网络。

container builder 推荐约束：

```text
read-only rootfs
non-root user
no-new-privileges
memory limit
cpu limit
pids limit
network none 或受控 GOPROXY
```

依赖策略：

- 默认允许通过配置的 `GOPROXY` 下载 Go modules。
- 支持 manifest `build.vendor_required=true`，要求源码包包含 `vendor/` 并使用 `-mod=vendor`。
- 不从 gateway 进程继承 `GONOSUMDB`、私有 token 等敏感环境变量。
- 私有依赖需要通过独立的 builder 配置提供，例如只读凭据或内网 module proxy。

构建缓存可以存在，但必须和 gateway 数据隔离：

```text
plugin_build_cache/
  gomod/
  gocache/
```

缓存不能作为产物可信来源。最终 artifact 仍以 `plugin.so` sha256 和 `manifest.json` 校验为准。

构建完成后需要保存：

- 源码包 sha256。
- 构建产物 sha256。
- builder 类型和 image。
- Go version、GOOS、GOARCH。
- CGO 设置和 build tags。
- ABI fingerprint 和参与计算的 abi JSON。
- `go version -m plugin.so` 或 module list。
- 构建日志路径和截断摘要。
- 构建错误。

## 生命周期

插件有两类状态：

- Desired state：SQLite 中管理员期望的启用状态、artifact、配置和 priority。
- Runtime state：当前进程中的加载、启用、错误、熔断和 draining 状态。

运行时状态不作为真相覆盖 desired state。进程重启后，Plugin Manager 根据 SQLite 重新收敛。

### 状态收敛和并发控制

Plugin Manager 应采用 reconcile 模型：Admin API 只修改 desired state，运行时管理器异步或同步把 runtime state 收敛到 desired state。

核心规则：

- `plugins.desired_generation` 每次 desired state 变更递增。
- runtime state 记录当前已应用的 generation。
- Plugin Manager 只把 runtime 推进到最新 generation，不用旧 generation 覆盖新状态。
- 连接路径只读取不可变 dispatch table snapshot。
- dispatch table 更新必须 copy-on-write，一次性原子替换。
- 启用、禁用、切换版本、配置更新都必须是幂等操作。
- 重复调用 enable 已启用插件，应返回当前状态而不是重复创建实例。
- 重复调用 disable 已禁用插件，应返回成功。
- 如果 enable 正在进行，第二个 enable 应等待、复用同一任务或返回 conflict，不能并行初始化两个实例。
- 对 upload、build retry、enable、rollback、promotion import/apply、GC 和 DR drill，应支持 idempotency key 或复用未完成 operation。
- 长操作必须能通过 operation API 查询状态；连接路径不等待长操作完成。

Admin 写操作事务边界：

- 上传 artifact：文件落盘到临时目录成功后，再在事务中写 artifact metadata；事务提交后移动到 content-addressed final path，或使用先 final path 后 DB commit 的补偿清理。
- 更新 plugin desired state：在一个 DB 事务中写 `plugins`、配置快照和审计记录。
- 切换 artifact：先确认目标 artifact 存在且未被 GC，再递增 generation。
- 删除 plugin：先写 desired disabled/deleted，再由 Plugin Manager 停止 runtime；文件清理由 GC 负责。

崩溃恢复：

- 启动时扫描 DB desired state、artifact 目录和 pending cleanup。
- 对 DB 存在但文件缺失的 artifact 标记 failed。
- 对文件存在但 DB 不引用的 artifact 标记 orphan，进入 GC candidate。
- 对 desired enabled 的插件按 priority 和 generation 收敛。
- 对 `deleted_pending_restart` 的 artifact 尝试清理。

错误返回应区分：

| 错误 | 含义 |
| --- | --- |
| `conflict` | generation 已变化或已有同类操作进行中 |
| `precondition_failed` | 依赖、secret、artifact、Go/API 版本不满足 |
| `validation_failed` | manifest、config schema 或 ReloadConfig 校验失败 |
| `runtime_failed` | plugin.Open、Init、handler 注册或 HealthCheck 失败 |
| `restart_required` | 操作已记录，但需要重启才能彻底生效 |

状态机需要按对象拆分，避免把 artifact、build job、plugin slot、runtime instance 和 review 混成一个字段。Admin 页面可以合并展示，但 API 和数据库应保留各自语义。

#### Artifact 状态机

```text
uploaded
  -> validated
  -> staged
  -> active
  -> superseded
  -> gc_candidate
  -> deleted

任意阶段:
  -> rejected
  -> revoked
  -> missing
```

| 状态 | 说明 | 允许动作 |
| --- | --- | --- |
| `uploaded` | 包已落盘，等待校验 | validate、delete |
| `validated` | manifest、平台、sha256 和包结构校验通过 | policy evaluate、stage、delete |
| `staged` | 可被插件槽位引用，但尚未处理生产流量 | load、review、switch、delete |
| `active` | 至少一个 plugin desired/runtime 正在引用 | switch、rollback、revoke |
| `superseded` | 不再 active，但仍可回滚 | rollback、gc mark、revoke |
| `gc_candidate` | 符合清理策略 | delete、restore candidate |
| `deleted` | DB 或文件已清理 | 无 |
| `rejected` | review 拒绝或策略不允许 | re-review、delete |
| `revoked` | denylist 命中或紧急撤销 | quarantine affected plugins、delete after restart |
| `missing` | DB 引用存在但文件缺失 | restore、mark failed、delete reference |

#### Build 状态机

```text
queued -> running -> succeeded
queued -> canceled
running -> failed
running -> canceled
failed -> queued
```

| 状态 | 说明 |
| --- | --- |
| `queued` | 等待 builder 执行 |
| `running` | builder 正在构建 |
| `succeeded` | 已产出 binary artifact |
| `failed` | 构建失败，保留日志摘要 |
| `canceled` | 管理员取消或系统关闭 |

构建状态只影响源码包产物，不应直接改变已启用插件的 runtime state。

#### Plugin Desired/Policy 状态机

```text
disabled -> enabled
enabled -> disabled
disabled -> deleted
enabled -> quarantined -> disabled

policy_state:
normal -> review_required -> normal
normal -> blocked
normal -> quarantined
normal -> revoked
```

| 字段 | 状态 | 说明 |
| --- | --- | --- |
| `desired_state` | `disabled` | 管理员期望插件不处理新流量 |
| `desired_state` | `enabled` | 管理员期望插件处理匹配流量 |
| `desired_state` | `deleted` | 插件槽位逻辑删除 |
| `policy_state` | `normal` | 没有策略阻断 |
| `policy_state` | `review_required` | 高风险变更待审批 |
| `policy_state` | `blocked` | 策略阻断，不能 enable 或 rollback |
| `policy_state` | `quarantined` | 已启用插件被隔离，新流量不得进入 |
| `policy_state` | `revoked` | 插件 ID 或 artifact 已撤销 |

收敛规则：

- `policy_state=blocked/revoked` 时，runtime 目标强制视为 disabled。
- `policy_state=review_required` 时，不能从 disabled 收敛到 enabled；已经 enabled 的插件如果只是需要重新审批配置，应保持旧 dispatch table，直到管理员批准新组合。
- `policy_state=quarantined` 时，立即从 dispatch table 移除 handler，connection takeover 连接进入 draining 或 force-close。
- `desired_state=deleted` 时，必须先停止新流量，再进入文件和 runtime 清理流程。
- rollback 不能绕过 `policy_state`；目标 artifact 的策略状态必须重新计算。

#### Runtime Instance 状态机

```text
not_loaded
  -> loading
  -> loaded
  -> initializing
  -> ready
  -> draining
  -> stopped

任意阶段:
  -> failed
  -> degraded
  -> deleted_pending_restart
```

状态含义：

| 状态 | 说明 |
| --- | --- |
| `not_loaded` | 当前进程尚未加载该 artifact |
| `loading` | 正在执行 `plugin.Open` 和符号查找 |
| `loaded` | `plugin.Open` 和 symbol lookup 成功，但 handler 未发布 |
| `initializing` | 正在创建实例、解码配置、迁移数据、注册 handler |
| `ready` | handler 已发布到 dispatch table |
| `draining` | 已停止接收新流量，等待已有调用或协议代理连接结束 |
| `stopped` | runtime instance 已停止；Go plugin 代码可能仍留在进程内 |
| `failed` | 构建、加载、配置或初始化失败 |
| `degraded` | 插件仍运行，但错误率、超时或熔断策略触发告警 |
| `deleted_pending_restart` | 逻辑删除完成，但已加载代码或文件需重启后彻底清理 |

#### Review 状态机

```text
not_required
  -> required
  -> approved
  -> expired

required -> rejected
approved -> expired
```

review 只绑定 artifact sha256、config hash、scope hash、rollout hash、runtime limits hash 和 policy snapshot version。任一输入变化都必须让旧 review 失效。

状态机不变量：

- 一个 plugin slot 在同一节点上最多有一个 `ready` runtime instance 处理新流量。
- 新 dispatch table 发布成功之前，旧 `ready` instance 继续处理新流量。
- `active artifact`、`desired artifact` 和 `loaded artifact` 可能短暂不同，API 必须分别展示。
- `artifact=revoked` 时不得成为 `desired artifact`，也不得通过 rollback 重新 active。
- runtime `failed` 不应自动修改 desired state；管理员需要看到“期望 enabled，但运行失败”的差异。

#### Native Plugin 实例约束

Go `plugin.Open` 的行为决定了第一版必须明确实例边界：

- 同一路径的 `.so` 在同一进程内只会加载一次。
- package `init()`、包级变量、全局单例、后台 goroutine 和注册到第三方库的全局 hook 不会因为 disable 或 delete 自动回滚。
- `Destroy()` 只能停止插件实例自己持有的资源，不能卸载 Go runtime 已加载的代码。
- 重新启用同一已加载 artifact 时，Plugin Manager 可以重新调用 factory 创建新实例，但包级全局状态仍是同一份。

插件开发约束：

- 插件业务状态必须优先放在 factory 返回的实例对象中，而不是 package 级全局变量。
- package 级全局变量只允许保存只读常量、sync.Once 初始化的不可变资源或无状态 helper。
- 不允许在 package `init()` 中启动 goroutine、打开长期连接、读取 secret 或注册全局副作用。
- 需要后台工作时必须使用 `RegisterBackgroundTask`，不要自行启动无法取消的 goroutine。
- `Init()` 可以注册 handler、订阅事件和初始化实例资源，但必须能在 `Destroy()` 中停止。
- `Destroy()` 必须幂等、短超时、可重复调用；失败不能阻止 dispatch table 移除 handler。
- handler、后台任务和事件订阅必须检查 context cancellation，避免旧 generation 在 disable、reload 或切版本后继续工作。

generation fence：

- Plugin Manager 为每个 runtime instance 分配 `runtime_instance_id` 和 `applied_generation`。
- handler registration、background task、event subscription 和 action 执行都应绑定该 generation。
- dispatch table 只引用当前 ready generation 的 registration。
- 旧 generation 的异步回调返回时，只能记录自己的结果，不能覆盖新 generation 的 runtime state、health 或 config。
- 如果插件在 `Destroy()` 后仍继续上报事件或指标，gateway 应标记为 stale generation 并丢弃或降级记录。

重新加载策略：

| 场景 | 策略 |
| --- | --- |
| 未加载过的新 artifact | 可以 `plugin.Open` 后启用，无需重启 |
| 已加载过的同一 artifact | 可以创建新实例，但代码和包级状态复用 |
| 同 plugin ID 新 artifact | 使用不同 content-addressed path；可以并存加载，旧实例 draining |
| 同路径覆盖文件 | 禁止；会破坏 Go plugin 缓存和 sha256 语义 |
| 删除已加载 artifact | 逻辑删除，标记 `deleted_pending_restart` |
| package init 有不可逆副作用 | 只能提示 restart required 或阻断热启用 |

### 上传

上传只做文件处理和 manifest 校验，不加载插件代码。不同 `artifact_type` 后续处理不同：

1. 管理员上传 `.mcgp`。
2. 服务端写入临时目录。
3. 解压并校验路径，拒绝 zip slip。
4. 解析 `manifest.json`。
5. 校验 manifest 必填字段、插件 ID 格式、目标平台和 API 版本。
6. 计算上传包 sha256。
7. 移动到 content-addressed 目录。
8. 如果 `artifact_type=binary`，校验 `plugin.so` 并写入 `plugin_artifacts`。
9. 如果 `artifact_type=source`，写入 `plugin_builds(status=queued)`，等待 builder 产出 artifact。
10. 如果 `plugins.id` 不存在，创建 disabled 插件记录。
11. 记录审计日志。

上传不会执行插件代码。

源码包上传成功不代表插件可加载。只有构建成功并生成 `plugin_artifacts` 后，才能执行加载和启用。

### 构建

构建只适用于 `artifact_type=source` 的 `.mcgp`：

1. Plugin Build Worker 获取 queued build job。
2. 选择匹配当前 gateway release 的 builder。
3. 创建隔离工作目录并展开源码包。
4. 根据 manifest `build` 配置生成固定 `go build` 命令。
5. 执行构建并收集日志。
6. 校验输出 `plugin.so` 存在且大小在限制内。
7. 计算 `plugin.so` sha256。
8. 尝试读取 `go version -m` 和基础构建信息。
9. 将 `plugin.so` 移入 artifact 目录。
10. 写入 `plugin_artifacts`，更新 `plugin_builds(status=succeeded, artifact_id=...)`。
11. 构建失败时更新 `plugin_builds(status=failed, error=...)`，不改变现有 active artifact。

构建成功不会自动加载或启用插件。管理员可以在管理页选择构建出的 artifact，再执行加载或启用。

### 加载

加载会执行 Go plugin 的 package init，因此只允许管理员操作。

1. 读取 plugin 当前 artifact。
2. 做 manifest preflight 校验。
3. 校验 ABI fingerprint、Go/API/GOOS/GOARCH、CGO 和 build tags。
4. 调用 `plugin.Open(file_path)`。
5. 查找 `Plugin` factory。
6. 将 factory 缓存在 Plugin Manager runtime registry。
7. 更新运行时状态为 loaded。

加载不等于启用。加载后插件代码已经进入进程，但 extension point handler 不生效。

### 启用

启用会让插件参与流量处理：

prepare 阶段：

1. 如果插件未加载，先尝试加载。
2. 调用 factory 创建插件实例。
3. 用 `NewConfigObj()` 创建配置对象。
4. 检查 `plugins.config_version` 和 manifest `config_version`，必要时先执行配置迁移。
5. 检查 `plugin_data` schema version，必要时执行数据迁移 dry-run 或迁移。
6. 从 `plugins.config_json` decode 到配置对象。
7. 调用 `ReloadConfig(config)` 做业务校验。
8. 创建 registration collector，但不发布到 active dispatch table。
9. 调用 `Init(gateway)`，插件把 handler 注册到 collector。
10. Plugin Manager 校验 extension point key、handler ID、签名、scope、rollout 和 capabilities。

warmup 阶段：

1. 如果插件实现 HealthCheck，执行 readiness check。
2. 如果插件注册后台任务且声明 `RunOnStart`，可以先执行一次短超时 warmup。
3. 对支持 dry-run evaluate 的插件，可以执行样例 request dry run。
4. 记录 warmup 结果和耗时。

commit 阶段：

1. 基于 collector 生成新的只读 dispatch table snapshot。
2. 原子发布新的 dispatch table。
3. 将 `plugins.enabled` 置为 1，记录已应用 generation。
4. 清除可清除的 `restart_required`。
5. 记录审计日志和 runtime state。

如果启用失败，旧的 active dispatch table 不变。

启用必须满足两阶段语义：prepare/warmup 失败不会影响旧实例；只有 commit 成功后新插件才处理新流量。commit 之后如果后台任务或 health 变为 degraded，按运行时治理处理，不应回滚已经完成的 DB 事务，除非管理员执行 rollback。

### 禁用

禁用是逻辑卸载：

1. 原子移除该插件的 extension point registration。
2. 调用插件实例 `Destroy()`。
3. 将 `plugins.enabled` 置为 0。
4. 运行时状态显示 disabled。

即使 `Destroy()` 返回错误，也不应继续让新连接调用该插件。错误需要展示在管理页并写入审计日志。

Go plugin 已加载的代码和包级变量仍留在进程内。禁用不能释放 `.so` 代码、全局变量或 package init 产生的进程级副作用。

### Reload 和 Secret 轮换

reload 用于在不切换 artifact 的情况下应用配置或 secret 变化。它不能替代热卸载，也不能保证所有插件都能安全热更新。

配置 reload 流程：

1. 保存新 `config_json` 前先做 JSON Schema 校验。
2. 创建配置快照并递增 desired generation。
3. 调用新配置的 `ReloadConfig()` dry run。
4. 如果插件声明 `reload_mode=hot`，在当前实例上调用 reload，并更新 dispatch table metadata。
5. 如果插件不支持 hot reload，标记 `reload_required` 或走重新创建实例流程。
6. connection takeover 已有连接默认继续使用旧配置；新连接使用新配置。

secret 轮换流程：

1. 管理员上传或更新 secret，新值写入 `plugin_secret_versions(state=active)`。
2. 旧值按 rotation 策略进入 `previous`、`expired` 或 `revoked`。
3. Plugin Manager 找到引用该 secret 的 enabled 插件。
4. 如果插件实现 `SecretReloader` 且 manifest `rotation.reload=hot`，调用 `ReloadSecret(ctx, name, oldRef, newRef)`。
5. 如果 hot reload 失败，保持旧 dispatch table，标记 `reload_failed` 并展示 Runbook。
6. 如果策略是 `reload_required` 或 `manual`，只更新 desired state 和管理页提示，不自动调用插件。
7. grace period 结束后清理旧 secret version，并记录审计日志。

connection takeover 插件规则：

- forwarding secret 轮换默认只影响新连接。
- 已认证的长连接不应因为 secret 轮换被强制重放登录流程。
- 如果后端要求同步切换 forwarding secret，应使用 `dual_read` grace period，并在 Runbook 中要求先更新 backend，再更新插件，最后关闭旧版本。
- 如果 secret 泄漏，管理员可以选择立即 revoke old version，并 force-close 受影响的 draining 或 active proxy 连接。

### 删除

删除也称卸载：

1. 如果插件启用中，先执行禁用。
2. 删除 `plugins` 记录。
3. 如果 artifact 未被其他插件记录引用，删除 artifact 记录和文件。
4. 如果当前进程已经加载过该 artifact，运行时标记 `restart_required` 或 `deleted_pending_restart`，提示重启后完全清理。
5. 记录审计日志。

在某些系统上，已加载 `.so` 文件可能无法立即删除。此时保留文件并标记待重启清理。

### 升级和切换版本

升级通过上传新 artifact 并切换 `plugins.artifact_id` 完成。

如果插件当前未启用：

- 直接切换 artifact。
- 下次加载或启用使用新 artifact。

如果插件当前启用：

1. 尝试加载新 artifact。
2. 创建新实例并调用 `ReloadConfig`、`Init`。
3. 如果新实例初始化成功，原子切换 dispatch table。
4. 调用旧实例 `Destroy()`。
5. 如果任何步骤失败，保持旧实例继续工作。

如果同一路径已经被 `plugin.Open` 加载过，不能用覆盖文件的方式升级。所有 artifact 必须使用不同 content-addressed 路径。

### Artifact 和运行时文件保留清理

artifact、source package、构建日志、配置快照和插件运行时文件需要明确保留策略，否则长期运行后会占满磁盘，也会影响回滚能力。

推荐保留策略：

- active artifact 永不自动清理。
- enabled 插件的 desired artifact 永不自动清理。
- 最近 N 个历史 artifact 默认保留，例如每个插件保留 5 个版本。
- 被配置快照引用的 artifact 默认保留。
- 已加载过的 Go plugin artifact 即使删除 DB 引用，也只能标记 `deleted_pending_restart`，重启后再清理文件。
- source package 可以按时间保留，例如 30-90 天，或在成功构建后只保留 sha256 和 provenance。
- build log 默认保留截断摘要，完整日志按时间或大小清理。
- SBOM、LICENSE、manifest 和 provenance 应随 artifact 一起保留。
- `runtime/<plugin>/data/` 默认保留，删除插件时由管理员选择保留或删除。
- `runtime/<plugin>/cache/` 可以按 manifest retention、配额和最近访问时间自动清理。
- `runtime/<plugin>/tmp/` 可以在 gateway 启动、插件 disable、artifact switch 或 action 完成后清理。
- `runtime/<plugin>/logs/` 和 `diagnostics/` 按日志/诊断保留策略清理，并必须先完成脱敏。

GC 流程：

1. 扫描 `plugin_artifacts`、`plugins`、`plugin_config_snapshots` 和 runtime loaded registry。
2. 标记 active、desired、snapshot referenced、loaded 的 artifact 为 protected。
3. 对超过保留策略且未 protected 的 artifact 标记 candidate。
4. 扫描 `plugin_file_resources` 和 runtime 目录，计算 data/cache/tmp/log/diagnostic 的可清理候选。
5. 管理页展示 candidate、namespace、data class、retention、是否可丢弃和释放空间预估。
6. 管理员确认后删除 DB 记录和可删除文件；cache/tmp 可以按策略自动清理。
7. 删除失败或已加载文件保留为 pending cleanup。
8. 下次启动时再次执行 pending cleanup。

GC 必须写审计日志。GC 不应删除当前启用插件需要的 secret、plugin_data、`runtime/<plugin>/data/` 或配置快照，除非管理员显式选择。

## 发布、回滚和恢复

### 发布流程

推荐发布流程：

1. 上传新 `.mcgp`。
2. 如果是源码包，等待构建成功。
3. 查看 manifest、capabilities、dependencies、runtime limits 和构建信息。
4. 对目标配置执行 schema 校验和 `ReloadConfig()` dry run。
5. 如果需要配置迁移，展示迁移预览。
6. 管理员确认后切换 `plugins.artifact_id`。
7. 尝试加载新 artifact。
8. 初始化新实例并原子切换 dispatch table。
9. 旧实例进入 draining。
10. 发布结果写入审计日志。

发布过程中任一步失败，都必须保留旧 active artifact 和旧 dispatch table。

### 回滚

回滚不是重新上传旧文件，而是把 `plugins.artifact_id` 切回已有 artifact：

- 回滚前检查旧 artifact 文件仍存在。
- 回滚前检查当前配置是否能被旧 artifact 接受。
- 回滚前检查旧 artifact 是否能读取当前 `plugin_data` schema，必要时选择数据快照或清理可丢弃数据。
- 如果配置版本已迁移且旧 artifact 不支持新版本，需要管理员选择旧配置快照或手工编辑。
- 回滚成功后，新实例进入 draining，旧版本重新成为 active。
- 回滚操作写入审计日志。

需要保存配置快照：

```sql
CREATE TABLE plugin_config_snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    config_json TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    priority INTEGER NOT NULL DEFAULT 100,
    scope_json TEXT NOT NULL DEFAULT '{}',
    rollout_json TEXT NOT NULL DEFAULT '{}',
    runtime_limits_json TEXT NOT NULL DEFAULT '{}',
    features_json TEXT NOT NULL DEFAULT '{}',
    policy_snapshot_json TEXT NOT NULL DEFAULT '{}',
    config_hash TEXT NOT NULL DEFAULT '',
    scope_hash TEXT NOT NULL DEFAULT '',
    rollout_hash TEXT NOT NULL DEFAULT '',
    runtime_limits_hash TEXT NOT NULL DEFAULT '',
    features_hash TEXT NOT NULL DEFAULT '',
    policy_hash TEXT NOT NULL DEFAULT '',
    desired_fingerprint TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_plugin_config_snapshots_plugin_time
    ON plugin_config_snapshots(plugin_id, created_at);
```

配置快照用于回滚和审计，不保存 secret 明文。严格说它保存的是一次 desired state 的发布输入摘要，而不只是 `config_json`：

- `artifact_id` 用于恢复当时使用的 artifact。
- `config_json/config_version` 用于恢复配置。
- `priority/scope_json/rollout_json/runtime_limits_json` 用于恢复排序、流量范围和运行时限制。
- `features_json/policy_snapshot_json` 用于解释当时的 feature 协商和策略判断。
- hash 字段用于 review、diff、drift 和审计关联。

配置快照创建时机：

- 每次修改 config、scope、rollout、priority、runtime limits 或 artifact 前。
- 每次成功 enable、rollback、promotion apply 前后可以各保存一份，便于恢复。
- 配置迁移前必须保存旧版本快照；迁移成功后保存新版本快照。

配置快照回滚规则：

- 回滚前必须重新执行当前准入策略、feature 协商、secret mapping、config schema 校验和 `ReloadConfig()` dry run。
- 如果快照引用的 artifact 缺失、被 revoked 或 Go/API 不兼容，不允许直接应用。
- 如果快照引用的 secret ref 在当前环境缺失，阻断回滚并要求管理员重新映射。
- 可以只回滚 config，也可以回滚完整 desired state；完整回滚必须展示 artifact/scope/rollout/runtime limits diff。
- 回滚应用成功后生成新的 desired generation，而不是把数据库时间倒回旧状态。
- 回滚操作必须创建新的快照和审计日志，保留原始快照不变。

### Canonical Hash 和脱敏 Diff

review、promotion、drift、rollback 和审计都依赖 hash。如果每个接口各自序列化 JSON，会出现同一配置 hash 不一致、审批失效误判或敏感字段泄露。第一版必须定义统一 canonicalization。

canonical JSON 规则：

- 使用 UTF-8 JSON。
- object key 按字节序升序排序。
- 去除无意义空白。
- number 使用 JSON 标准最短表示；不允许 NaN/Inf。
- string 不做 Unicode 归一化以外的业务改写。
- array 保持原顺序，除非 schema 明确声明该字段是 set。
- 缺失字段和显式默认值是否等价由 schema 决定；进入 hash 前应先应用 schema default。
- sensitive 字段不进入明文 diff，但 hash 仍基于真实值或 secret ref 名称计算。

推荐 hash：

```text
sha256(canonical_json)
```

fingerprint 输入：

| Hash | 输入 |
| --- | --- |
| `artifact_hash` | artifact sha256 |
| `config_hash` | canonical config JSON，包含 secret ref 名称，不包含 secret 明文 |
| `scope_hash` | canonical scope JSON |
| `rollout_hash` | canonical rollout JSON |
| `runtime_limits_hash` | canonical runtime limits JSON |
| `features_hash` | required/optional feature 协商结果摘要 |
| `policy_hash` | policy snapshot canonical JSON |
| `desired_fingerprint` | artifact、config、scope、rollout、priority、runtime limits、features、policy 的组合 hash |

组合 hash 使用带字段名的结构，避免简单字符串拼接歧义：

```json
{
  "artifact": "sha256:...",
  "config": "sha256:...",
  "scope": "sha256:...",
  "rollout": "sha256:...",
  "priority": 100,
  "runtime_limits": "sha256:...",
  "features": "sha256:...",
  "policy": "sha256:..."
}
```

脱敏 diff 规则：

- 普通字段显示 old/new。
- `x-mc-gateway-sensitive=true` 字段只显示 `unchanged` 或 `changed`。
- `x-mc-gateway-secret-ref` 字段显示 secret ref 名称变化，不显示 secret value。
- 未知字段按 conservative 处理；如果 config schema 不认识该字段，Admin diff 默认只显示字段存在性和 hash 变化。
- 外部 endpoint 可显示 host 和 scheme，query string 默认脱敏。
- 玩家名、UUID、source IP、session response、token 和 packet payload 不进入 diff。

示例 diff：

```json
{
  "config": {
    "auth_mode": { "old": "offline", "new": "mojang" },
    "forwarding_secret_ref": { "old": "velocity_v1", "new": "velocity_v2" },
    "external_auth_token": { "old": "changed", "new": "changed", "sensitive": true }
  },
  "scope": {
    "hosts": { "added": ["play.example.com"], "removed": [] }
  }
}
```

使用规则：

- review 必须绑定 artifact hash、config hash、scope hash、rollout hash、runtime limits hash、features hash 和 policy hash。
- promotion bundle 必须携带各 hash 和 desired fingerprint。
- drift 检测必须使用 canonical hash，不依赖版本字符串或原始 JSON 字节。
- 审计日志记录 hash 和脱敏 diff 摘要，不记录敏感字段明文。
- CLI diff、Admin diff 和 promotion import 必须使用同一 canonicalization 实现。

### 备份和恢复

备份范围：

- SQLite 数据库。
- plugin artifact 目录。
- source package 目录。
- build log 目录。
- plugin data。
- plugin runtime data 目录。
- secret 密文和 key metadata。

恢复规则：

- 恢复后先做 artifact 文件存在性和 sha256 校验。
- 缺失 artifact 的插件标记为 `failed`，不自动启用。
- secret 无法解密时，相关插件标记为 `failed`，要求管理员重新配置 secret。
- runtime data 恢复后应重新计算 `plugin_file_resources` 摘要、配额使用和 orphaned 状态。
- cache/tmp/log/diagnostics 缺失不应阻断启用；data 目录缺失时按插件 manifest 的 `file_storage.data.required` 或插件 HealthCheck 结果决定是否阻断。
- 已加载但 DB 中删除的 artifact 只能在当前进程继续存在，重启后消失。
- 恢复操作必须写入审计日志。

备份不应包含明文 secret。若使用外部 KMS，必须同时记录 KMS key id 和恢复步骤。

## 环境迁移、导入导出和配置漂移

备份恢复面向同一个部署环境的灾难恢复；环境迁移和 promotion 面向 dev、staging、prod 等不同环境之间的受控交付。生产环境不应通过直接复制 SQLite 或插件目录来完成发布，因为不同环境的 upstream、scope、rollout、secret、KMS 和网络策略通常不同。

### 环境模型

每个 gateway 实例或集群应有一个稳定的环境标识：

```json
{
  "environment_id": "prod-cn-1",
  "environment_type": "prod",
  "cluster_id": "gateway-prod-a",
  "plugin_dir": "/var/lib/mc-gateway/plugins"
}
```

环境标识用于：

- 写入导出 bundle 的 provenance，说明源环境。
- 在导入时判断是否跨环境。
- 区分 dev/staging/prod 的默认发布门禁。
- 生成配置漂移对比报告。
- 避免把仅适用于某环境的 scope、upstream 或 secret ref 误用到另一个环境。

### Promotion Bundle

跨环境交付应导出“期望状态”，而不是运行时状态。导出 bundle 可以是 zip 包或目录，不需要额外定义插件包扩展名；bundle 内通过 manifest 声明类型和 schema version。

推荐结构：

```text
promotion-bundle/
  manifest.json
  artifacts/
    sha256-<artifact>.mcgp
  plugins/
    <plugin-id>.desired.json
  checksums.txt
  README.md
```

`manifest.json` 示例：

```json
{
  "schema_version": "mc-gateway.plugin-promotion/v1",
  "source_environment": {
    "environment_id": "staging-cn-1",
    "environment_type": "staging",
    "cluster_id": "gateway-staging-a"
  },
  "created_at": 1730000000,
  "created_by": "admin",
  "plugins": [
    {
      "id": "mc-auth-proxy",
      "version": "0.2.0",
      "artifact_id": "sha256:...",
      "artifact_sha256": "...",
      "desired_state_path": "plugins/mc-auth-proxy.desired.json",
      "artifact_path": "artifacts/sha256-....mcgp",
      "config_hash": "sha256:...",
      "secret_refs": [
        "velocity_forwarding_secret",
        "third_party_yggdrasil_public_key"
      ]
    }
  ]
}
```

`plugins/<plugin-id>.desired.json` 保存可迁移的期望状态：

```json
{
  "plugin_id": "mc-auth-proxy",
  "artifact_id": "sha256:...",
  "enabled": true,
  "priority": 50,
  "scope": {
    "hosts": ["play.example.com"],
    "route_tags": ["auth-required"]
  },
  "rollout": {
    "percentage": 10,
    "sticky_key": "source_ip"
  },
  "dry_run": false,
  "config_version": 3,
  "config": {
    "match_hosts": ["play.example.com"],
    "backend": {
      "default": "backend.prod.internal:25566",
      "forwarding": {
        "mode": "velocity-modern",
        "secret_ref": "velocity_forwarding_secret"
      }
    }
  },
  "runtime_limits": {
    "handler_timeout_ms": 3000,
    "max_active_connection_sessions": 1024
  }
}
```

导出内容规则：

- 必须包含 plugin desired state、artifact 引用、manifest、config、scope、rollout、runtime limits、config version 和 provenance。
- 可以选择内嵌 artifact 包；离线环境推荐 full bundle，在线环境可以只导出 artifact sha256 和仓库引用。
- 不导出 secret 明文、secret 密文、KMS key、运行时 goroutine 状态、连接状态、metrics、日志或构建环境变量。
- 默认不导出 `plugin_data`。只有插件显式声明某些数据是可迁移配置数据时，才允许导出，并需要单独标记数据分类。
- 导出的 artifact 必须以 sha256 锁定；导入环境不能用同版本号但 sha256 不同的 artifact 静默替换。
- bundle 自身应包含 `checksums.txt`，导入前先校验完整性。

当前实现先采用更保守的本地 target apply 边界：promotion bundle 不导出 config 明文，只导出 `config_hash`、artifact/runtime/API/provenance 和 secret ref 摘要。目标环境 apply 时必须由目标侧请求提供 config，系统按 canonical hash 与 bundle `config_hash` 比对，再执行 `DryRunConfig` 和当前 governance gate；校验通过后只写入本地 desired state，不自动 enable active 流量。CLI `gateway plugin apply <bundle> --target-gateway ...` 会先按 bundle `artifact_sha256` 从源网关下载 artifact package、上传到目标网关，再调用目标侧 apply；完整环境覆盖、repository apply 跨节点编排和自动集群级 apply 仍属于后续实现。

### 环境覆盖和 Secret 映射

跨环境导入不能假设配置完全相同。应支持导入时提供环境覆盖：

```json
{
  "target_environment": {
    "environment_id": "prod-cn-1",
    "environment_type": "prod"
  },
  "secret_mapping": {
    "velocity_forwarding_secret": "prod_velocity_forwarding_secret",
    "third_party_yggdrasil_public_key": "prod_yggdrasil_public_key"
  },
  "config_overrides": {
    "mc-auth-proxy": {
      "backend.default": "backend.prod.internal:25566",
      "rollout.percentage": 5
    }
  }
}
```

覆盖规则：

- secret mapping 只映射 ref 名称，不复制 secret 值。
- 目标环境中缺失的 secret 必须阻断启用，不能 fallback 到源环境 secret 名称。
- backend address、外部 API endpoint、scope host、rollout percentage、网络出口策略和 runtime limits 可以作为环境差异覆盖。
- 覆盖后必须重新计算 config hash，并在审计日志中记录覆盖字段名，但不记录敏感值。
- 如果覆盖字段不在 manifest config schema 中，导入 dry-run 必须报错。
- prod 环境默认要求二次确认；高风险字段变更，例如 artifact、secret ref、scope 扩大、rollout 从小流量变全量，应进入发布门禁。

### 导入流程

推荐导入流程：

1. 上传 promotion bundle。
2. 静态校验 bundle manifest、checksums、artifact sha256 和 zip path。
3. 校验 artifact 与当前 gateway 的 GOOS、GOARCH、Go 版本、API 版本和 extension point 兼容性。
4. 对每个插件生成目标环境 diff。
5. 要求管理员完成 secret mapping 和环境覆盖。
6. 执行 config schema 校验、配置迁移 dry-run、`ReloadConfig()` dry-run 和 HealthCheck dry-run。
7. 展示发布门禁结果和风险摘要。
8. 管理员确认后把 artifact 登记到本地 `plugin_artifacts`，写入或更新 `plugins` desired state。
9. 如果导入请求要求启用，按正常 load/enable 流程发布 dispatch table。
10. 写入导入、配置覆盖、secret mapping、启用结果和失败原因的审计日志。

导入失败必须保持目标环境旧 desired state 和旧 dispatch table 不变。bundle 导入后可以只进入 staged 状态，由管理员后续手动启用。

### Diff 和漂移检测

系统应能计算插件期望状态指纹，用于跨环境对比和本环境漂移检测：

```text
desired_fingerprint = hash(
  plugin_id,
  artifact_sha256,
  config_version,
  config_hash,
  scope_hash,
  rollout_hash,
  priority,
  dry_run,
  runtime_limits_hash,
  features_hash,
  policy_hash
)
```

不进入指纹的内容：

- desired_generation。
- updated_at、updated_by。
- runtime state、metrics、health、draining 状态。
- secret 明文、secret 密文和 KMS metadata。
- 环境标识本身。

所有 hash 必须使用前文 Canonical Hash 规则计算。promotion import 做环境覆盖后，必须重新计算 config/scope/rollout/runtime_limits hash 和 desired fingerprint。

漂移状态建议：

| 状态 | 说明 |
| --- | --- |
| `identical` | artifact sha256、config、scope、rollout、priority 和 runtime limits 一致 |
| `artifact_drift` | plugin ID/version 相同但 artifact sha256 不同，必须高风险提示 |
| `config_drift` | config hash 不同 |
| `scope_drift` | scope 或 rollout 不同 |
| `runtime_limits_drift` | timeout、并发、资源限制不同 |
| `missing_secret` | 目标环境缺少必需 secret ref |
| `incompatible_runtime` | Go/API/runtime/extension point 不兼容 |
| `missing_artifact` | 目标环境没有对应 artifact，也无法从 bundle 或仓库导入 |
| `unknown` | 缺少源环境基线或指纹版本不匹配 |

漂移检测用途：

- staging 到 prod promotion 前展示差异。
- 生产环境手工修改后提示偏离已发布基线。
- 多实例模式下比较 global desired state 和各节点实际 artifact/runtime state。
- 灾备演练后确认恢复环境是否与基线一致。

### 灾备演练

插件系统需要提供可演练的恢复路径，而不是只写备份规则：

- 定期在空环境导入最近一次 promotion bundle 或备份副本。
- 校验 artifact sha256、manifest metadata 和 config hash。
- 重新绑定目标环境 secret，不复制源环境 secret。
- 执行插件 load dry-run、config dry-run 和 health check。
- 对 connection takeover 插件运行最小 smoke test，例如握手、登录失败响应和 backend dial。
- 生成演练报告，列出缺失 artifact、缺失 secret、版本不兼容和配置漂移。

灾备演练不应默认启用插件处理真实流量。只有管理员明确确认后，才可以把演练环境切为 active。

## 作用域、灰度和 Dry Run

插件启用不应只有“全局开/关”一种形态。gateway core 应在调用插件前提供统一作用域过滤和灰度策略，避免每个插件重复实现基础分流，也避免安全插件因为配置错误影响全站。

`plugins.scope_json` 描述插件适用范围：

```json
{
  "hosts": ["play.example.com", "*.example.net"],
  "route_ids": ["route-main"],
  "route_tags": ["auth-required"],
  "source_cidrs": ["10.0.0.0/8"],
  "protocol_versions": [765, 766],
  "server_names": ["lobby", "survival"]
}
```

scope 规则：

- 空 scope 表示对该 extension point 的所有请求生效。
- gateway 在 dispatch 前先做 scope 过滤；不匹配的插件不进入 handler 调用，也不计入 handler error。
- host 通配、CIDR 和 route tag 解析由 gateway core 提供稳定语义。
- 插件仍可以在业务逻辑里做更细粒度判断，但不应依赖未文档化的 gateway 内部状态。
- scope 变更需要原子发布新 dispatch table，不应影响已有 connection takeover 连接。

`plugins.rollout_json` 描述灰度策略：

```json
{
  "enabled": true,
  "percentage": 10,
  "sticky_key": "source_ip",
  "allowlist": ["203.0.113.0/24", "play.example.com"],
  "denylist": [],
  "start_at": 0,
  "end_at": 0
}
```

rollout 规则：

- 第一版可以只支持 percentage + sticky key；allowlist、denylist 和时间窗口预留。
- gateway 侧 sticky key 只能使用 source IP、host、route ID、route tag、protocol version、transport、service name 和 upstream protocol 等 core 已知元数据。
- 玩家名、UUID 或认证结果不能作为 gateway 侧 rollout sticky key；玩家维度灰度只能由 connection takeover 插件在其内部实现，或等待未来专门的 Minecraft 协议 extension point。
- percentage 必须用稳定 hash 计算，避免同一来源或 host 在短时间内反复切换路径。
- 对 first-match hook，未命中 rollout 的插件等价于 pass，不应调用 handler。
- 灰度策略变更必须写审计日志，并在 Admin 页面显示当前生效范围。
- transport、service name 和 upstream protocol scope 必须来自 gateway 解析后的枚举值，不能直接使用用户可控字符串。

`dry_run` 用于评估插件决策但不改变连接结果：

- dry-run 插件可以被调用，但其 handled/reject/error 结果只记录日志和指标，不影响实际连接。
- dry-run 不适合 connection takeover mode，因为插件一旦接管 `net.Conn` 就会改变数据路径。对 connection takeover 插件，dry-run 应禁止启用或只允许调用轻量 `Evaluate()` 接口。
- dry-run 的指标必须带 `dry_run=true` 维度或在结果中明确标记，避免和真实处理混淆。
- 安全类插件从 dry-run 切换到 enforced 模式需要管理员确认。

## 运行时治理

插件运行时必须可观测、可限流、可熔断。否则 trusted plugin 的能力越强，越容易把 gateway 主路径拖死。

### 调用边界

每次 extension point 调用都需要记录：

- plugin ID
- artifact ID
- extension point key
- handler ID
- 开始时间和耗时
- 返回结果：handled、pass、reject、error、panic、timeout
- 错误摘要

连接路径调用插件时：

- 不能持有全局插件锁。
- 必须从只读 dispatch table 快照读取 handler。
- 每个 handler 调用必须有 panic recover。
- 每个 handler 可以配置超时。
- 观测类 event subscriber 默认异步执行，不能阻塞连接路径。

### 超时策略

不同 extension point 的默认超时策略不同：

| 类型 | 默认超时后行为 |
| --- | --- |
| first-match hook | 记录 timeout，继续下一个 handler 或按配置 fail closed |
| chain middleware | 按配置 fail open 或 fail closed |
| provider | 当前 provider 失败后尝试 fallback provider；没有 fallback 则返回错误 |
| event subscriber | 丢弃本次事件并记录 timeout |
| connection takeover mode | 插件接管连接后由插件负责协议级超时；gateway 只管理连接生命周期和指标 |

安全相关插件可以配置 `fail_closed=true`。观测类插件默认 `fail_open=true`。

### 并发和背压

每个插件和每个 extension point 都需要可配置并发限制：

- 最大并发 handler 调用数。
- 最大等待队列长度。
- 队列满时的行为：pass、reject、drop event 或 fail closed。
- connection takeover mode 的最大活跃连接数。

当插件达到并发上限时，gateway 不能无限创建 goroutine 或无限缓存事件。

### 熔断

Plugin Manager 需要维护运行时错误计数：

- 连续错误次数。
- 最近窗口错误率。
- timeout 次数。
- panic 次数。
- 平均和 P95/P99 延迟。

超过阈值后可以：

- 标记 degraded。
- 临时跳过该插件。
- 自动禁用该插件。
- 仅对新连接禁用，保留已有 connection takeover 连接。
- 写入审计日志和管理页告警。

熔断策略必须可配置，默认不应因为观测类插件失败而中断玩家连接。

### 性能预算

插件运行在 gateway 主路径时必须有明确性能预算。默认值应保守，允许管理员按插件覆盖。

建议默认预算：

| 场景 | 默认预算 | 超出处理 |
| --- | --- | --- |
| `legacy upstream-connect contract` route.resolve/v1 provider handler | P95 < 10ms，不含插件自建上游连接耗时 | 记录 slow call，计入熔断窗口 |
| `connection.filter/v1` | P95 < 2ms | 超时按 fail open/closed 策略 |
| `route.resolve/v1` | P95 < 5ms | fallback 到默认路由或 fail closed |
| event subscriber | 单事件处理 < 100ms | 超时丢弃本次事件 |
| HealthCheck | 1-3s | 标记 degraded 或 not_ready |
| background task | 由任务声明，默认 30s | 取消 context，记录失败 |
| connection takeover active connection | 不设固定处理耗时 | 受 active connection、idle timeout 和 health 管理 |

预算规则：

- 连接路径预算只统计 gateway 调用插件 handler 的时间。
- route.resolve/v1 provider 中插件如果负责拨号，拨号耗时应单独记录为 plugin-owned dial duration。
- connection takeover mode 是长连接，不用 handler latency 评价整体性能，应看 active connections、吞吐、错误率和 idle timeout。
- 默认慢调用不会立即中断连接，但会进入指标、日志和告警。
- 安全类插件可以配置 fail closed，但必须显式展示风险。

### 性能基准和容量规划

性能预算定义的是运行期阈值，性能基准定义的是上线前证据。插件进入生产前，尤其是 connection takeover、认证、路由和 provider 类插件，应能证明它在目标容量下不会明显拖慢连接路径。

基准类型：

| 类型 | 目标 | 适用插件 |
| --- | --- | --- |
| micro benchmark | 测单个 handler 纯逻辑开销 | route、filter、policy、provider |
| integration benchmark | 测 gateway 调用插件的完整开销 | 所有连接路径插件 |
| protocol smoke benchmark | 测握手、登录失败、backend dial 等端到端路径 | connection takeover |
| soak test | 长时间运行，观察 goroutine、内存、连接和错误漂移 | connection takeover、后台任务 |
| regression benchmark | 和上一 artifact 或基线比较 | 升级、promotion、回滚前 |

建议基准指标：

- handler P50/P95/P99 latency。
- calls/sec 或 connections/sec。
- active calls。
- active proxy connections。
- bytes in/out per connection。
- goroutine 增长。
- heap 增长和 GC pause 摘要。
- timeout/error/panic rate。
- backend dial duration。
- external dependency latency，例如 session server。
- shutdown/drain duration。

connection takeover 插件不能只看 handler latency。它需要额外关注：

- 最大并发连接数。
- 每连接平均 goroutine 数。
- 每连接内存占用估算。
- 长连接 idle timeout 行为。
- backend 半关闭和异常断开行为。
- 认证源慢响应时的排队和超时。
- draining 时现有连接收敛时间。

容量估算可以使用保守公式：

```text
max_new_conn_per_sec = min(
  gateway_accept_capacity,
  plugin_handler_concurrency / handler_p99_seconds,
  external_dependency_budget
)

max_takeover_connections = min(
  runtime_limits.max_active_connection_sessions,
  memory_budget_bytes / estimated_bytes_per_connection,
  fd_budget / fds_per_connection
)
```

估算规则：

- handler P99 必须按目标机器、目标 Go 版本和目标 runtime 测量。
- connection takeover 的每连接资源必须用 soak test 估算，不能只凭代码审查。
- 外部认证源、CMDB、仓库和监控 API 必须单独设置 dependency timeout 和并发上限。
- 如果插件依赖远程服务，应记录 fail open/closed 策略对容量的影响。
- benchmark 结果只作为基线，不替代生产监控和灰度。

发布门禁中的性能检查：

- 新插件没有基线时，至少跑 smoke benchmark 和配置 dry-run。
- connection takeover 插件应跑最小并发连接 benchmark。
- artifact 升级时与上一 active artifact 比较 P95/P99、错误率和资源占用。
- 如果 P99 退化超过阈值，例如 20%，进入高风险确认。
- 如果超过 runtime limits 或触发 goroutine/连接泄漏检测，应阻断启用。
- promotion 到 prod 时应展示 staging 基准和目标环境容量差异。

benchmark 结果应保存为 artifact 附属元数据：

```json
{
  "artifact_id": "sha256:...",
  "benchmark_profile": "staging-small",
  "gateway_version": "0.1.0",
  "go_version": "go1.24.4",
  "machine": "4c8g",
  "results": {
    "handler_p95_ms": 4.2,
    "handler_p99_ms": 8.7,
    "conn_per_sec": 1200,
    "max_active_connection_sessions": 5000,
    "estimated_bytes_per_connection": 32768
  }
}
```

管理页不需要展示完整压测日志，但应展示摘要、基线对比、退化比例和是否通过门禁。完整 benchmark report 可以作为诊断附件下载，并需要脱敏外部地址、玩家名和 token。

### 告警规则

第一版可以先在 Admin 页面展示告警，后续再接 Prometheus/OpenTelemetry Alert。

推荐告警：

| 告警 | 条件 | 建议动作 |
| --- | --- | --- |
| PluginPanicHigh | panic count > 0 或连续 panic 超阈值 | 自动 degraded，必要时 auto-disable |
| PluginTimeoutHigh | timeout rate 超过阈值 | degraded，检查外部依赖 |
| PluginErrorRateHigh | error rate 超过 runtime_limits | degraded 或熔断 |
| PluginLatencyHigh | P95/P99 超过性能预算 | slow call 告警 |
| PluginActiveProxyHigh | active proxy connections 接近上限 | 拒绝新连接或扩容 |
| PluginHealthNotReady | HealthCheck not_ready | 阻止启用或从 dispatch 移除 |
| PluginBackgroundTaskFailing | 后台任务连续失败 | degraded |
| PluginBuildFailed | 构建失败 | 阻止切换 artifact |
| PluginNodePartialFailure | 多实例部分节点失败 | 暂停 rollout |
| PluginDiskUsageHigh | artifact/source/log 占用超阈值 | 运行 GC |

告警规则必须支持按插件配置静默窗口，避免升级、维护和测试期间产生噪声。静默操作也应记录审计日志。

### 后台任务

部分插件需要在连接路径之外执行后台工作，例如同步外部路由、刷新认证缓存、探测后端健康、定期上报指标或清理私有数据。第一版可以不提供通用 scheduler UI，但 SDK 和 Plugin Manager 应预留受控后台任务模型。

推荐接口：

```go
type BackgroundTaskRegistration struct {
    ID          string
    Schedule    BackgroundTaskSchedule
    Timeout     time.Duration
    RunOnStart  bool
    NonReentrant bool
    Jitter      time.Duration
    MaxFailures int
    Handler     func(context.Context) error
}

type BackgroundTaskSchedule struct {
    Type     string // interval/cron/manual
    Interval time.Duration
    Cron     string
}

type Gateway interface {
    RegisterBackgroundTask(task BackgroundTaskRegistration) error
}
```

manifest 也应能声明后台任务，便于 Admin 在启用前展示：

```json
{
  "background_tasks": [
    {
      "id": "refresh-profile-cache",
      "schedule": { "type": "interval", "interval_seconds": 300 },
      "timeout_ms": 30000,
      "run_on_start": true,
      "non_reentrant": true,
      "jitter_ms": 30000,
      "max_consecutive_failures": 3,
      "manual_trigger": true
    },
    {
      "id": "rebuild-route-cache",
      "schedule": { "type": "manual" },
      "timeout_ms": 60000,
      "manual_trigger": true
    }
  ]
}
```

后台任务规则：

- 任务只能在插件 enabled 后运行，disable 时必须停止调度并等待当前任务结束或超时。
- 每个任务必须有 timeout、panic recover、并发限制和最近错误摘要。
- 同一个 task 默认不允许重入；上一次未完成时跳过本轮并记录 skipped。
- 后台任务不能阻塞连接路径，不能持有 dispatch table 更新锁。
- 任务失败会计入插件健康状态和指标，但默认不直接中断已有连接。
- 任务需要使用 gateway 提供的 context，disable、shutdown 或 artifact switch 时应取消 context。
- `interval` 任务应支持 jitter，避免多插件或多节点同时打外部依赖。
- `cron` 第一版可以只预留 schema；如果实现，必须明确时区、错过窗口和夏令时语义。
- `manual` 任务只能由 Admin/API 触发，必须有权限、confirm token、timeout、审计和幂等键。
- task ID、schedule、timeout 或手动触发能力变化属于运行时行为变化，启用前应展示 diff。
- 任务结果只能保存摘要，不能保存 secret、token、完整外部 response、完整 packet payload 或未脱敏玩家隐私。

状态模型：

```text
registered -> scheduled -> running -> succeeded
                              |-> failed
                              |-> skipped
                              |-> canceled
                              |-> timed_out
```

状态字段建议：

| 字段 | 说明 |
| --- | --- |
| `task_id` | 插件内稳定任务 ID |
| `state` | registered/scheduled/running/succeeded/failed/skipped/canceled/timed_out |
| `last_started_at` | 最近开始时间 |
| `last_finished_at` | 最近结束时间 |
| `next_run_at` | 下一次计划运行时间，manual 任务为空 |
| `consecutive_failures` | 连续失败次数 |
| `last_error_kind` | 低基数错误类型 |
| `last_result_summary` | 脱敏摘要 |

多实例规则：

- 第一版单实例 gateway 内，任务只在本节点运行。
- 多实例模式下必须选择任务运行策略：`per_node`、`singleton` 或 `sharded`。
- `singleton` 和 `sharded` 任务需要 lease，避免多个节点重复执行同步、清理或外部写操作。
- lease 丢失时，任务 context 必须取消；handler 应尽快退出。
- 任务状态页面需要区分 global desired schedule 和 node runtime state。

当前实现状态：

- `background_tasks[].run_policy` 支持 `per_node`、`singleton`、`sharded`；未配置时默认为 `per_node`。
- `singleton` 使用全局 shard，`sharded` 使用 `background_tasks[].shard_key`，未配置 shard 时使用 `default`。
- `singleton` 和 `sharded` 通过 Admin SQLite 中的 `plugin_task_leases` 获取、续租和释放 lease；续租失败会取消任务 context。
- task summary 会展示 `node_id`、`lease_required`、`lease_acquired`、`lease_owner`、`lease_expires_at` 和 `lease_skipped`。
- node state、local artifact package mirror 和 partial rollout failure 已有可展示状态；远端 artifact 传输和 CLI promotion 跨网关 apply 编排已落地。repository apply 跨节点编排、自动 artifact 分发和自动集群级 apply 仍是 M9 后续工作，不因 task lease 可用而视为完成。

推荐指标：

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `plugin_background_task_runs_total` | counter | `plugin_id`、`task_id`、`result` | 后台任务执行次数 |
| `plugin_background_task_duration_seconds` | histogram | `plugin_id`、`task_id` | 后台任务耗时 |
| `plugin_background_task_skipped_total` | counter | `plugin_id`、`task_id`、`reason` | 任务跳过次数 |
| `plugin_background_task_consecutive_failures` | gauge | `plugin_id`、`task_id` | 当前连续失败次数 |
| `plugin_background_task_next_run_timestamp` | gauge | `plugin_id`、`task_id` | 下一次计划运行时间 |

### 健康检查

插件应能暴露健康状态，帮助管理员判断是否可以启用、是否需要降级或是否可以切流。

推荐接口：

```go
type HealthStatus string

const (
    HealthReady    HealthStatus = "ready"
    HealthDegraded HealthStatus = "degraded"
    HealthNotReady HealthStatus = "not_ready"
)

type HealthCheck interface {
    Health(ctx context.Context) (HealthStatus, string)
}
```

健康状态语义：

| 状态 | 含义 | 默认处理 |
| --- | --- | --- |
| `ready` | 插件已初始化，依赖可用，可以处理新流量 | 正常参与 dispatch |
| `degraded` | 插件可运行但外部依赖或后台任务异常 | 管理页告警，是否继续处理由策略决定 |
| `not_ready` | 插件依赖缺失、配置无效或初始化未完成 | 不应启用或不应接收新流量 |

健康检查规则：

- enable 前如果插件实现 HealthCheck，应在 `ReloadConfig()` 和 `Init()` 后执行一次 readiness check。
- 健康检查必须有短超时，例如 1-3 秒。
- connection takeover 插件如果外部认证源不可用，可以按配置 fail open、fail closed 或 degraded。
- 健康状态变化应更新 runtime state、指标和审计/事件。
- Admin 页面需要显示最近一次 health check 时间、状态、摘要和失败原因。

### 指标

Plugin Manager 至少应维护内存态指标，并通过 Admin metrics 页面/API 暴露。后续接入 Prometheus 或 OpenTelemetry 时沿用同一语义。

推荐指标：

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `plugin_handler_calls_total` | counter | `plugin_id`、`extension_point`、`handler_id`、`result` | handler 调用次数 |
| `plugin_handler_duration_seconds` | histogram | `plugin_id`、`extension_point`、`handler_id` | handler 调用耗时 |
| `plugin_errors_total` | counter | `plugin_id`、`extension_point`、`error_kind` | 插件返回错误或内部治理错误 |
| `plugin_panics_total` | counter | `plugin_id`、`extension_point`、`handler_id` | handler panic 次数 |
| `plugin_timeouts_total` | counter | `plugin_id`、`extension_point`、`handler_id` | handler 超时次数 |
| `plugin_active_calls` | gauge | `plugin_id`、`extension_point` | 当前运行中的 handler 调用数 |
| `plugin_active_connection_sessions` | gauge | `plugin_id` | connection takeover mode 当前连接数 |
| `plugin_backpressure_drops_total` | counter | `plugin_id`、`extension_point`、`reason` | 背压导致的 pass、reject 或 drop 次数 |
| `plugin_circuit_breaker_state` | gauge | `plugin_id` | 0=closed，1=open，2=half-open |
| `plugin_scope_matches_total` | counter | `plugin_id`、`extension_point`、`result` | scope/rollout 命中或跳过次数 |
| `plugin_health_status` | gauge | `plugin_id` | 0=not_ready，1=degraded，2=ready |
| `plugin_resource_limit_hits_total` | counter | `plugin_id`、`limit` | 应用层资源限制命中次数 |
| `plugin_alerts_total` | counter | `plugin_id`、`alert`、`severity` | 插件告警触发次数 |
| `plugin_slow_calls_total` | counter | `plugin_id`、`extension_point`、`handler_id` | 超过性能预算的调用次数 |
| `plugin_external_dependency_status` | gauge | `plugin_id`、`dependency_id` | 0=not_ready，1=degraded，2=ready |
| `plugin_external_dependency_requests_total` | counter | `plugin_id`、`dependency_id`、`operation`、`result` | 通过受控 ExternalClient 发起的外部调用次数 |
| `plugin_external_dependency_duration_seconds` | histogram | `plugin_id`、`dependency_id`、`operation` | 受控外部调用耗时 |
| `plugin_external_dependency_inflight` | gauge | `plugin_id`、`dependency_id` | 当前正在执行的受控外部调用数 |
| `plugin_external_dependency_rate_limited_total` | counter | `plugin_id`、`dependency_id`、`reason` | dependency 并发、队列或策略限制导致的拒绝次数 |
| `plugin_external_dependency_circuit_state` | gauge | `plugin_id`、`dependency_id` | 0=closed，1=open，2=half-open |
| `plugin_external_dependency_errors_total` | counter | `plugin_id`、`dependency_id`、`error_kind` | 外部依赖调用或健康检查错误 |
| `plugin_node_state` | gauge | `node_id`、`plugin_id`、`state` | 多实例节点状态，未来能力 |
| `plugin_build_duration_seconds` | histogram | `plugin_id`、`builder_type` | 源码包构建耗时 |
| `plugin_build_failures_total` | counter | `plugin_id`、`builder_type`、`reason` | 构建失败次数 |
| `plugin_artifact_load_total` | counter | `plugin_id`、`result` | artifact 加载成功或失败次数 |
| `plugin_benchmark_result` | gauge | `plugin_id`、`profile`、`metric` | 最近一次 benchmark 摘要指标，低基数 |
| `plugin_events_total` | counter | `plugin_id`、`event_name`、`severity` | 插件业务事件数量，event_name 必须来自 manifest |
| `plugin_events_dropped_total` | counter | `plugin_id`、`reason` | 插件业务事件因限流、队列满、schema 不匹配或脱敏失败被丢弃 |
| `plugin_custom_metric_dropped_total` | counter | `plugin_id`、`metric_name`、`reason` | 自定义指标因未声明、标签非法或超出配额被丢弃 |

指标基数控制：

- 标签不能包含玩家名、host 原文、错误全文或 secret。
- `handler_id` 必须来自插件注册元数据，不能动态生成无限值。
- `transport`、`service_name`、`upstream_protocol` 可以作为低基数标签。
- `listener_port`、`quic_application_protocol` 和 `websocket_path` 默认不作为全局指标标签；如需导出，应先做 allowlist 或 bucket 化。
- 错误详情进入日志和最近错误摘要，指标只记录低基数 `error_kind`。

### 可观测导出

第一版应把“内部指标语义”和“外部导出格式”分开。Plugin Manager 先维护统一指标、事件和 trace 摘要模型，Admin API 读取同一份模型；Prometheus、OpenTelemetry 或其他 exporter 只是输出适配器，不改变指标含义。

导出策略：

| 输出 | 第一版策略 | 说明 |
| --- | --- | --- |
| Admin metrics API | 实现 | 返回当前快照、最近窗口和低基数摘要 |
| Prometheus scrape | 预留或轻量实现 | 使用同一指标名和标签白名单 |
| OpenTelemetry metrics | 预留 | 适合已有 OTel collector 的部署 |
| OpenTelemetry traces | 预留 | 第一版可以只保留内存 trace 摘要和诊断包 |
| 外部 event sink | 预留 event subscriber | 异步弱一致，不进入连接决策路径 |

Prometheus 导出规则：

- metric name 与内部指标保持稳定，必要时增加 `mc_gateway_` 前缀。
- 自动附加 `plugin_id`、`runtime_type`、`extension_point`、`transport`、`upstream_protocol` 等低基数标签。
- 不导出未声明的自定义指标标签。
- histogram bucket 使用 gateway 统一配置；插件 manifest 只能请求有限 profile，例如 `latency_fast`、`latency_external`。
- exporter 失败不影响连接路径，只增加 exporter error 指标和 Admin warning。

OpenTelemetry 导出规则：

- trace/span 仍以 gateway 创建的 connection span、plugin handler span、external dependency span、background task span 为边界。
- 默认不把 W3C trace context 注入第三方认证源、session server 或 Minecraft backend，除非环境策略和插件 manifest 同时允许。
- OTel attributes 只允许低基数字段，例如 plugin ID、extension point、dependency ID、result、error kind、protocol version bucket。
- 玩家名、UUID、source IP 原文、host 原文、secret、token、session response 和 packet payload 不进入 attributes。
- 采样策略由 gateway 控制；插件只能建议 event/metric，不控制全局采样。

事件导出规则：

- 插件业务事件先进入 gateway 的脱敏、schema 校验和限流，再进入 event subscriber 或外部 sink。
- 外部 sink 投递失败不回滚插件业务结果，也不影响 MC 登录连接。
- audit log 是管理操作的真相来源；业务事件 sink 不能替代 audit log。
- event sink 的 at-least-once 可能重复投递，事件需要 `event_id`、`connection_id`、`plugin_id` 和时间戳用于下游去重。

Admin 需要展示：

- exporter 是否启用、最后一次导出成功时间、失败计数和最近错误摘要。
- 每个插件的指标 drop、event drop、label/cardinality 拒绝原因。
- 当前脱敏策略和高基数字段拦截统计。
- Prometheus/OTel 配置只展示摘要，不展示 token、endpoint query 或凭据。

### 插件业务事件和自定义指标

gateway core 不解析 MC 登录、session 校验、身份映射或后续协议状态，但管理页和外部观测系统仍需要看到这些业务结果。应提供受控的插件业务事件 API，让插件自己上报领域事件，gateway 负责脱敏、限流、存储摘要、关联 trace 和转发。

推荐接口：

```go
type EventSeverity string

const (
    EventInfo    EventSeverity = "info"
    EventWarning EventSeverity = "warning"
    EventError   EventSeverity = "error"
)

type PluginEvent struct {
    Name       string
    Severity   EventSeverity
    Subject    string
    Attributes map[string]string
}

type MetricKind string

const (
    MetricCounter   MetricKind = "counter"
    MetricGauge     MetricKind = "gauge"
    MetricHistogram MetricKind = "histogram"
)

type PluginMetric struct {
    Name       string
    Kind       MetricKind
    Value      float64
    Unit       string
    Labels     map[string]string
}

type Gateway interface {
    EmitEvent(ctx context.Context, event PluginEvent) error
    RecordMetric(ctx context.Context, metric PluginMetric) error
}
```

事件命名：

- 推荐格式：`<domain>.<action>`，例如 `auth.success`、`auth.failure`、`backend.dial_failed`、`forwarding.failed`。
- 事件名必须低基数，不能包含玩家名、host、UUID、错误文本或外部 endpoint。
- 插件 manifest 可以声明会产生的事件名、severity 和属性 schema，便于 Admin 页面提前展示。

示例 manifest：

```json
{
  "events": [
    {
      "name": "auth.success",
      "severity": "info",
      "attributes": {
        "auth_mode": "string",
        "forwarding_mode": "string",
        "backend": "string"
      }
    },
    {
      "name": "auth.failure",
      "severity": "warning",
      "attributes": {
        "auth_mode": "string",
        "reason": "enum"
      }
    }
  ],
  "metrics": [
    {
      "name": "auth_attempts_total",
      "kind": "counter",
      "labels": ["auth_mode", "result", "reason"]
    },
    {
      "name": "session_server_latency_seconds",
      "kind": "histogram",
      "labels": ["dependency_id", "result"]
    }
  ]
}
```

事件处理规则：

- `EmitEvent` 不应阻塞主连接路径；gateway 可以入队、采样或丢弃，并记录 drop 计数。
- 事件默认只保留最近摘要，不进入长期审计，除非插件声明 `audit=true` 且管理员允许。
- `Subject` 可以是脱敏主体，例如 hash 后玩家 ID、connection ID 或 backend ID；不能是明文 secret、token 或完整玩家 profile。
- `Attributes` 只允许低基数字符串；高基数字段需要 hash、redact 或放入日志摘要而不是指标标签。
- 插件不得通过事件绕过审计权限；管理操作仍必须使用 gateway 审计日志。
- 事件 schema 变化属于 manifest 变化，需要重新校验和展示。

自定义指标规则：

- `RecordMetric` 不应因为指标后端不可用阻塞主路径；失败时返回可忽略错误并增加 drop/error 计数。
- 指标名在导出时加插件命名空间，例如 `plugin_custom_auth_attempts_total`，并自动附加 `plugin_id`。
- label key 必须在 manifest 中声明；未声明 label 默认拒绝或丢弃。
- label value 不允许玩家名、UUID、source IP、host 原文、完整错误、secret、token 或 session response。
- counter 只能递增，gauge 可以设置当前值，histogram bucket 由 gateway 统一配置或按 manifest 受限声明。
- 自定义指标总量、label 数量和 label value cardinality 需要配额，超出后 drop 并触发 `plugin_resource_limit_hit`。

MC 登录插件示例：

- 登录成功：插件上报 `auth.success` 事件和 `auth_attempts_total{result="success"}`。
- session server 拒绝：插件上报 `auth.failure{reason="session_rejected"}`，并返回 Minecraft login disconnect。
- session server 超时：插件上报 `auth.failure{reason="session_timeout"}` 和 dependency latency metric。
- backend forwarding 失败：插件上报 `forwarding.failed`，但 forwarding secret 不进入事件属性。

这套事件和指标 API 只提供观测出口，不改变 `legacy upstream-connect contract` 的责任划分。gateway core 不消费这些事件来决定 MC 登录结果，也不把事件作为登录流水线的一部分。

### Tracing 和上下文传播

日志、指标和审计回答的是不同问题，但排查一次连接故障时需要把它们串起来。Plugin Manager 应提供统一 trace/context 语义，让一次 client connection、插件 handler、外部依赖调用、backend dial 和 connection takeover 转发能通过 `trace_id`、`connection_id` 和 span 关联。

基本标识：

| 字段 | 说明 |
| --- | --- |
| `connection_id` | gateway 为每个客户端连接生成，日志和诊断中稳定出现 |
| `trace_id` | 一次连接或一次管理操作的追踪 ID，可跨插件和外部依赖传播 |
| `span_id` | 单个操作片段，例如 handler 调用、backend dial、external dependency call |
| `parent_span_id` | 父 span，用于串联调用树 |
| `plugin_id` | 插件 ID |
| `handler_id` | handler ID |
| `extension_point` | extension point key |

span 边界建议：

| Span | 触发时机 |
| --- | --- |
| `gateway.connection` | 新客户端连接进入到连接关闭 |
| `gateway.route.lookup` | route snapshot lookup |
| `plugin.handler` | 每次 extension point handler 调用 |
| `plugin.takeover` | connection takeover 插件接管后的长连接生命周期 |
| `plugin.external_dependency` | 插件访问声明的 external dependency |
| `plugin.backend_dial` | connection takeover 插件连接 backend |
| `plugin.action` | Admin 执行插件 action |
| `plugin.background_task` | 后台任务一次执行 |
| `plugin.health_check` | 健康检查一次执行 |

SDK 可以提供 Tracer 接口：

```go
type Tracer interface {
    StartSpan(ctx context.Context, name string, attrs map[string]string) (context.Context, Span)
}

type Span interface {
    End(err error)
    AddEvent(name string, attrs map[string]string)
}

type Gateway interface {
    Tracer() Tracer
}
```

tracing 规则：

- gateway 调用插件时传入的 `context.Context` 必须携带 trace/span 信息。
- 插件创建后台任务、外部依赖请求和 backend dial 时应沿用传入 context。
- connection takeover 插件接管长连接后，应为连接生命周期创建长 span，并为关键阶段添加事件，例如 auth_start、auth_result、backend_dial、forwarding_start、disconnect。
- external dependency span 的属性只能包含 dependency ID、purpose、error kind、status code class 和 duration，不包含完整 URL query、token、玩家名或 session response。
- trace 采样率必须可配置，默认可以只采样错误、慢调用、管理操作和少量正常请求。
- trace export 是未来能力；第一版可以先在内存最近 trace 摘要和诊断包中展示。

上下文传播边界：

- 对 gateway 内部插件调用，使用 Go `context.Context`。
- 对 HTTP/gRPC external dependency，如果管理员允许，可以注入 W3C `traceparent`；默认不向第三方认证源发送内部 trace ID，避免信息泄露。
- 对 Minecraft backend forwarding，不默认把 trace ID 写入游戏协议；如果插件自定义 forwarding 需要携带 trace，应由插件配置显式开启。
- 对 event subscriber，事件 payload 可包含 trace ID 和 connection ID，但 subscriber 不应修改原 trace。
- 对 sandbox-process，control RPC 和 stream relay metadata 必须携带 trace ID，但不能包含 secret。

隐私和基数控制：

- trace attr 不得包含玩家名、UUID、IP 原文、完整 host、secret、token、session server response 或完整 packet payload。
- 可以使用 hash 或 redacted value，但 hash salt 和保留周期需要组织策略。
- span event 名称必须低基数，不能包含动态玩家名或错误全文。
- Admin 页面默认展示 trace 摘要；完整 trace 需要 admin 权限，并按保留策略清理。

与日志、指标、审计的关系：

- 日志必须带 `trace_id` 和 `connection_id`，如果上下文存在。
- 指标不把 `trace_id` 作为标签。
- 审计日志可记录管理操作 trace ID，但不记录连接级高频 trace。
- 诊断包可以包含相关 trace 摘要，用于串联日志、metrics snapshot 和 plugin runtime state。

### 审计事件

所有插件管理写操作、自动治理动作和高风险状态变化都应写入审计日志。审计日志不保存 secret 明文、完整构建日志或大段 stack trace，只保存可追溯摘要和目标 ID。

推荐 action：

| Action | Target Type | 触发时机 |
| --- | --- | --- |
| `plugin_artifact_upload` | `plugin_artifact` | 上传 `.mcgp` 或 raw `.so` |
| `plugin_artifact_delete` | `plugin_artifact` | 删除未使用 artifact |
| `plugin_artifact_gc` | `plugin_artifact` | 清理超过保留策略的 artifact/source/log |
| `plugin_build_queue` | `plugin_build` | source `.mcgp` 创建构建任务 |
| `plugin_build_start` | `plugin_build` | builder 开始构建 |
| `plugin_build_finish` | `plugin_build` | 构建成功 |
| `plugin_build_fail` | `plugin_build` | 构建失败 |
| `plugin_load` | `plugin` | 执行 `plugin.Open` 并加载符号 |
| `plugin_enable` | `plugin` | 插件开始处理新流量 |
| `plugin_disable` | `plugin` | 插件停止接收新流量 |
| `plugin_delete` | `plugin` | 删除插件槽位 |
| `plugin_config_update` | `plugin` | 修改配置、priority、runtime limits |
| `plugin_scope_update` | `plugin` | 修改 scope、rollout 或 dry-run |
| `plugin_config_migrate` | `plugin` | 插件升级触发配置迁移 |
| `plugin_data_migrate` | `plugin` | 插件私有数据 schema 迁移 |
| `plugin_data_gc` | `plugin` | 清理过期或可丢弃 plugin_data |
| `plugin_data_snapshot` | `plugin` | 创建 plugin_data 快照 |
| `plugin_action_run` | `plugin` | 管理员执行插件声明式 action |
| `plugin_artifact_switch` | `plugin` | 切换 active artifact |
| `plugin_rollback` | `plugin` | 回滚到旧 artifact |
| `plugin_health_changed` | `plugin` | 健康状态变化 |
| `plugin_background_task_fail` | `plugin` | 后台任务失败超过阈值 |
| `plugin_secret_update` | `plugin_secret` | 创建、更新或轮换 secret |
| `plugin_secret_rotate` | `plugin_secret` | 创建新 secret version 并触发 reload 策略 |
| `plugin_secret_reload` | `plugin` | 插件完成或失败 secret hot reload |
| `plugin_secret_version_revoke` | `plugin_secret` | 撤销指定 secret version |
| `plugin_secret_delete` | `plugin_secret` | 删除 secret |
| `plugin_dependency_block` | `plugin` | 依赖缺失导致启用失败 |
| `plugin_dependency_degraded` | `plugin` | 依赖插件降级或禁用导致当前插件降级 |
| `plugin_external_dependency_changed` | `plugin` | 外部依赖 ready/degraded/not_ready 状态变化 |
| `plugin_circuit_open` | `plugin` | 熔断打开 |
| `plugin_auto_disable` | `plugin` | 治理策略自动禁用插件 |
| `plugin_force_close_connections` | `plugin` | 管理员强制关闭 draining 连接 |
| `plugin_resource_limit_hit` | `plugin` | 资源配额命中阈值 |
| `plugin_alert_silence` | `plugin` | 管理员静默插件告警 |
| `plugin_slow_call` | `plugin` | 插件调用超过性能预算 |
| `plugin_node_sync_fail` | `plugin` | 多实例节点同步失败，未来能力 |
| `plugin_policy_evaluate` | `plugin_artifact` | 执行准入策略评估 |
| `plugin_review_approve` | `plugin_artifact` | 管理员批准高风险 artifact 或配置 |
| `plugin_review_reject` | `plugin_artifact` | 管理员拒绝 artifact 或配置 |
| `plugin_advisory_import` | `plugin_security_advisory` | 导入或创建安全公告 |
| `plugin_advisory_match` | `plugin_artifact` | artifact 命中安全公告 |
| `plugin_advisory_ack` | `plugin_artifact` | 管理员确认或忽略安全公告命中 |
| `plugin_policy_revoke` | `plugin_artifact` | artifact/plugin ID 被加入 denylist 并撤销 |
| `plugin_policy_quarantine` | `plugin` | 已启用插件因策略命中被隔离或禁用 |
| `plugin_promotion_export` | `plugin_artifact` | 导出 promotion bundle |
| `plugin_promotion_import` | `plugin_artifact` | 导入 promotion bundle |
| `plugin_promotion_apply` | `plugin` | 将导入结果应用到 desired state |
| `plugin_repository_import_apply` | `plugin` | 将仓库导入结果应用到 desired state |
| `plugin_drift_detected` | `plugin` | 发现 artifact/config/scope/runtime limits 漂移 |
| `plugin_dr_drill` | `plugin` | 执行灾备演练 |
| `plugin_repository_add` | `plugin_repository` | 添加仓库 |
| `plugin_repository_check` | `plugin_repository` | 检查仓库更新 |
| `plugin_repository_import` | `plugin_artifact` | 从仓库下载 artifact |

审计 message 应包含 sha256、版本、仓库来源、promotion bundle ID、环境标识、失败原因摘要和是否需要重启；不应包含 secret 值、完整配置中的敏感字段、外部 token、构建环境变量或玩家隐私数据。

当前 Admin 审计模型只有 `message` 字段。插件系统落地时应给 `audit_logs` 增加可选 `metadata_json` 字段，用于保存机器可读字段；旧记录默认 `{}`，现有 Admin 页面仍可只展示 `message` 摘要。

```json
{
  "plugin_id": "mc-auth-proxy",
  "artifact_id": "sha256:...",
  "version": "0.2.0",
  "repository_id": "official",
  "build_id": "build_...",
  "restart_required": true
}
```

建议 migration：

```sql
ALTER TABLE audit_logs
ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}';
```

metadata 规则：

- `message` 面向人读，短、稳定、脱敏。
- `metadata_json` 面向机器处理，保存 plugin ID、artifact ID、operation ID、generation、policy hash、route decision ID、secret name、secret version ID、restart required 等低敏字段。
- `metadata_json` 不能保存 secret 明文、token、完整 config、完整外部 response、完整 packet payload、玩家名、UUID 或 source IP 原文。
- 审计查询 API 可以按 metadata 中的 `plugin_id`、`artifact_id`、`operation_id` 做过滤；第一版没有索引时也可以先只做最近窗口过滤。
- 外部 audit sink 使用 `metadata_json` 作为结构化 payload；投递失败不影响本地审计写入。

### 日志和诊断

插件需要统一日志入口，不能让每个插件随意把大量日志写到未知路径。第一版可以先提供 gateway logger 的子 logger，并在 Admin 页面展示最近错误摘要。

推荐接口：

```go
type Logger interface {
    Debug(msg string, fields map[string]any)
    Info(msg string, fields map[string]any)
    Warn(msg string, fields map[string]any)
    Error(msg string, fields map[string]any)
}

type Gateway interface {
    Logger() Logger
}
```

日志规则：

- 日志自动带 `plugin_id`、`artifact_id`、`plugin_version` 和 `extension_point`。
- 插件不能记录 secret、token、session key、完整 forwarding secret、构建环境变量或用户敏感信息。
- gateway 应提供基础脱敏 helper，例如对 secret ref、token 字段和 known secret name 做 mask。
- 插件日志应有级别、速率限制和单条大小限制，避免故障插件刷爆磁盘。
- panic stack 可以进入内部日志，但 Admin 页面只展示摘要；完整 stack 需要 admin 权限。
- connection takeover 插件不能默认记录完整 Minecraft packet payload；需要显式 debug 开关，并且默认关闭。

诊断包可以作为未来 Admin 能力，用于排查插件问题。诊断包应包含：

- 插件 manifest 和 metadata。
- runtime state、health、metrics 摘要。
- 最近错误、panic 摘要和构建日志摘要。
- artifact sha256、builder 元数据、Go/API 版本。
- dispatch table 中该插件的 extension point 注册摘要。

诊断包不应包含：

- secret 明文。
- 完整 `config_json` 中标记为 sensitive 的字段。
- 玩家 token、session server response 原文或完整协议包。
- builder 私有凭据、环境变量或 GOPRIVATE token。

### 优雅禁用

禁用插件时需要区分新流量和已有流量：

1. 先从 dispatch table 移除插件，让新连接不再进入该插件。
2. 对普通 hook/middleware/provider，等待当前调用完成或超时。
3. 对 connection takeover mode，允许已有连接继续到自然关闭，或按管理员操作强制关闭。
4. 调用 `Destroy()` 释放插件全局资源。
5. 更新 runtime state。

Admin 页面需要展示 active calls、active proxy connections 和 draining 状态。

### 冲突处理

多个插件注册同一个 extension point 时必须有确定规则：

- 先按 `plugins.priority` 升序。
- priority 相同按 plugin ID 字典序。
- 同一插件内多个 handler 按 handler priority，再按 handler ID。

不同类型冲突规则：

| 类型 | 冲突规则 |
| --- | --- |
| first-match hook | 第一个 handled 的 handler 生效 |
| all hook | 所有匹配 handler 都执行，任一错误是否中断由 hook 定义 |
| chain middleware | 按顺序执行，handler 可以 next/reject/handled |
| provider | 默认只允许一个 active provider；允许多个时必须定义 fallback |
| event subscriber | 全部异步执行，互不影响 |

如果插件注册了 manifest 未声明的 extension point，启用失败。

dispatch table 生成前必须执行冲突分析：

1. 收集所有 enabled 或待启用插件的 registration。
2. 按 extension point 分组。
3. 应用 scope/rollout 静态重叠检查。
4. 应用 composition 里的 exclusive/conflicts/before/after/provides/consumes。
5. 生成排序后的 dispatch plan。
6. 如果存在 blocking conflict，拒绝启用并保持旧 dispatch table。
7. 如果只有 warning conflict，要求管理员确认并记录审计。

典型 blocking conflict：

- 两个 connection takeover 插件在同一 host 或全局 scope 下都声明 `legacy upstream-connect contract` 接管。
- 两个 provider 都声明同一个 singleton provider name，且没有 fallback/selection 策略。
- middleware 排序形成环，例如 A before B、B before A。
- 插件声明 exclusive extension point，但已有重叠 scope 的插件启用。
- packet filter 插件在同一 phase/packet 范围内都可能改写 payload，且没有 chain 语义。

典型 warning conflict：

- 两个 route.resolve/v1 provider 插件 scope 可能重叠，但 priority 明确，低优先级插件可能永远不会执行。
- event subscriber 多个插件订阅同一事件，可能产生额外开销。
- all hook 中多个插件都会处理同一请求，错误中断策略需要管理员确认。
- route resolver provider 有多个候选，但配置了 fallback。

Admin 需要展示 dispatch plan：

- extension point key。
- 匹配 scope 摘要。
- 最终顺序。
- 每个 handler 的 priority、plugin ID、handler ID。
- 被跳过或被遮蔽的 handler。
- blocking/warning conflict 和建议修复方式。

遮蔽诊断：

- 对 first-match hook，如果高优先级插件在全局 scope 下总是 handled，低优先级同 extension point 插件可能永远不会被调用。
- gateway 可以在静态分析中提示 `shadowed_handler`。
- runtime metrics 应能显示 scope matched、called、pass、handled 的比例，帮助确认是否真的被遮蔽。

修复方式：

- 调整 priority。
- 收窄 scope。
- 改成 chain/all 语义的 extension point，前提是该 extension point 支持。
- 把共享能力改成 provider，由一个插件提供，其他插件 consume。
- 禁用冲突插件。

## Extension Point 设计

插件系统不只提供 Hook。Hook 适合固定时机插入逻辑，但不是所有扩展都应抽象成 callback。第一版把可扩展能力统一称为 Extension Point，并按调用语义分成几类：

| 类型 | 适合场景 | 第一版策略 |
| --- | --- | --- |
| hook | 固定时机调用插件，例如路由命中后、默认拨号前 | 第一版落地 |
| middleware | 连接或请求处理链路上的前后置过滤、短路和改写 | 预留模型 |
| provider | 替换或复用某类能力实现，例如 upstream dialer、route resolver、插件间认证来源 | 预留模型 |
| event subscriber | 异步订阅事件，例如连接关闭、路由变更、插件状态变化 | 预留模型 |
| rule/policy engine | 配置化规则，例如 host rewrite、黑白名单、限流策略 | 建议作为内置插件能力 |

Extension Point 必须稳定命名和版本化：

```text
<domain>.<action>/v<version>
```

候选 Extension Point 清单：

| Key | 类型 | 阶段 | 说明 |
| --- | --- | --- | --- |
| `legacy upstream-connect contract` | hook | 第一版 | 路由命中后、默认拨号前，插件可以返回真实上游连接或自管 stream endpoint |
| `ingress.service/v1` | provider/service | 未来 | 插件声明自定义入口服务，由 gateway/supervisor 管理 listener 和 lifecycle |
| `connection.accept/v1` | hook/event | 预留 | 新连接进入后通知或检查 |
| `connection.filter/v1` | middleware | 预留 | 连接级过滤、限流、拒绝、附加上下文 |
| `connection.closed/v1` | event | 预留 | 连接关闭后异步通知 |
| `handshake.filter/v1` | middleware | 预留 | Minecraft handshake 解析后改写或拒绝 |
| `status.ping/v1` | hook | 预留 | 处理 server list ping，允许自定义 MOTD、favicon、players 和 version |
| `route.resolve/v1` | hook | 预留 | 路由查询前后改写目标或 fallback |
| `route.resolver/v1` | provider | 预留 | 替换或增强路由来源 |
| `route.changed/v1` | event | 预留 | 路由变更后异步通知 |
| `upstream.dialer/v1` | provider | 预留 | 替换默认上游拨号器 |
| `auth.provider/v1` | provider | 预留 | 仅作为插件间复用的认证来源抽象；不参与 gateway core 登录流程 |
| `minecraft.packet.observe/v1` | event | 预留 | 观测 packet metadata，不修改数据流 |
| `minecraft.packet.filter/v1` | middleware | 预留 | 对特定 packet 做拒绝、改写或短路，风险高 |
| `admin.auth.provider/v1` | provider | 预留 | Admin 登录接入外部身份源 |
| `metrics.collect/v1` | hook/event | 预留 | 插件导出指标 |
| `audit.sink/v1` | provider/event | 预留 | 审计日志写入外部系统 |
| `plugin.state.changed/v1` | event | 预留 | 插件状态变化后异步通知 |

第一版内置 hook：

```text
legacy upstream-connect contract
```

它发生在：

```text
client handshake
  -> parse Minecraft host
  -> route snapshot lookup
  -> legacy upstream-connect contract extension point
  -> default upstream dial
  -> proxyConnections
```

### 入口传输和上游协议模型

当前 gateway 已经有多种入口和上游形态：TCP/Admin 共享入口、KCP、QUIC、WebSocket，以及 TCP/KCP/QUIC/HAProxy upstream。插件系统需要把这些信息纳入 request、scope、metrics 和 Admin 诊断，而不是只暴露一个 host 字符串。

传输维度：

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `Transport` | `tcp`、`kcp`、`quic`、`websocket` | 客户端进入 gateway 的传输方式 |
| `ServiceName` | `tcp_admin`、`kcp`、`quic`、`websocket` | 对应 Admin 服务名或逻辑入口名 |
| `ListenerPort` | 端口号 | 当前入口监听端口 |
| `IngressBranch` | `minecraft`、`admin_http`、`websocket_http` | TCP/Web 端口复用时的分流结果 |
| `QUICApplicationProtocol` | ALPN 值 | QUIC 入口协商的 application protocol，未知时为空 |
| `WebSocketPath` | path | WebSocket 入口 path，默认不进指标标签 |

上游维度：

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `UpstreamRaw` | 原始 route upstream 字符串 | 例如 `host:25565`、`kcp://host:25565`、`quic://host:25565`、`haproxy://host:25565` |
| `UpstreamProtocol` | `tcp`、`kcp`、`quic`、`haproxy`、`custom` | route 解析后的默认上游协议 |
| `UpstreamAddress` | host:port | 去掉协议前缀后的默认目标 |
| `ForwardedClientIP` | bool | 默认上游是否会转发客户端 IP，例如 HAProxy protocol |

设计规则：

- `Transport` 描述客户端到 gateway 的入口，`UpstreamProtocol` 描述 gateway/plugin 到 backend 的连接方式，二者不能混用。
- `legacy upstream-connect contract` 插件可以忽略默认 `UpstreamProtocol` 并返回自己的 `net.Conn`；Admin 仍需要展示它覆盖了默认上游协议。
- route.resolve/v1 provider 插件如果只是实现另一种拨号方式，应在事件、指标和诊断里记录 effective upstream protocol，例如 `custom-tunnel`、`socks5`、`tailscale`。
- connection takeover mode 插件可以自行连接 TCP/KCP/QUIC/HAProxy backend，也可以完全不使用 route 中的 upstream；但必须在配置和 health/preflight 中说明 backend 类型。
- HAProxy upstream 表示 gateway 向 backend 写 PROXY protocol header。插件如果自己实现真实 IP 转发，必须声明 forwarding mode，避免重复写入或后端误信任。
- WebSocket 入口的 HTTP upgrade 和 TCP/Admin 端口复用分流由 gateway core 处理；插件默认只接收已经归类为 Minecraft stream 的连接。
- 插件不应接管 Admin HTTP 分支。Admin 自身扩展走 `admin.*` extension point 或声明式 UI，而不是在 TCP/Web 分流前截获 HTTP。

端口复用边界：

- TCP/Admin 共享端口先由 core 判断 HTTP/Admin/WebSocket 分支还是 Minecraft TCP 分支。
- `connection.accept/v1` 如果未来在分流前触发，只能做轻量观测和来源拒绝，不能读取任意首包导致分流不稳定。
- 第一版 `legacy upstream-connect contract` 发生在 Minecraft handshake 解析和 route lookup 之后，不会看到 Admin HTTP 请求。
- 端口复用的 initial packet replay 必须和 `legacy upstream-connect contract.InitialData` 共用同一套只读/回放语义，避免首包被重复消费。

scope 扩展：

- scope 应支持 `transport`、`service_name`、`listener_port`、`upstream_protocol` 和 `route_id`。
- connection takeover 插件如果只支持 TCP 入口或不支持 WebSocket/KCP/QUIC，需要在 manifest capabilities 或 Minecraft capability 中声明；启用时对 scope 做兼容提示。
- rollout sticky key 默认仍以 source IP/connection metadata 为主；玩家名 sticky 只能由 connection takeover 插件在自己解析登录后实现。

### 插件自定义入口服务，未来能力

第一版的插件只能在 gateway core 已接受并归类的连接上工作，不能直接新增监听端口。原因是入口服务涉及端口冲突、Admin 服务启停、systemd/Docker/host network 暴露、TLS 证书、UDP socket、权限和健康检查，必须纳入统一服务模型。

未来如果需要插件提供新的入口，例如 TLS termination、PROXY protocol inbound、自定义 UDP 隧道、Bedrock/Geyser 协议适配或专用 sidecar 入口，应设计独立的 `ingress.service/v1`，而不是复用 `legacy upstream-connect contract`。

`ingress.service/v1` 设计要求：

- 插件只声明服务规格，由 gateway/supervisor 创建 listener；插件不直接 `net.Listen` 任意端口。
- 服务规格包含 protocol、port、bind address、TLS/secret ref、health check、resource limits 和 expose policy。
- 与 Admin 服务表统一做端口冲突检查，不能占用 TCP/Admin 共享入口、KCP、QUIC、WebSocket 或已启用插件服务端口。
- lifecycle 由 Plugin Manager 管理，disable/delete 时先停止接收新连接，再 drain 或强制关闭。
- sandbox-process runtime 下可以把自定义入口放到独立进程或 sidecar，gateway 只负责控制面和健康状态。
- ingress 插件产生的连接仍应转化为统一 `Transport`/`ServiceName`/`ConnectionID`/trace 语义后再进入后续 extension point。
- 自定义入口服务属于高风险能力，启用、端口变化、TLS/secret 变化和 scope 扩大都需要 review。

第一版只保留 schema 和设计边界，不实现 `ingress.service/v1`。如果插件确实需要监听内部端口，应作为外部 sidecar 独立部署，再通过 gateway 的 TCP/KCP/QUIC/WebSocket 入口接入。

### Hook

Hook 是固定时机的 callback。建议第一版把现有散列参数收敛为 request struct，方便后续追加字段：

```go
type UpstreamConnectRequest struct {
    Source          net.Conn
    ConnectionID    string
    TraceID         string
    Transport       string
    ServiceName     string
    ListenerPort    int
    IngressBranch   string
    QUICApplicationProtocol string
    WebSocketPath   string
    SourceAddr      string
    ServerHost      string
    RawServerHost   string
    ProtocolVersion int
    NextState       int
    InitialData     []byte
    RouteHit        bool
    RouteID         string
    RouteTags       []string
    UpstreamRaw     string
    UpstreamProtocol string
    UpstreamAddress string
    ForwardedClientIP bool
    Scope           map[string]string
    Metadata        map[string]string
}

type UpstreamConnectHandler func(context.Context, *UpstreamConnectRequest) (net.Conn, error)
type UpstreamConnectAcceptor func(*UpstreamConnectRequest) bool
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `Source` | 客户端连接，只用于读取连接属性；handler 不应直接读写它 |
| `ConnectionID` | gateway 生成的连接 ID，用于日志、指标和诊断 |
| `TraceID` | 跨插件和后端转发的 trace 标识 |
| `Transport` | 客户端入口传输：`tcp`、`kcp`、`quic`、`websocket` |
| `ServiceName` | Admin 服务名或逻辑入口名，例如 `tcp_admin`、`kcp`、`quic`、`websocket` |
| `ListenerPort` | 入口监听端口 |
| `IngressBranch` | 端口复用分流结果；第一版传给插件时应为 `minecraft` |
| `QUICApplicationProtocol` | QUIC ALPN，非 QUIC 为空 |
| `WebSocketPath` | WebSocket 入口 path，非 WebSocket 为空 |
| `SourceAddr` | 脱敏前的来源地址字符串；指标中不能直接作为标签 |
| `ServerHost` | 规范化后的 Minecraft server address host |
| `RawServerHost` | 原始 handshake server address，可能包含 forwarding 相关内容 |
| `ProtocolVersion` | Minecraft protocol version |
| `NextState` | handshake next state，例如 status/login |
| `InitialData` | gateway 已读取的初始 handshake bytes；只读，插件不得修改 |
| `RouteHit` | 是否命中 gateway route |
| `RouteID` | 命中的 route ID |
| `RouteTags` | route tag，用于 scope、rollout 和策略 |
| `UpstreamRaw` | route 中的原始 upstream 字符串 |
| `UpstreamProtocol` | 解析后的默认上游协议：`tcp`、`kcp`、`quic`、`haproxy` |
| `UpstreamAddress` | 去掉协议前缀后的默认上游地址 |
| `ForwardedClientIP` | 默认上游路径是否会转发客户端 IP，例如 HAProxy protocol |
| `Scope` | gateway 计算出的 scope/rollout 摘要 |
| `Metadata` | 预留扩展字段，只允许低基数字符串 |

request 规则：

- 新字段只能追加，已有字段语义不能改变。
- `InitialData` 必须是副本或只读视图，插件修改它不能影响 gateway 内部 buffer。
- `Source` 不作为插件读写数据通道；connection takeover 必须通过返回的 `net.Conn` 接管。
- `RawServerHost` 可能包含敏感或非标准数据，日志默认不记录。
- `SourceAddr`、玩家名、UUID 不应进入指标标签。
- `Transport`、`ServiceName`、`UpstreamProtocol` 是低基数字段，可以用于指标标签；`ListenerPort` 是否作为标签由部署规模决定。
- `WebSocketPath` 和 `QUICApplicationProtocol` 进入日志和诊断摘要时需要限长和低基数化。
- `Metadata` 不能承载 secret、token、完整协议 payload 或高基数字段。

第一版 handler 返回 `(net.Conn, error)`。后续如果需要表达更丰富结果，可以新增 `legacy upstream-connect contract`：

```go
type UpstreamConnectResult struct {
    Conn          net.Conn
    Mode          string // dialer/connection takeover
    Decision      string // handled/pass/reject
    RejectReason  string
    Backend       string
    Metadata      map[string]string
}
```

`legacy upstream-connect contract` 不在返回值中携带身份信息；后续 `legacy upstream-connect contract` 即使增加结构化 result，也不应让 gateway core 消费 Minecraft 登录身份并拼装登录流程。身份 forwarding、登录结果和登录后的协议处理都由 connection takeover 插件自己写入、代理或通过观测事件上报。对 gateway core 来说，plugin-owned `net.Conn` 是 opaque stream endpoint，而不是可解析的 Minecraft 登录子流程。

该 opaque 语义是协议代理能力成立的关键约束。gateway core 只能基于调用前已经拥有的连接元数据、handshake 元数据、route 元数据、scope 和插件运行时状态做治理；不能从 connection takeover 插件内部拿到玩家身份后再参与 Minecraft 业务决策。插件如果需要把玩家身份传给 backend，应直接按目标 backend 支持的 forwarding 协议写入后端连接。

`legacy upstream-connect contract` 调用模式：

- `legacy upstream-connect contract` 是 first-match hook。
- 按 `plugins.priority` 升序调用。
- priority 相同按插件 ID 升序调用。
- 插件内多个 handler 时按注册顺序或 handler priority 调用。
- acceptor 返回 false 时跳过。
- handler 返回 `api.ErrPass` 时继续尝试下一个 handler。
- handler 返回 `net.Conn, nil` 时使用该连接，不再执行默认拨号。
- handler 返回其他 error 时本次连接失败并记录日志。
- 没有插件处理时走默认 upstream dial。

`legacy upstream-connect contract` 支持两种使用模式：

| 模式 | 返回值 | 适合场景 |
| --- | --- | --- |
| route.resolve/v1 provider | 插件返回真实上游连接 | 自定义拨号、隧道、代理、服务发现、灰度和 fallback |
| connection takeover mode | 插件返回自管 `net.Conn`，并在插件内部继续代理协议 | 完整协议代理、MC 正版/三方登录、登录策略、后续 play 阶段协议处理 |

route.resolve/v1 provider 示例：

```text
client
  -> gateway
  -> legacy upstream-connect contract
  -> plugin net.Dial/custom tunnel
  -> backend
```

connection takeover mode 示例：

```text
client
  -> gateway
  -> legacy upstream-connect contract
  -> plugin-owned net.Conn endpoint
  -> plugin Minecraft protocol proxy
  -> backend
```

在 connection takeover mode 中，插件可以返回一个由插件控制的连接端点，例如 `net.Pipe()` 的一端。gateway 会把已经读取到的初始 handshake 包写入该连接，并继续把客户端后续字节转发到该连接。插件在另一端读取完整 Minecraft 字节流，因此可以自行实现：

- handshake 和 login start 解析。
- online-mode encryption request/response。
- Mojang/Yggdrasil `hasJoined` 校验。
- 三方 Yggdrasil-like session server 校验。
- UUID、profile、textures 和 properties 处理。
- 登录失败 kick message。
- 登录成功后的 backend 选择。
- Velocity modern forwarding、BungeeCord forwarding 或自定义身份转发。
- 后续 play 阶段协议代理或观测。

这种模式下，MC 正版/三方登录不要求 gateway core 内置登录协议。登录协议、认证源、身份映射和后端转发策略可以全部由插件实现。gateway 只需要提供稳定的 stream endpoint extension point。

### Minecraft 登录插件职责划分

正版/三方登录插件在本设计中应被视为“插件实现的完整业务能力”，而不是 gateway core 暴露若干认证回调后由 core 拼装登录流程。

结论：按当前设计，正版/三方登录插件可以实现。它不依赖 gateway core 实现任何 Minecraft 登录业务逻辑，也不依赖第一版实现 `auth.provider/v1`。必要条件是 `legacy upstream-connect contract` 支持 connection takeover mode，并且 gateway 能把初始 handshake bytes 回放到插件返回的 `net.Conn`。

需要纠正的边界是：正版/三方登录以及登录后的逻辑不是 gateway core 的职责，也不是 core 调用插件拿到认证结果后继续处理。插件接管 stream 后，应由插件自己完成认证、身份映射、后端连接、forwarding、configuration/play 阶段代理、失败响应和连接关闭。gateway core 只负责把连接稳定交给插件，并围绕这个交接点做生命周期和治理。

插件负责：

- Minecraft 协议状态机，包括 handshake、login、configuration 和 play 阶段中它选择接管的部分。
- 正版或三方认证协议，例如 Mojang/Yggdrasil、Yggdrasil-like server、自定义账号系统或混合认证。
- 玩家身份、UUID、profile、properties、textures 和权限映射。
- 登录失败响应、kick reason 和审计事件内容。
- 认证成功后的 backend 选择、连接建立和身份 forwarding。
- 登录后的协议代理、观测、限流、策略控制和连接关闭处理。

gateway core 负责：

- 在路由命中后、默认拨号前调用 `legacy upstream-connect contract`。
- 将已读取的初始 Minecraft handshake 数据交还给插件自管连接。
- 把客户端连接和插件返回的 `net.Conn` 做稳定转发。
- 提供插件生命周期、配置、secret、指标、审计、超时、并发和 draining 支撑。
- 捕获 handler panic、timeout 和错误，保护 dispatch table 和其他插件。

因此，MC 正版/三方登录插件只依赖 `legacy upstream-connect contract` 的 connection takeover mode 就可以成立。`auth.provider/v1` 只能作为未来插件之间复用认证来源的抽象，例如多个 connection takeover 插件共用同一个 Yggdrasil-like session verifier；它不是 gateway core 登录流水线，也不是第一版实现登录插件的必要条件。

SDK/API 约束：

- 第一版不提供 `AuthenticatePlayer()`、`OnLoginSuccess()`、`ForwardIdentity()` 这类由 gateway core 编排的 Minecraft 登录回调。
- 第一版不定义 core 可消费的 `PlayerIdentity`、`AuthResult` 或 `SessionResult` 返回值。
- 第一版不在 gateway connection context 中写入玩家名、UUID、profile、权限或认证状态。
- 第一版不提供“core 路由后，插件认证，再由 core forwarding”的半接管模式。
- 插件如果需要内部拆分登录逻辑，应在插件包内自行组织模块，或未来通过插件间 `auth.provider/v1` provider 复用；gateway core 仍不参与登录状态机。
- 管理页展示的登录成功率、认证失败原因和 forwarding 失败只能来自插件主动上报的脱敏事件、指标、health 和诊断摘要。

connection takeover mode 的约束：

- 插件必须在返回自管 `net.Conn` 前启动对应的读写处理，否则 gateway 写入初始包时可能阻塞。
- 插件负责关闭自管连接、后端连接和内部 goroutine。
- 插件负责协议状态机、超时、认证失败响应和 backend forwarding 的安全性。
- 后端如果信任插件转发身份，应只允许 gateway 或插件代理访问，并保护 forwarding secret。
- gateway 不应在持有插件全局锁时调用 handler，也不应在 dispatch table 更新时阻塞已有连接。
- 插件 panic 或返回错误只应影响当前连接，不能破坏全局 dispatch table。

### `net.Conn` 接管契约

`legacy upstream-connect contract` 返回的 `net.Conn` 必须满足 Go `net.Conn` 基本语义。gateway 会把它视为“上游连接”并执行双向转发。

gateway 负责：

- 在调用 hook 前读取 Minecraft handshake 所需的初始数据。
- 当插件返回 `net.Conn` 后，先把已经读取的初始数据写入该连接。
- 初始数据写入成功后，启动 client <-> returned conn 的双向转发。
- 转发结束后关闭双方连接，并记录字节数、耗时和错误摘要。
- 当初始数据写入失败时，关闭 returned conn 和 client conn，并记录本次连接失败。

插件负责：

- 返回的 `net.Conn` 在返回前已经有 goroutine 或后端连接负责读取，避免 gateway 写入初始数据阻塞。
- 正确实现 `Read`、`Write`、`Close`、`SetDeadline`、`SetReadDeadline` 和 `SetWriteDeadline`。
- 如果使用 `net.Pipe()`，必须在另一端及时读取并处理关闭。
- 自己管理后端连接、认证流程、协议状态机和 goroutine 生命周期。
- 不假设 gateway 会解析 login 阶段或 play 阶段协议。

关闭和 deadline 规则：

- gateway 可以为初始写入设置短 write deadline，避免插件返回不可读 conn 导致连接路径挂死。
- connection takeover 插件接管后，协议级超时由插件负责；gateway 只管理连接级 idle timeout 和转发 timeout。
- 如果底层连接支持 half-close，gateway 可以优先使用 half-close；不支持时退化为 Close。
- 插件 `Close()` 必须幂等。
- 插件不能在 `Close()` 中长期阻塞。

错误规则：

- handler 返回 `api.ErrPass` 表示不处理，gateway 尝试下一个 handler 或默认 dial。
- handler 返回 `api.ErrBlocked` 表示明确拒绝连接，gateway 关闭 client conn，可记录拒绝原因。
- handler 返回其他 error 表示插件处理失败，默认本次连接失败；是否 fallback 到下一个 handler 由 extension point 策略决定。
- returned conn 初始写入失败视为该插件 handled 失败，不再继续调用后续 first-match handler，避免重复消费已读 handshake 状态。

### Minecraft 协议能力声明

connection takeover 插件能接管完整 Minecraft 字节流，但管理员仍需要知道插件声称支持哪些协议版本、协议阶段、认证模式、forwarding 模式和 modded 边界。否则插件启用后才发现某些客户端版本、backend 或 modded 客户端不兼容，排障成本很高。

manifest 可以增加 `minecraft`：

```json
{
  "minecraft": {
    "protocol_versions": {
      "min": 760,
      "max": 767,
      "tested": [760, 763, 765, 767],
      "unsupported_policy": "kick"
    },
    "states": {
      "status": "transparent",
      "login": "handled",
      "configuration": "transparent",
      "play": "transparent"
    },
    "auth_modes": ["mojang", "yggdrasil-like", "offline"],
    "forwarding": {
      "supported": ["velocity-modern", "bungeecord", "none"],
      "default": "velocity-modern",
      "requires_secret": true
    },
    "modded": {
      "forge": "transparent",
      "fabric": "transparent",
      "fml": "unsupported",
      "unknown": "pass"
    },
    "packet_features": {
      "compression": "transparent",
      "encryption": "handled",
      "brand_observe": false,
      "packet_rewrite": false
    }
  }
}
```

字段语义：

| 字段 | 说明 |
| --- | --- |
| `protocol_versions.min/max` | 插件声明支持的 Minecraft protocol version 范围 |
| `protocol_versions.tested` | 插件作者或 conformance 覆盖过的版本 |
| `unsupported_policy` | 插件对不支持版本的自声明处理策略：`kick`、`pass`、`close`；不表示 gateway core 代替插件处理 Minecraft 登录或协议 |
| `states.status/login/configuration/play` | 对各协议阶段的处理：`handled`、`transparent`、`observe`、`unsupported` |
| `auth_modes` | 支持的认证模式 |
| `forwarding.supported` | 支持的身份转发方式 |
| `modded` | 对 Forge/Fabric/FML 等 modded handshake 的处理声明 |
| `packet_features` | 压缩、加密、brand、packet rewrite 等能力 |

管理规则：

- manifest 声明不代表 gateway core 会解析这些阶段，也不代表 core 会代替插件返回 kick、pass 或 close；它用于管理页展示、发布门禁、测试矩阵和示例文档。
- gateway 已知的 `ProtocolVersion` 可以在 dispatch 前用于 scope/rollout 和兼容提示。
- 如果请求的 protocol version 不在插件声明范围内，第一版 gateway 只做兼容提示或按 scope/rollout 跳过插件；一旦进入 connection takeover handler，unsupported version 的 kick、pass、close 都由插件自己实现。
- 如果插件声明 `unsupported_policy=pass`，它不应在不支持版本上接管连接；这需要插件的 acceptor 或 handler 自行返回 `api.ErrPass`。
- connection takeover 插件如果启用 forwarding，必须声明 supported forwarding mode 和 secret requirement。
- backend 如果要求 Velocity/Bungee forwarding，管理页应提示 backend 直连保护和 secret 配置。
- modded 支持必须保守声明；unknown modded 默认不应被当作 supported。

测试要求：

- `tested` 中的每个 protocol version 至少有 handshake/login smoke test。
- auth mode、forwarding mode 和 unsupported protocol version 需要有 fixture。
- configuration/play transparent 模式至少验证不会破坏基础转发。
- modded transparent/pass/unsupported 需要有最小 handshake fixture 或明确标为未测试。

发布门禁：

- connection takeover 插件启用前展示 Minecraft 能力矩阵。
- scope 中包含的 protocol version 超出插件声明范围时进入 warning 或 blocking。
- forwarding mode 变更、从 transparent 改为 handled、开启 packet rewrite 都属于高风险变更。
- promotion import 应对比源/目标环境 backend forwarding 配置和插件 forwarding 支持。

### Minecraft Auth Proxy 插件设计模板

`mc-auth-proxy` 是 connection takeover mode 的代表性插件。它的目标不是让 gateway core 增加登录逻辑，而是在插件内部完整实现 Minecraft 登录代理。

处理流程：

```text
client
  -> gateway reads initial handshake
  -> legacy upstream-connect contract
  -> mc-auth-proxy returns plugin-owned net.Conn
  -> plugin reads handshake/login start
  -> plugin performs auth flow
  -> plugin dials selected backend
  -> plugin forwards authenticated identity
  -> plugin proxies configuration/play bytes
```

插件职责：

- 解析 Minecraft handshake，识别 server address、protocol version 和 next state。
- 处理 login start。
- 根据配置选择 auth mode：Mojang/Yggdrasil、Yggdrasil-like、offline、自定义外部账号系统或混合模式。
- 对 online-mode 登录执行 encryption request/response 和 shared secret 处理。
- 调用 session server，例如 Mojang `hasJoined` 或三方 session endpoint；推荐通过 `ExternalClient` 使用 manifest 声明的 `external_dependencies`。
- 生成或映射玩家 UUID、name、profile properties、textures 和权限上下文。
- 执行白名单、黑名单、会员、ban、外部 entitlement 或风控检查。
- 认证失败时返回 Minecraft login disconnect/kick response。
- 认证成功后选择 backend，并连接 backend。
- 使用 Velocity modern forwarding、BungeeCord forwarding 或自定义 forwarding 传递身份。
- 继续代理 configuration/play 阶段，或只做透明转发和观测。

gateway core 职责：

- 提供 `legacy upstream-connect contract` stream endpoint。
- 回放初始 handshake bytes。
- 提供配置、secret、日志、指标、scope、rollout、health、draining 和生命周期。
- 不解析 login/encryption/session/forwarding 业务协议，不决定正版/三方登录成败，也不拼装登录后协议链路。
- 不读取或维护玩家身份上下文；身份只存在于插件内部、插件转发给 backend 的协议数据以及插件主动上报的脱敏观测数据中。

配置示例：

```json
{
  "match_hosts": ["play.example.com"],
  "auth_modes": [
    {
      "name": "official",
      "type": "mojang",
      "session_server": "https://sessionserver.mojang.com",
      "scope_hosts": ["premium.example.com"],
      "fail_policy": "fail_closed"
    },
    {
      "name": "third-party",
      "type": "yggdrasil-like",
      "session_server": "https://auth.example.com/sessionserver",
      "public_key_pem_ref": "third_party_yggdrasil_public_key",
      "scope_hosts": ["third.example.com"],
      "fail_policy": "fail_closed"
    }
  ],
  "backend": {
    "default": "127.0.0.1:25566",
    "forwarding": {
      "mode": "velocity-modern",
      "secret_ref": "velocity_forwarding_secret"
    }
  },
  "cache": {
    "profile_ttl_seconds": 300,
    "negative_ttl_seconds": 30
  },
  "policy": {
    "whitelist_enabled": true,
    "allow_offline_fallback": false
  }
}
```

secret 建议：

| Secret | 用途 |
| --- | --- |
| `velocity_forwarding_secret` | Velocity modern forwarding shared secret |
| `third_party_yggdrasil_public_key` | 三方 Yggdrasil-like 服务公钥或证书 |
| `external_auth_token` | 外部会员、权限或风控 API token |

缓存策略：

- profile/session 校验结果可以短 TTL 缓存，降低外部认证源压力。
- negative cache TTL 必须短，避免误封或临时认证失败长期影响玩家。
- cache key 不应包含 secret 或完整 session response。
- 缓存应随插件实例生命周期清理；需要跨重启缓存时使用 PluginDataStore 并设置容量限制。

失败策略：

| 场景 | 默认策略 |
| --- | --- |
| session server 超时 | fail closed，返回登录失败；可按 host 配置 fail open |
| external entitlement API 失败 | fail closed 或 degraded，由配置决定 |
| backend dial 失败 | 返回登录后断开或 fallback backend |
| forwarding secret 缺失 | 启用失败 |
| protocol parse error | 关闭连接并记录低基数错误 |
| unsupported protocol version | 返回明确 kick message |

后端保护：

- 如果 backend 信任 forwarding 身份，backend 应只接受 gateway 或插件代理来源 IP。
- backend 必须启用对应 forwarding 模式，避免玩家绕过 gateway 直连伪造身份。
- forwarding secret 需要定期轮换；轮换时插件和 backend 应有协调窗口。
- 管理页应提示 forwarding secret 泄漏时需要同步轮换后端配置。

观测：

- 记录认证成功、认证失败、session server 超时、backend dial 失败、forwarding 失败的低基数指标。
- 不把玩家名、UUID、IP 原文放入指标标签。
- 日志默认只记录脱敏玩家标识或 hash。
- debug packet logging 默认关闭，并且必须有时间限制。

HealthCheck 建议：

- 检查 session server 可达性。
- 检查 external entitlement API 可达性。
- 检查 backend dial 或轻量探测。
- 检查必需 secret 是否已配置。
- 某个 auth mode 不可用时可返回 degraded，并在状态中标出影响的 host。

外部依赖建议：

| Dependency ID | 用途 | 默认策略 |
| --- | --- | --- |
| `mojang-session` | Mojang/Yggdrasil session 校验 | `fail_closed`，短 timeout，不在连接路径多次重试 |
| `third-party-yggdrasil` | 三方 Yggdrasil-like session/profile 校验 | `fail_closed`，endpoint 由环境覆盖 |
| `entitlement-api` | 会员、白名单、权限或风控校验 | 按配置 `fail_closed` 或 `degraded` |

这些 external dependency 只约束插件如何访问外部服务和如何暴露健康状态。session response 的解析、认证成功与否、kick message、身份映射、forwarding 和后续协议代理仍完全由 `mc-auth-proxy` 插件实现。

测试建议：

- offline/official/third-party 三种 auth mode。
- 正常登录、session server 拒绝、session server 超时、协议版本不支持。
- forwarding secret 缺失、错误和轮换。
- backend 连接失败和 fallback。
- 登录失败 kick message 编码。
- goroutine 和连接关闭。

后续 hook 可以按同一模型扩展：

| Hook | 调用模式 | 说明 |
| --- | --- | --- |
| `connection.accept/v1` | all | 新连接进入后通知插件，可用于限流、审计 |
| `route.resolve/v1` | first-match 或 transform | 路由查询前后改写目标 |
| `legacy upstream-connect contract` | first-match | 接管上游连接创建 |
| `connection.close/v1` | all async | 连接关闭后通知插件 |
| `metrics.collect/v1` | all | 插件导出指标 |

### Middleware

Middleware 适合连接处理链路上的过滤和改写，语义比 Hook 更接近一条有顺序的处理链。

候选扩展点：

| Extension Point | 模式 | 说明 |
| --- | --- | --- |
| `connection.filter/v1` | chain | 新连接进入后检查，可放行、拒绝或附加上下文 |
| `handshake.filter/v1` | chain | Minecraft handshake 解析后改写或拒绝 |

Middleware 设计原则：

- 必须有确定顺序，按 priority 和插件 ID 排序。
- 每个 middleware 必须显式返回 `next`、`reject` 或 `handled`。
- 连接路径不能持全局锁执行 middleware。
- 每个 middleware 必须有超时和 panic 边界。

### Minecraft Status 和 Packet 扩展

`status.ping/v1` 适合低成本定制 server list ping：

- 动态 MOTD。
- favicon。
- online/max players 展示。
- protocol version 和 version text。
- 维护模式提示。
- 按 host/source/route tag 返回不同状态。

`status.ping/v1` 不应要求插件接管完整连接。它可以由 gateway core 解析 status request 后调用插件返回响应结构。第一版如果不实现该扩展点，仍可由 connection takeover 插件完整处理 status state。

`minecraft.packet.observe/v1` 只做观测：

- 只暴露 packet metadata，例如 phase、direction、packet ID、size、connection ID。
- 默认不暴露完整 payload。
- 异步执行，不能阻塞连接路径。
- 适合审计、统计、调试和告警。

`minecraft.packet.filter/v1` 可以修改或拒绝 packet，风险高：

- 必须明确 phase：handshake、status、login、configuration、play。
- 必须限制 packet 类型和大小。
- 必须有严格超时、panic recover 和 fail open/closed 策略。
- 默认不进入第一版主路径。
- 对 play 阶段 packet 改写容易破坏协议兼容，建议优先由 connection takeover 插件自行实现。

modded handshake 处理：

- Forge/Fabric/FML 等 modded handshake 可能改变 server address、login payload 或 configuration 阶段行为。
- 第一版不要求 gateway core 理解 modded protocol。
- 需要 modded 支持的插件应使用 connection takeover mode，或等待后续专门 extension point。

### 路由解析层

当前 gateway 的基础路由真相来自 SQLite `routes` 表，并通过内存 snapshot 提供低开销 lookup。插件系统引入动态路由后，必须保持“本地静态路由、插件动态路由、外部路由源”之间的合成规则可解释，否则 Admin 页面会无法说明一次连接为什么去了某个 backend。

第一版边界：

- SQLite route snapshot 仍是默认路由来源。
- `legacy upstream-connect contract` 发生在 route lookup 之后，可以覆盖默认上游连接，但不应反向修改 SQLite routes。
- `route.resolve/v1` 和 `route.resolver/v1` 第一版只预留；如果提前实现，也必须生成可审计的 route decision。
- 插件如果需要同步外部 CMDB/服务发现结果，第一版更适合写成后台任务加本地插件缓存，再由 `legacy upstream-connect contract` 使用，而不是直接改 gateway route 表。

动态路由合成模型：

```text
Minecraft handshake host
  -> normalize host
  -> SQLite route snapshot lookup
  -> optional route.resolve/v1 transform/override
  -> legacy upstream-connect contract
  -> effective upstream
```

route decision 应包含：

| 字段 | 说明 |
| --- | --- |
| `requested_host` | 客户端 handshake host 的规范化结果 |
| `sqlite_route_hit` | 是否命中 SQLite route snapshot |
| `sqlite_upstream` | SQLite 路由给出的 upstream，脱敏展示 |
| `route_provider` | 做出覆盖或增强决策的插件/handler |
| `decision` | `use_sqlite`、`override`、`fallback`、`reject`、`pass` |
| `effective_upstream` | 最终交给 upstream.connect/default dial 的 upstream |
| `reason_code` | 低基数原因，例如 `cmdb_match`、`health_fallback`、`maintenance` |
| `cache_status` | `hit`、`miss`、`stale`、`bypass` |

路由 provider 规则：

- `route.resolve/v1` 适合对单次请求做 first-match transform，例如 fallback、灰度或按 host 改写 upstream。
- `route.resolver/v1` 适合替换或增强 route source，例如外部 CMDB、服务发现、配置中心。
- provider 默认单例或 priority first-match；多个 provider 同时覆盖同一 host/scope 时必须进入冲突分析。
- provider 不能在连接路径直接执行无界外部调用；必须有 timeout、cache、fallback 和 fail policy。
- provider 的外部依赖应使用 `ExternalClient` 并声明 `purpose=route`。
- provider 返回的 upstream 必须通过与 Admin route 相同的 `ValidateUpstream` 语义，支持 `tcp`、`kcp://`、`quic://`、`haproxy://` 或声明的 custom protocol。
- provider 不能写入 SQLite routes，除非它是明确的“同步插件”并通过 Admin API/权限/审计路径更新；同步写入和运行时决策必须分开。

缓存和一致性：

- route provider 可以有内存缓存或 PluginDataStore 缓存；缓存需要 TTL、stale 策略和手动刷新 action。
- 外部 route source 不可用时，默认策略应是使用最后可用缓存或 SQLite fallback，而不是阻塞连接路径。
- route decision 日志和指标只记录低基数字段；完整外部 response 不进入日志、事件或诊断包。
- Admin route 页面需要区分 SQLite route、插件动态 route decision 和插件缓存状态。
- route.changed/v1 事件只说明 SQLite route 或 provider cache 状态变化，不能作为强一致事务边界。

审计边界：

- 管理员修改 SQLite routes 走现有 Admin route API 和 audit log。
- 插件运行时每次 route decision 不进入长期审计；只进入 metrics、trace 和最近事件摘要。
- 插件如果通过受控 action 刷新 route cache、导入外部路由或写入 SQLite route，必须写审计日志。
- promotion bundle 默认只导出 SQLite desired routes 和插件 desired state，不导出 provider 的运行时缓存。

### Provider

Provider 用于替换某类能力实现。Mock 的运行时价值应收敛为 Provider，而不是让插件直接 monkey patch 某个函数。

候选扩展点：

| Extension Point | 说明 |
| --- | --- |
| `upstream.dialer/v1` | 替换默认 TCP/KCP/QUIC/HAProxy 上游拨号实现 |
| `route.resolver/v1` | 替换或增强路由解析来源 |
| `auth.provider/v1` | 提供可复用的游戏侧认证来源，例如 Yggdrasil-like session 校验；只供其他插件调用 |
| `admin.auth.provider/v1` | 提供管理页登录身份源，例如 OIDC、LDAP 或企业 SSO；只影响 Admin 登录 |

`auth.provider/v1` 容易被误解成 gateway core 的游戏登录扩展点，因此需要明确：

- 它不是第一版 MC 正版/三方登录插件的必要依赖。
- 它不定义 gateway core 的 Minecraft 登录流水线。
- 它只用于插件之间复用认证来源，例如一个 connection takeover 插件调用另一个插件提供的 session verifier。
- 即使未来实现，调用方仍应是负责协议代理的插件，而不是 gateway core。

Provider 设计原则：

- 同类 provider 默认只允许一个 active provider，避免多个实现互相覆盖。
- 如果允许多个 provider，必须定义选择规则，例如 priority first-match。
- Provider 需要清晰声明能力边界和 fallback 行为。
- `auth.provider/v1` 不应让 gateway core 参与 Minecraft 登录状态机；它只是插件间共享认证来源的可选接口。
- Provider 适合正式扩展，不应称为 mock；mock 只保留在测试语境。

### Admin Auth Provider

`admin.auth.provider/v1` 是管理页登录扩展点，不是 Minecraft 游戏侧登录扩展点。它只负责让 Admin 页面接入 OIDC、LDAP、企业 SSO 或内部身份平台；不会被 `legacy upstream-connect contract`、MC 登录插件或游戏连接路径调用。

基本原则：

- 初始 setup 必须先创建本地 SQLite admin；外部身份源不能替代 setup 流程。
- 必须保留本地 admin break-glass 账号；不能因为启用外部登录而锁死所有管理员。
- gateway 签发 Admin session token；插件不得直接签发、保存或验证 Admin bearer token。
- 插件只返回外部身份、groups/claims 摘要和认证证据；gateway 负责账号链接、权限映射、session TTL、CSRF、审计和权限检查。
- 外部认证不可 fail open。SSO/OIDC/LDAP 不可用时，外部登录失败或降级展示；本地 break-glass 登录仍可用。
- 禁用、隔离或撤销 Admin auth provider 时，应阻止新的外部登录，并可按策略撤销该 provider 已签发的 gateway session。

候选接口：

```go
type AdminAuthProvider interface {
    ProviderID() string
    LoginMethods(ctx context.Context) ([]AdminLoginMethod, error)
    BeginLogin(ctx context.Context, req AdminBeginLoginRequest) (AdminBeginLoginResult, error)
    CompleteLogin(ctx context.Context, req AdminCompleteLoginRequest) (AdminExternalIdentity, error)
}

type AdminLoginMethod struct {
    ID          string
    Type        string // redirect/form
    DisplayName string
}

type AdminExternalIdentity struct {
    ProviderID       string
    Subject          string
    UsernameHint     string
    DisplayName      string
    Email            string
    Groups           []string
    Claims           map[string]string
    EvidenceSummary  map[string]string
}
```

OIDC 类 provider 通常使用 `redirect` 方法：

```text
Admin login page
  -> gateway creates CSRF/state/nonce
  -> provider BeginLogin returns redirect URL
  -> browser redirects to IdP
  -> IdP callback reaches gateway
  -> provider CompleteLogin validates code/token/JWKS
  -> gateway maps identity and issues gateway session
```

LDAP 类 provider 可以使用 `form` 方法：

```text
Admin login page
  -> gateway receives username/password over existing Admin HTTPS
  -> gateway calls provider CompleteLogin
  -> provider binds/searches LDAP
  -> gateway maps identity and issues gateway session
```

安全边界：

- OIDC `state`、`nonce`、CSRF cookie 和回调 URL 校验由 gateway 拥有；插件不得绕过。
- LDAP password、OIDC code、refresh token、ID token 和 access token 不进入日志、事件、审计 metadata、诊断包或插件业务事件。
- provider 返回的 `Subject` 必须是稳定唯一主体；gateway 按部署级 salt 计算 `subject_hash` 后存储和审计。
- `Claims` 只允许低基数字符串摘要；大 token、完整 JWT、完整 LDAP entry 不返回给 Admin API。
- 插件不能修改 `adminuser` 表、session store 或 permission map；只通过 provider 接口返回认证结果。
- 插件不能声明自定义 Admin 权限绕过 gateway 权限模型；权限只能映射到 gateway 已知 permission key 或默认角色。

账号链接和权限映射：

- 推荐默认策略是预链接：管理员先把外部 provider subject 绑定到已有本地用户。
- 可以预留 JIT provisioning，但必须有 allow rule，例如 issuer、domain、group 或 claim 匹配，并设置默认角色/permission set。
- 如果外部 subject 已绑定另一个用户，必须阻断登录并写审计。
- 如果外部 username/email 与已有本地用户碰撞，但 subject 未预链接，默认阻断并要求管理员手动链接。
- groups/claims 到 `admin/member/guest` 或稳定 permission key 的映射由 gateway 配置保存和审计；插件只提供标准化输入。
- 权限映射结果应在 session 签发时固化，并携带 mapping version；映射变更后可以要求重新登录或撤销旧 session。

运行时和生命周期：

- provider health 应展示 `ready/degraded/not_ready`、最近错误种类和受影响 login method。
- provider not_ready 不应影响本地登录，也不应阻塞非 Admin 的 MC 连接路径。
- provider 配置或 secret 变化应支持 reload；OIDC issuer/client ID、LDAP URL、bind DN、CA bundle 和 group mapping 变化需要重新 preflight。
- 删除或禁用 provider 前，Admin 页面必须提示受影响的外部身份链接和现有 session 处理策略。
- 多个 Admin auth provider 可以并存，但同一个 callback path、provider ID 或 login method ID 不能冲突。

`admin.auth.provider/v1` 第一版可以先作为预留设计；即使提前实现，也应独立于 MC 登录插件，不改变 `legacy upstream-connect contract` 的责任划分。

### Event Subscriber

Event Subscriber 用于异步通知，不应阻塞主连接路径。

候选事件：

| Event | 说明 |
| --- | --- |
| `connection.closed/v1` | 连接关闭后通知 |
| `route.changed/v1` | 路由变更后通知 |
| `plugin.state.changed/v1` | 插件状态变化后通知 |
| `audit.recorded/v1` | 审计事件写入后通知 |

manifest 可以声明订阅：

```json
{
  "event_subscriptions": [
    {
      "id": "audit-forwarder",
      "events": ["audit.recorded/v1", "plugin.state.changed/v1"],
      "delivery": "at_least_once",
      "queue_size": 1000,
      "timeout_ms": 1000,
      "retry": {
        "max_attempts": 3,
        "backoff_ms": 500
      },
      "overflow": "drop_oldest"
    }
  ]
}
```

SDK 可以提供：

```go
type EventSubscriptionRegistration struct {
    ID      string
    Events  []string
    Handler func(context.Context, PluginEventEnvelope) error
}

type PluginEventEnvelope struct {
    ID           string
    Name         string
    Version      string
    CreatedAt    time.Time
    TraceID      string
    ConnectionID string
    SubjectHash  string
    Attributes   map[string]string
}

type Gateway interface {
    SubscribeEvents(reg EventSubscriptionRegistration) error
}
```

Event Subscriber 设计原则：

- 默认异步执行。
- 单个插件处理失败不影响其他 subscriber。
- 需要队列长度、丢弃策略和错误计数。
- 适合观测、审计、同步外部系统，不适合修改当前连接结果。
- 事件处理是弱一致的，不能用于必须同步返回的认证、路由、权限或连接拒绝决策。
- 订阅者收到的是脱敏后的 event envelope，不能要求完整 packet payload、secret、token 或玩家隐私原文。
- `delivery=best_effort` 可以在队列满、超时或插件 disabled 时丢弃事件并记录 drop。
- `delivery=at_least_once` 需要 retry 和去重 ID；handler 必须幂等。
- 第一版不建议承诺 `exactly_once`；如果 manifest 声明，应上传或启用时拒绝。
- 审计事件投递到外部 sink 失败不能删除 gateway 本地审计日志；外部 sink 是副本，不是真相来源。
- `audit.recorded/v1` 默认至少保存本地投递失败摘要；是否重试到成功由管理员策略决定。
- 队列溢出策略支持 `drop_newest`、`drop_oldest`、`dead_letter` 和 `block_producer`；连接路径事件默认不允许 `block_producer`。
- dead letter 只保存脱敏摘要和错误类型，管理员可以查看、丢弃或手动重放。

投递状态：

| 状态 | 说明 |
| --- | --- |
| `queued` | 已入队，等待 subscriber 处理 |
| `delivered` | subscriber 成功处理 |
| `failed` | 本次处理失败，可重试 |
| `dropped` | 因队列满、超时、策略或插件禁用被丢弃 |
| `dead_letter` | 超过重试次数，进入死信摘要 |

推荐指标：

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `plugin_event_delivery_total` | counter | `subscriber_plugin_id`、`subscription_id`、`event_name`、`result` | 事件投递结果 |
| `plugin_event_delivery_duration_seconds` | histogram | `subscriber_plugin_id`、`subscription_id`、`event_name` | 单次投递耗时 |
| `plugin_event_queue_depth` | gauge | `subscriber_plugin_id`、`subscription_id` | 当前订阅队列长度 |
| `plugin_event_dead_letter_total` | counter | `subscriber_plugin_id`、`subscription_id`、`reason` | 进入死信的事件数量 |

### 插件间协作

插件间协作必须通过显式、可审计、可版本化的机制完成，不能依赖 Go 全局变量、包级单例、隐式 init 顺序或直接访问其他插件实例。

允许的协作方式：

| 方式 | 说明 |
| --- | --- |
| dependencies | 声明启用顺序和必需插件 |
| provider | 一个插件提供能力，另一个插件通过 gateway 暴露的 provider interface 使用 |
| event subscriber | 通过事件异步通知，适合观测和状态同步 |
| PluginDataStore | 只用于插件自己的命名空间；不建议跨插件共享 |
| SecretStore | secret 只授权给声明它的插件；跨插件共享必须显式配置 |

协作规则：

- 插件不能直接获取其他插件实例指针。
- 插件不能读写其他插件的 `plugin_data` 命名空间。
- provider API 必须版本化，例如 `auth.provider/v1`。
- provider 的输入输出不能包含未文档化的内部结构。
- 依赖图不能有环；可选依赖不能影响核心启用路径。
- 禁用 provider 插件时，依赖它的插件应进入 degraded、disabled 或 restart_required，具体由依赖声明和策略决定。
- event subscriber 是异步弱一致，不应用来实现必须同步返回的认证或路由决策。

如果需要插件 A 调用插件 B 的能力，应优先把 B 的能力抽象成 provider，由 gateway core 管理生命周期、排序、超时、熔断和审计。

### Rule / Policy Engine

常见小需求不一定需要用户写 Go 插件。可以内置一个规则插件或策略引擎，通过配置完成：

- host rewrite
- IP 黑白名单
- upstream rewrite
- 简单限流
- 按 host 或来源地址设置策略

Rule / Policy Engine 可以作为官方插件实现，底层仍注册 hook、middleware 或 provider。这样能减少用户为简单需求编写源码包的成本。

### Mock 和 Mixin 的定位

Mock 不作为用户插件机制。它适合测试，也可以启发 provider 设计：把需要替换的能力抽成接口，再让插件注册 provider。

Mixin、AOP、monkey patch 不作为正式插件方案：

- Go 没有自然的运行时 mixin 模型。
- 修改已有对象或函数行为不利于审计、禁用和排错。
- 编译期 mixin、代码生成或 source weaving 会让插件边界变模糊。

不同技术形态应这样落位：

| 技术形态 | 适合场景 | 在本设计中的定位 |
| --- | --- | --- |
| test mock | 单元测试、conformance、故障注入 | 只在测试工具和示例中使用，不进入生产插件 API |
| provider | 替换某类能力实现，例如 route resolver、Admin auth provider、audit sink | 正式扩展点，gateway 管生命周期、超时、熔断和冲突 |
| middleware | 对连接/请求链做顺序处理、过滤、改写或短路 | 正式扩展点，但必须有确定顺序和失败策略 |
| hook | 固定时机调用插件，例如上游连接前 | 第一版主路径 |
| rule/policy engine | 常见配置化策略，不需要写 Go 代码 | 官方插件或内置能力 |
| mixin/source weaving | 编译期织入横切逻辑 | 只允许官方 build-time instrumentation，不能作为普通用户插件 |
| monkey patch | 运行时替换内部函数或全局变量 | 不支持 |

设计原则：

- 如果一个能力需要替换默认实现，应抽象成 provider，而不是 mock。
- 如果一个能力需要在链路中“前后置处理”，应抽象成 middleware，而不是 mixin。
- 如果一个能力只是配置化判断，应优先放入 rule/policy engine，而不是让用户写源码插件。
- 如果一个能力需要修改 gateway 内部未公开位置，先评估是否应该新增 extension point；不要让插件直接改内部实现。
- 如果确实需要源码级扩展，只能走受控 build-time instrumentation，并作为 gateway binary 供应链的一部分 review、测试和发布。
- mock 名称只保留在测试语境，生产 manifest 不应出现 `mock` 类型 extension point。

如果未来需要源码级扩展，只应通过明确的 provider、middleware、rule engine 或 build-time instrumentation 注册点表达，而不是允许插件修改 gateway 内部实现。

Extension Point API 的扩展原则：

- Extension Point key 一旦发布不能改变语义。
- 不在同一版本里改变 handler 函数签名。
- 新字段只追加到 request struct。
- 不同语义或不兼容签名必须发布新 version。
- Dispatch table 必须是只读快照，更新时整体替换，避免连接路径持锁执行插件代码。

## 配置和数据

### 配置版本

插件配置需要版本化：

- manifest 声明 `config_version`。
- `plugins.config_version` 记录当前保存的配置版本。
- `config_schema` 用于管理页展示和基础校验。
- `ReloadConfig()` 做业务级校验。

示例：

```json
{
  "config_version": 2,
  "config_schema": {
    "type": "object",
    "properties": {
      "match_host": { "type": "string" },
      "upstreams": {
        "type": "array",
        "items": { "type": "string" }
      }
    }
  }
}
```

### 配置迁移

插件升级时可能需要迁移配置。SDK 可以提供可选接口：

```go
type ConfigMigrator interface {
    MigrateConfig(fromVersion int, raw json.RawMessage) (toVersion int, migrated json.RawMessage, err error)
}
```

迁移规则：

- artifact 切换前先检查目标插件是否支持从当前版本迁移。
- 迁移成功后写入新 `config_json` 和 `config_version`。
- 迁移失败时不切换 active artifact。
- 管理页展示迁移预览和错误。
- 破坏性迁移需要管理员确认。

### 插件私有数据

SDK 可以提供小型 KV 能力：

```go
type PluginDataStore interface {
    Get(ctx context.Context, key string, dst any) error
    Put(ctx context.Context, key string, value any) error
    Delete(ctx context.Context, key string) error
}
```

使用约束：

- key 必须限定在插件 ID 命名空间内。
- value 必须是 JSON。
- 单条 value 和总容量需要限制。
- 不适合保存大规模日志、指标或玩家长期数据。
- 插件删除时，管理页应询问是否同时删除 `plugin_data`。

### 插件文件资源和工作目录

不是所有插件状态都适合放进 SQLite JSON KV。协议代理、登录、仓库、审计和路由同步类插件可能需要读取包内模板、CA bundle、规则文件，或写入缓存、临时文件、诊断输出。SDK 应提供受控文件路径，而不是让插件自行推断 gateway 的目录结构。

```go
type PluginFileStore interface {
    ResourcePath(ctx context.Context, name string) (string, error)
    DataDir(ctx context.Context) (string, error)
    CacheDir(ctx context.Context) (string, error)
    TempDir(ctx context.Context) (string, error)
    LogDir(ctx context.Context) (string, error)
    DiagnosticsDir(ctx context.Context) (string, error)
    Stat(ctx context.Context) (PluginFileUsage, error)
}

type PluginFileUsage struct {
    DataBytes        int64
    CacheBytes       int64
    TempBytes        int64
    LogBytes         int64
    DiagnosticsBytes int64
    QuotaBytes       int64
}
```

manifest 可以声明包内资源和运行时文件需求：

```json
{
  "resources": [
    {
      "name": "default-rules",
      "path": "resources/rules/default.json",
      "type": "json",
      "required": true,
      "sha256": "..."
    },
    {
      "name": "ca-bundle",
      "path": "resources/certs/ca.pem",
      "type": "certificate",
      "required": false
    }
  ],
  "file_storage": {
    "data": {
      "max_bytes": 16777216,
      "retention": "keep",
      "exportable": false
    },
    "cache": {
      "max_bytes": 67108864,
      "retention": "7d",
      "discardable": true
    },
    "tmp": {
      "max_bytes": 104857600,
      "retention": "restart"
    }
  }
}
```

使用规则：

- `ResourcePath` 只返回 manifest 声明的包内资源路径，且路径在 artifact 只读目录内。
- `DataDir` 用于需要跨重启保留但不适合 JSON KV 的小型文件；大规模玩家数据仍应放外部存储。
- `CacheDir` 只能保存可重建数据；GC 可以按 retention 和配额清理。
- `TempDir` 只保存运行中临时文件；gateway 启动、插件 disable 或 action 完成后可以清理。
- `DiagnosticsDir` 只保存诊断中间产物；导出前必须脱敏并受权限控制。
- 插件不得把 secret、token、session response、完整 packet payload 或未脱敏玩家隐私写入可导出的文件。
- 文件配额超过时，SDK 返回明确错误；插件应降级或清理缓存，不应阻塞连接路径。
- 覆盖写入同一 runtime 文件时，配额按 `current_usage - old_file_size + new_file_size` 计算，不能把安全重写误判为超限。
- source package 构建输出不能把 builder cache 当作运行时资源；只有进入 `.mcgp` artifact 或 runtime 目录的文件才受该模型管理。

备份和迁移：

- `data/` 默认参与备份恢复，但默认不进入 promotion bundle。
- `cache/`、`tmp/`、`logs/` 和 `diagnostics/` 默认不参与 promotion bundle。
- 只有 manifest 明确声明 `exportable=true` 且 data class 允许迁移的文件，才能由管理员导出。
- 回滚 artifact 前需要检查目标版本是否兼容当前 `file_storage` schema；不兼容时应提示清理、迁移或放弃回滚。

### Secret Store

SDK 可以提供 secret 读取能力：

```go
type SecretRef struct {
    Name      string
    Version   string
    NotBefore time.Time
    NotAfter  time.Time
}

type SecretStore interface {
    Get(ctx context.Context, name string) (string, error)
    GetVersion(ctx context.Context, ref SecretRef) (string, error)
    CurrentRef(ctx context.Context, name string) (SecretRef, error)
}

type SecretReloader interface {
    ReloadSecret(ctx context.Context, name string, oldRef SecretRef, newRef SecretRef) error
}
```

使用约束：

- 插件只能读取 manifest 声明并已授权的 secret。
- secret 不应出现在插件状态、日志、审计 message 和错误返回中。
- SecretStore 只提供读取；写入和轮换由 Admin 管理。
- secret 读取失败应让插件配置校验或启用失败，而不是在连接路径里反复失败。
- 插件如果缓存 secret，必须缓存 `SecretRef` 而不是只缓存明文值，便于轮换和诊断。
- `ReloadSecret` 只用于刷新插件内部缓存和新连接行为，不应强制修改已有连接的协议状态。
- `GetVersion` 主要用于 dual-read grace period；grace period 结束后旧版本应返回 expired。
- `SecretRef.Version` 不应泄露 secret 内容，可以是随机版本 ID、递增 generation 或 KMS version。

### 配置 UI Schema

`manifest.config_schema` 第一版可以兼容 JSON Schema 子集，并增加 `x-mc-gateway-*` UI hint。这样管理页可以先用 JSON 编辑器兜底，后续逐步渲染表单。

示例：

```json
{
  "config_version": 1,
  "config_schema": {
    "type": "object",
    "required": ["match_host", "auth_mode"],
    "properties": {
      "match_host": {
        "type": "string",
        "title": "Match host",
        "description": "Minecraft server host matched by this plugin",
        "x-mc-gateway-ui": { "widget": "host" }
      },
      "auth_mode": {
        "type": "string",
        "enum": ["mojang", "yggdrasil-like", "offline"],
        "x-mc-gateway-ui": { "widget": "select" }
      },
      "forwarding_secret_ref": {
        "type": "string",
        "x-mc-gateway-secret-ref": "velocity_forwarding_secret",
        "x-mc-gateway-sensitive": true
      },
      "debug_packet_log": {
        "type": "boolean",
        "default": false,
        "x-mc-gateway-advanced": true,
        "x-mc-gateway-restart-required": false
      }
    }
  }
}
```

推荐 UI hint：

| Hint | 说明 |
| --- | --- |
| `x-mc-gateway-sensitive` | 管理页脱敏展示，审计日志不记录明文 |
| `x-mc-gateway-secret-ref` | 字段值必须引用插件声明的 secret |
| `x-mc-gateway-advanced` | 默认折叠到高级配置 |
| `x-mc-gateway-restart-required` | 修改后需要重启或重新启用 |
| `x-mc-gateway-reload-required` | 修改后需要 reload config |
| `x-mc-gateway-ui.widget` | 表单控件建议，例如 host、cidr、duration、endpoint、textarea |
| `x-mc-gateway-validation` | gateway 侧附加校验类型，例如 upstream address、URL、CIDR |

配置编辑规则：

- 保存前先做 JSON Schema 校验，再调用插件 `ReloadConfig()` dry run。
- 管理页展示变更 diff，但 sensitive 字段只显示 changed，不显示明文。
- config schema 变化需要记录配置快照。
- 如果某字段标记 restart required，保存后插件状态应显示 restart required 或 pending reload。
- schema 不能替代插件业务校验，最终仍以 `ReloadConfig()` 为准。

### 插件声明式 Admin UI

第一版不运行插件包内的前端 JavaScript，也不允许插件直接向 Admin 页面注入 HTML。Admin 扩展应优先采用声明式 UI：插件通过 manifest、config schema、runtime status 和受控 action 描述希望展示的内容，管理页用内置组件渲染。

设计目标：

- 管理页可展示插件特有配置、状态、健康、诊断和动作。
- 插件不能绕过 Admin 权限、CSRF、防 XSS、审计和脱敏规则。
- UI 能随插件 manifest 一起版本化，且不引入前端构建链。
- 未来如果支持自定义前端，也有清晰隔离边界。

manifest 可以增加 `admin_ui`：

```json
{
  "admin_ui": {
    "config_form": {
      "schema": "config_schema",
      "layout": [
        { "field": "match_hosts", "width": "full" },
        { "field": "auth_mode", "width": "half" },
        { "field": "forwarding_secret_ref", "width": "half" }
      ]
    },
    "status_panels": [
      {
        "id": "auth-summary",
        "title": "Auth summary",
        "metrics": [
          "auth_success_total",
          "auth_failure_total",
          "session_server_latency_ms"
        ]
      }
    ],
    "actions": [
      {
        "id": "flush-profile-cache",
        "title": "Flush profile cache",
        "kind": "plugin_action",
        "danger": false,
        "confirm": true,
        "permission": "admin"
      }
    ]
  }
}
```

声明式 UI 支持范围：

| 能力 | 第一版策略 |
| --- | --- |
| config form layout | 支持字段顺序、分组、折叠、高级项、secret ref |
| status panel | 展示低基数 metrics、health、recent errors、background tasks |
| action button | 触发受控插件 action，必须鉴权和审计 |
| diagnostics link | 下载或生成诊断包，必须脱敏 |
| docs link | 展示 README、homepage、Runbook 链接 |
| custom HTML/JS | 第一版不支持 |

插件 action 需要显式注册：

```go
type PluginActionRegistration struct {
    ID          string
    Title       string
    Description string
    Dangerous   bool
    Timeout     time.Duration
    Handler     func(context.Context, ActionRequest) (ActionResult, error)
}

type Gateway interface {
    RegisterAction(action PluginActionRegistration) error
}
```

action 规则：

- action ID 必须在 manifest `admin_ui.actions` 中声明。
- action 默认只允许 admin 执行；未来可细分权限。
- dangerous action 必须二次确认。
- action 必须有 timeout、panic recover 和并发限制。
- action 不能直接返回 secret、token、大日志或高基数玩家列表。
- action 结果只允许结构化 JSON 摘要，字段需要按 sensitive hint 脱敏。
- 每次 action 执行必须写审计日志，包含 actor、plugin ID、action ID、success、duration 和摘要。

典型 action：

- 刷新外部路由缓存。
- 清理 profile/session cache。
- 重新探测 backend。
- 手动触发同步任务。
- 生成诊断包。

不适合作为 action：

- 上传或启用另一个插件。
- 修改 Admin 用户权限。
- 返回 secret 明文。
- 执行任意 shell 命令。
- 长时间运行且无法取消的任务。

状态面板数据来源：

- Plugin Manager runtime state。
- 插件 metrics。
- HealthCheck。
- BackgroundTask 状态。
- 最近日志摘要。
- 插件 action 返回的短摘要。

状态面板不能直接查询插件私有数据库或读取插件任意文件。需要展示的数据应通过受控 API、metrics 或 PluginDataStore 摘要暴露。

未来自定义前端：

- 只能作为 future capability，不进入第一版。
- 必须使用 iframe sandbox 或独立 origin。
- 必须有严格 CSP，禁止读取 Admin token。
- 与 gateway 通信只能走受控 postMessage 或 scoped API token。
- 静态资源必须进入 `.mcgp` 包校验、sha256、大小限制和供应链审计。
- 自定义 UI 启用应作为高风险能力进入准入策略和 review。

### 插件预检和自测

`ReloadConfig()` dry run 只能证明配置结构基本可接受，不能证明插件在目标环境下能安全启用。复杂插件还需要检查 secret、外部依赖、backend、文件资源、数据 schema、scope、协议版本和插件自身业务假设。SDK 应提供标准预检和自测接口，供 Admin 发布门禁、CLI、promotion import 和灾备演练复用。

```go
type PreflightRequest struct {
    Config        json.RawMessage
    Scope         map[string]string
    Rollout       map[string]any
    RuntimeLimits RuntimeLimits
    DryRun        bool
}

type PreflightResult struct {
    Status  string // passed/warning/failed
    Checks  []PreflightCheckResult
    Summary string
}

type PreflightCheckResult struct {
    ID       string
    Severity string // info/warning/error/blocking
    Status   string // passed/warning/failed/skipped
    Message  string
    Evidence map[string]string
}

type PreflightChecker interface {
    Preflight(ctx context.Context, req PreflightRequest) (PreflightResult, error)
}

type SelfTestRequest struct {
    Profile  string // quick/protocol-smoke/integration/soak
    Fixtures []string
}

type SelfTester interface {
    SelfTest(ctx context.Context, req SelfTestRequest) (PreflightResult, error)
}
```

预检范围：

| 检查 | 说明 |
| --- | --- |
| config | JSON Schema、业务校验、迁移 dry-run 和敏感字段引用 |
| secret | 必需 secret 是否存在、版本是否有效、rotation state 是否兼容 |
| external dependency | endpoint、timeout、fail policy、health 和数据传输声明 |
| file resources | 包内 resources 是否存在、sha256 是否匹配、runtime data/cache 配额 |
| plugin data | schema version、迁移 dry-run、回滚兼容性 |
| scope/rollout | scope 是否过宽、rollout 是否可解释、sticky key 是否可用 |
| runtime limits | timeout、并发、active proxy、后台任务限制是否满足插件最低要求 |
| minecraft | protocol version、states、auth modes、forwarding、modded 和 fixture 覆盖 |
| backend | backend dial、forwarding mode、直连保护和 fallback 配置 |

自测规则：

- `quick` 只做本地、短耗时检查，适合上传后和启用前默认执行。
- `protocol-smoke` 可以构造握手、登录失败响应、backend dial 等 fixture，适合 connection takeover 插件。
- `integration` 可以访问声明的 external dependencies，但必须使用 `ExternalClient` 和受控 timeout。
- `soak` 属于上线前或 staging 证据，不应在生产 Admin 请求里同步执行。
- 自测结果只保存摘要和证据 ID，不保存 secret、token、完整 response、完整 packet payload 或玩家隐私原文。
- 插件没有实现 `PreflightChecker` 时，gateway 仍执行通用门禁；高风险插件缺少预检接口应进入 warning 或 review_required。

MC 登录插件预检示例：

- 检查 `velocity_forwarding_secret`、三方 Yggdrasil 公钥和 external auth token 是否存在。
- 检查 `mojang-session` 或 `third-party-yggdrasil` dependency health。
- 用 fixture 验证 unsupported protocol version 的 kick message。
- 验证配置中的 forwarding mode 与 backend 要求一致。
- 验证 profile cache、negative cache 和 runtime file quota 不会超过默认策略。

## Plugin API

插件接口保持小而稳定：

```go
type Plugin interface {
    Init(gateway Gateway) error
    Destroy() error
    NewConfigObj() any
    ReloadConfig(config any) error
}

type Gateway interface {
    HandleConn(conn net.Conn)
    ExitWaitGroup() *sync.WaitGroup
    Hook(hook string, handler any) error
}
```

`Gateway.Hook` 是现有探索代码里的第一版注册入口。落地时建议保留兼容层，但在 SDK 中引入更通用的注册 API：

```go
type ExtensionPointType string

const (
    ExtensionPointHook       ExtensionPointType = "hook"
    ExtensionPointMiddleware ExtensionPointType = "middleware"
    ExtensionPointProvider   ExtensionPointType = "provider"
    ExtensionPointEvent      ExtensionPointType = "event"
)

type ExtensionPoint struct {
    Type    ExtensionPointType
    Key     string
    Version int
    Mode    string
}

type HandlerRegistration struct {
    ID       string
    Priority int
    Handler  any
}

type Gateway interface {
    RegisterExtension(point ExtensionPoint, registration HandlerRegistration) error
    DataStore() PluginDataStore
    Secrets() SecretStore
    Files() PluginFileStore
    ExternalClient() ExternalClient
    Logger() Logger
    Tracer() Tracer
    EmitEvent(ctx context.Context, event PluginEvent) error
    RecordMetric(ctx context.Context, metric PluginMetric) error
    SubscribeEvents(reg EventSubscriptionRegistration) error
    RegisterBackgroundTask(task BackgroundTaskRegistration) error
    PluginInfo() PluginRuntimeInfo
}
```

第一版需要补充：

- `api.APIVersion` 常量。
- `api.ErrPass` 表示当前 handler 放弃处理，继续后续 handler。
- `api.ErrBlocked` 表示插件主动拒绝连接。
- `api.ErrFeatureUnavailable` 表示 optional feature 在当前 gateway 不可用。
- Extension Point 类型增加 key、version、type、mode 元数据。
- Register extension point handler 时携带 handler ID 或 priority，方便调试和排序。
- 可选 `HealthCheck` 接口用于上报 ready/degraded/not_ready。
- 可选 `PreflightChecker` 和 `SelfTester` 用于发布门禁、CLI、promotion import 和灾备演练复用。
- 可选后台任务注册接口用于受控定时任务。
- 可选 `PluginFileStore` 用于读取包内只读资源、获取插件私有 data/cache/tmp/log/diagnostics 工作目录和查看配额使用。
- 可选 `ExternalClient` 用于访问 manifest 声明的外部依赖，并自动应用 timeout、并发、retry、熔断、trace、metrics 和脱敏规则。
- 可选 `SubscribeEvents` 用于注册异步事件订阅，按 manifest 声明的事件、队列、超时、重试和溢出策略投递。
- `PluginRuntimeInfo` 应包含 plugin ID、artifact ID、version、dry-run 状态、scope 摘要、gateway/API 版本和 feature set。

插件配置：

- 插件通过 `NewConfigObj()` 返回结构体指针。
- gateway 将 `config_json` decode 到该结构体。
- 支持 `json` 和 `toml` tag，推荐使用 `json` tag。
- `ReloadConfig()` 负责业务级校验。
- `manifest.config_schema` 用于管理页展示表单和做基础校验，但最终以 `ReloadConfig()` 为准。
- `config_schema` 应支持 `sensitive`、`secret_ref`、`advanced`、`restart_required` 等 UI hint，便于管理页脱敏和提示。

实例开发规则：

- `Plugin()` factory 每次调用都应返回新的实例对象。
- 实例字段保存配置、缓存句柄、后台任务状态和外部 client；不要把可变业务状态放在包级全局变量。
- `Init()` 不应阻塞等待长期任务完成；长期任务通过 `RegisterBackgroundTask` 或插件自管可取消 context 启动。
- `Destroy()` 必须停止实例拥有的 goroutine、连接、文件句柄和 ticker，并允许重复调用。
- handler 闭包不要捕获会被 reload 原地修改的可变配置；推荐在新 generation 创建新 handler。
- 插件如果必须使用 package 级全局缓存，必须在 README 和 manifest risk note 中说明，并保证不同 config/generation 不会互相污染。

## 开发者体验

插件系统需要让开发者能独立完成开发、构建、测试、诊断和发布。

### SDK 和模板

需要提供：

- `plugin/api` 稳定 API 文档。
- `examples/plugins/upstream-rewrite` 最小模板。
- `examples/plugins/mc-auth-proxy` connection takeover 模板。
- manifest source 多格式解析和 canonical JSON schema。
- 统一的 `gateway plugin init/build/test` 开发工具链。

详细工具链设计见 [plugin-development-toolchain-design.md](plugin-development-toolchain-design.md)。工具链必须继续遵守 manifest-only 元数据约束：插件作者只维护一个 manifest source 文件，Go 代码中不再保存 `manifestJSON` 或等价重复元数据；`.mcgp` 包内仍以 canonical `manifest.json` 作为服务端可信边界。

### CLI 工具

建议提供 `gateway plugin` 子命令，降低插件开发和运维成本。开发入口直接扩展在 `gateway plugin init/build/test` 下，不新增 `dev` 子命名空间。

候选命令：

| 命令 | 说明 |
| --- | --- |
| `gateway plugin init` | 生成插件模板 |
| `gateway plugin validate <path>` | 校验 manifest、源码目录或 `.mcgp` 包 |
| `gateway plugin build --type source` | 打包 source `.mcgp` |
| `gateway plugin build --type binary` | 构建并打包 binary `.mcgp` |
| `gateway plugin build --type both` | 同时生成 source/binary `.mcgp` |
| `gateway plugin build --from-source` | 从 source `.mcgp` 生成 binary `.mcgp`，逐步替代 `source-build` 主路径 |
| `gateway plugin test` | 运行插件 unit、manifest、harness 或 conformance profile |
| `gateway plugin inspect plugin.mcgp` | 查看 manifest、supply chain、sha256、Go/API 版本 |
| `gateway plugin compat plugin.mcgp` | 检查当前 gateway 是否可能加载该插件 |
| `gateway plugin features` | 查看当前 gateway 支持的 feature key 和版本 |
| `gateway plugin preflight` | 对插件包或已安装插件执行通用预检和插件 Preflight |
| `gateway plugin self-test` | 运行 quick/protocol-smoke/integration 自测 profile |
| `gateway plugin benchmark` | 运行插件 benchmark、soak 或 regression profile |
| `gateway plugin contract check` | 校验契约文件和上一 release 的兼容性 |
| `gateway plugin conformance` | 运行插件契约 conformance suite |
| `gateway plugin export` | 从 Admin API 导出 promotion bundle |
| `gateway plugin import` | 上传并校验 promotion bundle |
| `gateway plugin diff` | 对比 bundle、目标环境和当前 desired state |
| `gateway plugin drift` | 查看当前环境相对基线的漂移状态 |
| `gateway plugin dr-drill` | 触发或查看灾备演练 |
| `gateway plugin data inspect <plugin>` | 查看 plugin_data schema、data class、大小、配额和 GC candidate |
| `gateway plugin data export <plugin>` | 导出允许迁移的数据，受 data_class 和权限控制 |
| `gateway plugin data gc <plugin>` | 按 retention 清理过期或可丢弃 plugin_data |
| `gateway plugin sbom` | 生成或校验 SBOM，供供应链策略使用 |
| `gateway plugin sign verify/key-rotation/revoke` | 验证 artifact 签名，维护本地 trust store 并吊销不可信 key |

CLI 规则：

- CLI 校验不能替代服务端校验，服务端必须重复做安全校验。
- build 命令必须生成稳定 zip，避免无意义 sha256 变化。
- inspect 命令不能执行插件代码。
- compat 命令只能做 preflight，必须检查 required/optional features，但不能保证 `plugin.Open` 一定成功。
- features 命令输出必须和 Admin `/plugins/features` API 使用同一契约。
- diff、drift、export 和 import 必须使用同一 canonical hash 与脱敏 diff 实现。
- build 命令应默认使用与 gateway release 匹配的 builder image；本地 Go plugin adapter 可以先使用当前 Go toolchain。
- data inspect 默认只显示摘要，不导出 value。
- data export 必须经过 Admin API 权限检查，且只能导出 manifest 声明 `exportable=true` 的数据。
- data gc 必须支持 dry-run，先展示将清理的 data_class、key 数量和总大小。

### 本地开发

本地开发流程：

1. 从示例复制插件目录。
2. 编写唯一 manifest source，默认是 `manifest.yaml`。
3. 使用与 gateway 匹配的 Go toolchain。
4. 运行 `gateway plugin build` 构建 `.mcgp`。
5. 通过 Admin 上传。
6. 查看 ABI 校验结果、构建日志、加载状态和运行错误。

开发环境可以允许 raw `.so` 上传或本地插件目录加载，但生产推荐统一 `.mcgp`。

### 测试 Harness

需要提供插件测试工具，避免开发者只能在真实 gateway 中调试：

- manifest 校验命令。
- 构建兼容性检查。
- mock `api.Gateway`。
- extension point handler 单元测试 helper。
- `legacy upstream-connect contract` 的 route.resolve/v1 provider 测试 helper。
- connection takeover mode 的 net.Pipe 测试 helper。
- 构造 Minecraft handshake/login packet 的测试工具。

测试工具应覆盖：

- `ReloadConfig()` 配置校验。
- handler priority 和 acceptor 行为。
- `api.ErrPass`、拒绝和错误返回。
- 自管 `net.Conn` 读写关闭行为。
- goroutine 泄漏和超时。

除开发者本地 harness 外，gateway 仓库需要提供 conformance suite，用于 release 前验证公开插件契约。conformance suite 应能在 CI 中构建示例插件、运行契约 fixture，并输出兼容性报告。

### 测试矩阵

插件系统本身需要覆盖以下测试矩阵：

| 类别 | 覆盖项 |
| --- | --- |
| 包格式 | `.mcgp` manifest、binary/source、zip slip、大小限制、缺失文件 |
| ABI | Go version、GOOS/GOARCH、GOAMD64/GOARM64、CGO、build tags、ABI fingerprint、API version、Plugin symbol |
| 契约 | manifest schema、extension point contract、错误码、CLI 输出 golden |
| 生命周期 | upload、build、load、enable、disable、delete、restart reconcile |
| native instance | factory 新实例、Destroy 幂等、stale generation、package global 风险提示 |
| 配置 | schema 校验、ReloadConfig dry run、配置迁移、配置快照、sensitive diff |
| Admin UI | 声明式 schema、status panel、action 权限、action 审计、禁止自定义 JS |
| extension point | request 字段、handler ordering、priority、scope、rollout、dry-run、ErrPass、ErrBlocked |
| 入口传输 | TCP、KCP、QUIC、WebSocket、TCP/Admin 端口复用、initial packet replay |
| 上游协议 | TCP、KCP、QUIC、HAProxy upstream、custom dialer、真实 IP 转发边界 |
| 组合冲突 | composition constraints、scope overlap、provider 单例、middleware 排序环、shadowed handler |
| connection takeover | 初始 handshake 回放、net.Pipe、deadline、Close、初始写入失败 |
| 治理 | timeout、panic recover、并发上限、背压、熔断、draining |
| tracing | trace ID、connection ID、span parent、context propagation、采样和脱敏 |
| 性能 | micro benchmark、integration benchmark、protocol smoke、soak、regression threshold |
| Minecraft 兼容 | protocol versions、states、auth modes、forwarding、modded、unsupported policy |
| 后台任务 | run-on-start、不可重入、timeout、disable cancel、shutdown cancel |
| health | ready、degraded、not_ready、enable preflight、手动 health check |
| preflight/self-test | config、secret、external dependency、resources、protocol-smoke、脱敏证据 |
| external dependency | endpoint diff、secret ref、timeout、fail policy、dependency health、data classes |
| event subscription | delivery mode、queue overflow、retry、drop、dead letter、weak consistency |
| secret | secret 缺失、轮换、删除、日志和审计脱敏 |
| plugin data | schema version、data migration、quota、retention、GC、snapshot、rollback |
| plugin files | resources、data/cache/tmp/log/diagnostic 目录、quota、retention、GC、path traversal |
| artifact | 回滚、GC candidate、已加载 artifact pending cleanup、缺失文件恢复 |
| Admin 权限 | guest/member/admin 的读写权限和 401/403 |
| 多实例预留 | node state 上报、partial rollout failed、节点级 restart required |

示例插件也需要测试：

- `upstream-rewrite` 覆盖 route.resolve/v1 provider。
- `mc-auth-proxy` 覆盖 connection takeover mode、登录失败响应和 forwarding secret 引用。
- 示例插件 source/binary `.mcgp` 都能通过 validate、compat、build 和 conformance。

### 发布门禁

插件从上传到启用应有明确门禁。第一版可以把门禁做成管理页检查清单。

启用前必须通过：

- manifest schema 校验。
- plugin ID、version、runtime、artifact_type 合法。
- Go version、GOOS/GOARCH、plugin API 兼容。
- capabilities、runtime limits、scope、rollout 展示并由管理员确认。
- 必需 dependencies 存在且启用。
- 必需 secret 已配置。
- required external dependencies 的 secret、endpoint、timeout、fail policy 和 health check 满足启用策略。
- config schema 校验和 `ReloadConfig()` dry run 成功。
- 通用 preflight 检查通过；插件实现 `PreflightChecker` 时，结果不得包含 blocking/error。
- artifact sha256 和 manifest schema 校验通过。
- artifact 未命中 denylist，准入策略未返回 blocking risk。
- 当前 artifact/config/scope/runtime limits 组合已有有效 review，或当前策略不要求 review。
- source package 构建成功，且构建产物 manifest 与 artifact 记录一致。
- HealthCheck 如果存在，ready 或按策略允许 degraded。
- 高风险插件或 connection takeover 插件至少通过 quick self-test；涉及协议接管时建议通过 protocol-smoke。
- 高风险或连接路径插件的 benchmark/smoke 结果未超过 runtime limits。
- dispatch plan 不存在 blocking conflict，例如 connection takeover scope 重叠、provider 单例冲突或 middleware 排序环。
- connection takeover 插件的 Minecraft protocol version、forwarding mode 和 scope/protocol version 兼容性已检查。
- 插件声明的 transport、service name 和 upstream protocol 支持范围与当前 scope 兼容。
- 如果插件接管 HAProxy upstream 或自定义真实 IP forwarding，已检查不会与 gateway 默认 HAProxy protocol 重复或冲突。

建议启用前检查：

- supply chain、license、SBOM、signature 状态已查看。
- 准入策略结果、risk level、review 记录和 denylist 状态已查看。
- external dependency 数据传输、fail policy 和降级行为已查看。
- benchmark 摘要、基线对比和容量估算已查看。
- dispatch plan、warning conflict 和 shadowed handler 已查看。
- transport/upstream protocol 兼容矩阵已查看，尤其是 KCP、QUIC、WebSocket 和 HAProxy upstream。
- Minecraft 能力矩阵、unsupported protocol policy、modded 边界和 forwarding secret 要求已查看。
- 最近构建日志无明显风险。
- 配置快照已创建。
- 灰度 scope/rollout 已设置，尤其是 connection takeover 插件。
- 回滚 artifact 可用。

高风险变更需要二次确认：

- 启用 connection takeover 插件。
- 从 dry-run 切换到 enforced。
- capabilities 变更。
- secret 引用变更。
- major version 升级。
- 全局 scope 启用安全或认证类插件。
- 准入策略从 permissive 切换到 restricted 后首次启用。
- forwarding mode 变更、Minecraft state 从 transparent 改为 handled、开启 packet rewrite。
- transport 支持范围扩大到全局、多入口，或启用自定义 upstream protocol/真实 IP 转发。

### 错误诊断

Admin 页面和 API 需要展示足够可操作的错误：

- manifest 缺字段或 schema 错误。
- Go version、GOOS/GOARCH、SDK/API 版本不兼容。
- builder image 不匹配。
- 构建失败日志摘要。
- `plugin.Open` 失败原因。
- Plugin symbol 缺失。
- `ReloadConfig()` 失败。
- `Init()` 失败。
- handler panic、超时和错误计数。

### 兼容性提示

插件状态页需要展示：

- gateway version。
- gateway Go version。
- gateway ABI fingerprint。
- plugin API version。
- 插件声明的 Go version。
- 插件构建的 GOOS/GOARCH。
- 插件 ABI fingerprint 和差异项。
- 插件源码包 sha256 和产物 sha256。
- 当前 artifact 是否已加载过。
- 当前变更是否需要重启。

## Admin API

### 权限矩阵

沿用当前 Admin 角色：`admin`、`member`、`guest`。插件管理风险高，第一版写操作只给 `admin`。

| 操作 | guest | member | admin |
| --- | --- | --- | --- |
| 查看插件列表、manifest、capabilities | 否 | 是 | 是 |
| 查看运行状态、metrics、构建摘要 | 否 | 是 | 是 |
| 查看审计日志 | 否 | 否 | 是 |
| 上传 artifact 或 source package | 否 | 否 | 是 |
| 触发构建、重试构建、取消构建 | 否 | 否 | 是 |
| 加载、启用、禁用、删除插件 | 否 | 否 | 是 |
| 修改配置、priority、runtime limits | 否 | 否 | 是 |
| 切换版本、回滚 artifact | 否 | 否 | 是 |
| 创建、轮换、删除 plugin secret | 否 | 否 | 是 |
| 查看准入策略、风险评级和 review 记录 | 否 | 是 | 是 |
| 修改准入策略、审批、拒绝、撤销或解除隔离 | 否 | 否 | 是 |
| 导出、导入 promotion bundle 和应用跨环境变更 | 否 | 否 | 是 |
| 查看配置漂移和灾备演练报告 | 否 | 是 | 是 |
| 管理插件仓库、从仓库导入 artifact | 否 | 否 | 是 |
| 管理 Admin 外部登录 provider 和账号绑定，未来能力 | 否 | 否 | 是 |

member 可以读取插件状态是为了排障；不能读取 secret 明文，也不能执行会加载代码或改变流量的动作。

第一版可以继续使用三角色模型，但 API 内部应按稳定 permission key 判定权限，而不是把 `admin/member/guest` 写死到每个 handler。这样后续接入企业 SSO、项目空间、多租户或细粒度审批时，不需要重写插件 API。

推荐权限 key：

| Permission | 说明 | 默认角色 |
| --- | --- | --- |
| `plugin.read` | 查看插件列表、详情、manifest 和 runtime state | member、admin |
| `plugin.metrics.read` | 查看 metrics、trace 摘要和健康状态 | member、admin |
| `plugin.events.read` | 查看插件业务事件脱敏摘要 | member、admin |
| `plugin.logs.read` | 查看插件日志摘要和诊断入口 | member、admin |
| `plugin.audit.read` | 查看插件审计日志 | admin |
| `plugin.artifact.upload` | 上传 `.mcgp` 或 raw `.so` | admin |
| `plugin.artifact.delete` | 删除未使用 artifact 或执行 GC | admin |
| `plugin.build.manage` | 触发、重试、取消源码包构建 | admin |
| `plugin.load` | 执行会加载 native code 的 load | admin |
| `plugin.enable` | 启用、禁用、reload、切换 artifact | admin |
| `plugin.config.write` | 修改 config、scope、rollout、priority、runtime limits | admin |
| `plugin.rollback` | 回滚 artifact 或配置快照 | admin |
| `plugin.secret.write` | 创建、轮换、删除 plugin secret | admin |
| `plugin.data.read` | 查看 plugin_data 摘要 | member、admin |
| `plugin.data.export` | 导出允许迁移的 plugin_data value | admin |
| `plugin.data.gc` | 清理 plugin_data | admin |
| `plugin.files.read` | 查看插件文件资源、运行目录使用量和 GC candidate | member、admin |
| `plugin.files.gc` | 清理 cache/tmp/log/diagnostic 等可丢弃文件 | admin |
| `plugin.task.read` | 查看后台任务 schedule、状态和最近运行摘要 | member、admin |
| `plugin.task.run` | 手动触发或取消插件后台任务 | admin |
| `plugin.event_delivery.read` | 查看事件订阅投递状态、队列和死信摘要 | member、admin |
| `plugin.event_delivery.manage` | 重放或丢弃死信事件摘要 | admin |
| `plugin.action.run` | 执行插件声明式 action | admin |
| `plugin.policy.read` | 查看准入策略、风险评级和 review | member、admin |
| `plugin.policy.write` | 修改准入策略、denylist、quarantine | admin |
| `plugin.review.write` | 审批或拒绝 artifact/config/scope 组合 | admin |
| `plugin.promotion.write` | 导入、导出和应用 promotion bundle | admin |
| `plugin.repository.manage` | 管理插件仓库和导入候选版本 | admin |
| `admin.auth.provider.manage` | 管理 Admin 外部登录 provider、登录方式和账号绑定，未来能力 | admin |

权限检查规则：

- 所有写接口都必须检查 permission key、CSRF/confirm token、资源版本或 generation。
- 能加载代码、改变流量、读取敏感诊断、导出数据或执行 dangerous action 的接口必须写审计日志。
- `plugin.load`、`plugin.enable`、`plugin.rollback`、`plugin.policy.write` 和 `plugin.secret.write` 属于高风险权限，后续可单独拆给不同管理员。
- `plugin.data.export` 不等于 `plugin.data.read`；默认 member 只能看摘要，不能导出 value。
- `plugin.logs.read` 只能看脱敏摘要；完整诊断包需要 `plugin.audit.read` 或单独的未来权限。
- 插件声明式 action 可以在 manifest 标记 `required_permission`，但只能引用 gateway 已知 permission key，不能自定义绕过权限模型。
- Admin 外部登录 provider 只能映射到 gateway 已知 permission key 或默认角色，不能由插件自定义新权限并直接放行。
- 权限拒绝返回稳定 `permission_denied`，响应中可以包含缺失 permission key，但不能泄露 secret 或敏感配置。

资源范围：

第一版可以把权限作用于全局。后续扩展时，permission 应支持 resource scope：

```json
{
  "permission": "plugin.enable",
  "resource": {
    "plugin_id": "official.mc-auth-proxy",
    "environment": "prod",
    "route_tags": ["auth-required"]
  }
}
```

scope 扩展规则：

- 没有 scope 的 permission 表示全局权限。
- 有 scope 的 permission 只能作用于匹配的 plugin、route tag、environment 或 repository。
- promotion import/apply 必须同时检查源 bundle 操作权限和目标 plugin/scope 写权限。
- 多实例模式下 permission resource 应支持 `instance_id`。

接口草案：

| Method | Path | 说明 |
| --- | --- | --- |
| `GET` | `/admin/api/plugins` | 列出插件配置和运行状态 |
| `GET` | `/admin/api/plugins/operations` | 查询插件长操作列表和状态 |
| `GET` | `/admin/api/plugins/operations/{operation_id}` | 查询单个长操作进度、结果和错误 |
| `POST` | `/admin/api/plugins/operations/{operation_id}/cancel` | 请求取消可取消的长操作 |
| `GET` | `/admin/api/plugins/metrics` | 查看插件运行时指标快照 |
| `GET` | `/admin/api/plugins/features` | 查看当前 gateway 支持的 feature key、版本和状态 |
| `GET` | `/admin/api/plugins/traces` | 查询最近插件 trace 摘要，按 plugin、connection 或 trace 过滤 |
| `GET` | `/admin/api/plugins/traces/{trace_id}` | 查看单条 trace 摘要和关联日志入口 |
| `GET` | `/admin/api/plugins/admin-auth/providers` | 查看 Admin 外部登录 provider、登录方式和健康状态，未来能力 |
| `POST` | `/admin/api/plugins/admin-auth/providers/{id}/preflight` | 检查 OIDC/LDAP/SSO 配置、secret 和健康，未来能力 |
| `GET` | `/admin/api/plugins/admin-auth/identities` | 查看外部身份绑定摘要，未来能力 |
| `POST` | `/admin/api/plugins/admin-auth/identities` | 创建或更新外部身份绑定，未来能力 |
| `DELETE` | `/admin/api/plugins/admin-auth/identities/{provider_id}/{subject_hash}` | 删除外部身份绑定，未来能力 |
| `POST` | `/admin/api/plugins/artifacts` | 上传 `.mcgp` |
| `GET` | `/admin/api/plugins/artifacts/{artifact_id}` | 查看 artifact 元数据 |
| `DELETE` | `/admin/api/plugins/artifacts/{artifact_id}` | 删除未使用 artifact |
| `POST` | `/admin/api/plugins/artifacts/{artifact_id}/policy/evaluate` | 重新执行准入策略评估 |
| `POST` | `/admin/api/plugins/artifacts/{artifact_id}/reviews` | 审批或拒绝 artifact/config/scope/runtime limits 组合 |
| `POST` | `/admin/api/plugins/artifacts/{artifact_id}/revoke` | 将 artifact sha256 加入 denylist 并撤销 |
| `GET` | `/admin/api/plugins/artifact-gc/candidates` | 查看可清理 artifact、source 和日志 |
| `POST` | `/admin/api/plugins/artifact-gc/run` | 执行 artifact/source/log 清理 |
| `GET` | `/admin/api/plugins/policies/admission` | 查看插件准入策略 |
| `PUT` | `/admin/api/plugins/policies/admission` | 更新插件准入策略 |
| `GET` | `/admin/api/plugin-advisories` | 查看安全公告 |
| `POST` | `/admin/api/plugin-advisories` | 导入或创建本地安全公告、导入本地 feed，或提交 `rescan=true` 重新扫描本地 artifact |
| `GET` | `/admin/api/plugin-vulnerabilities` | 查看本地漏洞库，可按 package name 过滤 |
| `POST` | `/admin/api/plugin-vulnerabilities` | 导入或创建本地漏洞记录、导入本地漏洞库，或提交 `rescan=true` 按 SBOM dependency 重新扫描本地 artifact |
| `POST` | `/admin/api/plugins/security-advisories/{id}/matches/{artifact_id}/ack` | 确认、忽略或标记已缓解 |
| `POST` | `/admin/api/plugins/{id}/quarantine` | 按策略隔离已启用插件并停止新流量 |
| `POST` | `/admin/api/plugins/{id}/unquarantine` | 解除隔离，需重新通过门禁 |
| `POST` | `/admin/api/plugins/promotion/export` | 导出一个或多个插件的 promotion bundle |
| `POST` | `/admin/api/plugins/promotion/import` | 上传并校验 promotion bundle |
| `POST` | `/admin/api/plugins/promotion/import/{id}/diff` | 生成目标环境 diff 和漂移报告 |
| `POST` | `/admin/api/plugins/promotion/import/{id}/apply` | 应用导入结果到本地 desired state |
| `GET` | `/admin/api/plugins/drift` | 查看当前环境相对基线的插件漂移状态 |
| `POST` | `/admin/api/plugins/dr-drills` | 创建插件灾备演练任务 |
| `GET` | `/admin/api/plugins/dr-drills/{id}` | 查看灾备演练报告 |
| `GET` | `/admin/api/plugins/dispatch-plan` | 查看当前 dispatch plan、排序和遮蔽诊断 |
| `POST` | `/admin/api/plugins/conflicts/check` | 对拟启用或拟更新插件执行冲突分析 |
| `GET` | `/admin/api/plugins/builds` | 列出源码包构建任务 |
| `GET` | `/admin/api/plugins/builds/{build_id}` | 查看构建状态和日志摘要 |
| `POST` | `/admin/api/plugins/builds/{build_id}/retry` | 用同一源码包重新构建 |
| `POST` | `/admin/api/plugins/builds/{build_id}/cancel` | 取消排队中或运行中的构建 |
| `GET` | `/admin/api/plugins/{id}` | 查看单个插件 |
| `PUT` | `/admin/api/plugins/{id}` | 更新 priority、scope、rollout、dry-run、config、artifact |
| `POST` | `/admin/api/plugins/{id}/load` | 热加载插件代码 |
| `POST` | `/admin/api/plugins/{id}/enable` | 启用插件 |
| `POST` | `/admin/api/plugins/{id}/disable` | 禁用插件 |
| `POST` | `/admin/api/plugins/{id}/reload` | 重新应用配置；不能真正卸载已加载 Go plugin |
| `POST` | `/admin/api/plugins/{id}/preflight` | 对当前或拟更新配置执行插件预检和通用门禁检查 |
| `POST` | `/admin/api/plugins/{id}/self-test` | 执行插件声明支持的 quick/protocol-smoke/integration 自测 |
| `POST` | `/admin/api/plugins/{id}/health-check` | 手动触发健康检查 |
| `GET` | `/admin/api/plugins/{id}/external-dependencies` | 查看外部依赖声明、状态、超时、失败策略和最近错误 |
| `GET` | `/admin/api/plugins/{id}/external-dependencies/{dependency_id}` | 查看单个外部依赖的声明、受控调用统计、熔断状态和最近错误 |
| `POST` | `/admin/api/plugins/{id}/external-dependencies/{dependency_id}/health-check` | 手动触发单个外部依赖健康检查 |
| `POST` | `/admin/api/plugins/{id}/rollback` | 回滚到指定历史 artifact |
| `GET` | `/admin/api/plugins/{id}/config-snapshots` | 查看配置/desired state 快照列表 |
| `POST` | `/admin/api/plugins/{id}/config-snapshots/{snapshot_id}/diff` | 查看快照与当前 desired state 的脱敏 diff |
| `POST` | `/admin/api/plugins/{id}/config-snapshots/{snapshot_id}/rollback` | 回滚 config 或完整 desired state 到指定快照 |
| `POST` | `/admin/api/plugins/{id}/draining/force-close` | 强制关闭 draining 的 connection takeover 连接 |
| `GET` | `/admin/api/plugins/{id}/events` | 查看插件业务事件脱敏摘要 |
| `GET` | `/admin/api/plugins/{id}/route-decisions` | 查看插件动态路由和上游覆盖的最近决策摘要 |
| `GET` | `/admin/api/plugins/{id}/event-subscriptions` | 查看事件订阅、队列、投递状态和死信摘要 |
| `POST` | `/admin/api/plugins/{id}/event-subscriptions/{subscription_id}/dead-letter/replay` | 手动重放死信事件摘要 |
| `POST` | `/admin/api/plugins/{id}/event-subscriptions/{subscription_id}/dead-letter/drop` | 丢弃死信事件摘要 |
| `GET` | `/admin/api/plugins/{id}/custom-metrics` | 查看插件自定义指标声明和最近摘要 |
| `GET` | `/admin/api/plugins/{id}/logs` | 查看插件最近日志和错误摘要 |
| `GET` | `/admin/api/plugins/{id}/diagnostics` | 下载插件诊断包，未来能力 |
| `GET` | `/admin/api/plugins/{id}/background-tasks` | 查看后台任务状态 |
| `POST` | `/admin/api/plugins/{id}/background-tasks/{task_id}/run` | 手动触发允许 manual trigger 的后台任务 |
| `POST` | `/admin/api/plugins/{id}/background-tasks/{task_id}/cancel` | 请求取消当前运行中的后台任务 |
| `GET` | `/admin/api/plugins/{id}/node-states` | 查看多实例节点状态，未来能力 |
| `GET` | `/admin/api/plugins/{id}/data` | 查看 plugin_data 摘要、schema version、data class、大小和配额 |
| `POST` | `/admin/api/plugins/{id}/data/gc` | 按 retention 清理过期或可丢弃 plugin_data |
| `POST` | `/admin/api/plugins/{id}/data/snapshots` | 创建 plugin_data 快照，未来能力 |
| `GET` | `/admin/api/plugins/{id}/files` | 查看插件文件资源、运行目录使用量、配额和 GC candidate |
| `POST` | `/admin/api/plugins/{id}/files/gc` | 清理插件 cache/tmp/log/diagnostic 等可丢弃文件 |
| `GET` | `/admin/api/plugins/{id}/admin-ui` | 查看声明式 Admin UI schema、状态面板和 action 定义 |
| `GET` | `/admin/api/plugins/{id}/actions` | 查看插件可执行 action |
| `POST` | `/admin/api/plugins/{id}/actions/{action_id}` | 执行插件 action |
| `GET` | `/admin/api/plugins/{id}/secrets` | 查看 secret 配置状态，不返回明文 |
| `PUT` | `/admin/api/plugins/{id}/secrets/{name}` | 创建或轮换 secret |
| `POST` | `/admin/api/plugins/{id}/secrets/{name}/rotate` | 创建新 secret version 并按 rotation 策略触发 reload |
| `POST` | `/admin/api/plugins/{id}/secrets/{name}/revoke-version` | 撤销指定 secret version，可能触发 force close |
| `DELETE` | `/admin/api/plugins/{id}/secrets/{name}` | 删除 secret |
| `DELETE` | `/admin/api/plugins/{id}` | 删除插件 |
| `GET` | `/admin/api/plugin-repositories` | 列出插件仓库，未来能力 |
| `POST` | `/admin/api/plugin-repositories` | 添加插件仓库，未来能力 |
| `POST` | `/admin/api/plugin-repositories/{id}/check` | 检查仓库更新，未来能力 |
| `POST` | `/admin/api/plugin-repositories/{id}/import` | 下载仓库 artifact 到本地，未来能力 |
| `DELETE` | `/admin/api/plugin-repositories/{id}` | 删除仓库配置，未来能力 |

所有写接口必须记录审计日志：

- actor
- source IP
- action
- target type: `plugin`、`plugin_artifact`、`plugin_build`、`plugin_secret` 或 `plugin_repository`
- target id
- success
- message
- metadata_json，保存 plugin ID、artifact ID、operation ID、generation、policy hash 等脱敏机器字段

接口返回原则：

- artifact detail 必须包含 manifest、metadata、sha256、Go/API/ABI fingerprint 兼容性、supply chain、documentation、license、SBOM 和 signature 状态。
- artifact detail 必须包含 required/optional feature 协商结果、缺失 feature 和 compat warning。
- artifact detail 必须包含 admission status、risk level、policy result、review status 和 denylist 命中原因。
- artifact detail 必须包含 active advisory matches、severity、recommended action、fixed version 和 mitigation status。
- artifact detail 如果包含 `minecraft` 声明，必须展示 protocol versions、tested versions、states、auth modes、forwarding、modded 和 packet features。
- artifact upload 失败必须返回明确的包校验错误，例如 zip slip、manifest 缺失、文件过大或 runtime entry 缺失。
- 会进入异步执行的接口必须返回 `operation_id`；重复请求携带同一 idempotency key 时应返回同一未完成 operation。
- plugin detail 必须包含 desired state、runtime state、active artifact、可回滚 artifact、依赖状态、secret 状态、ABI fingerprint diff 和 restart required 原因。
- plugin detail 必须包含 scope、rollout、dry-run、health、background task 和最近日志摘要。
- plugin detail 应包含最近 preflight/self-test 摘要、状态、证据 ID 和阻断原因。
- plugin detail 应包含 composition constraints、conflict status、shadowed handler 提示和 dispatch order 摘要。
- plugin detail 应包含 external dependencies、endpoint 摘要、fail policy、secret ref 状态、data classes 和 dependency health。
- external dependency detail 应包含 declared endpoints、effective timeout、max_concurrent、retry、fail policy、data classes、health、circuit state、inflight、最近调用结果和最近错误摘要。
- external dependency detail 可以展示 observed controlled calls；native `go-plugin` 未经 ExternalClient 的外联只能作为未知风险提示，不能假装已被完整审计。
- plugin detail 应包含 Minecraft 协议能力声明与当前 scope/protocol version 的兼容提示。
- plugin detail 应包含插件声明的业务事件、自定义指标 schema 和最近事件摘要入口。
- plugin detail 应包含动态 route decision 摘要入口、provider cache 状态和 SQLite fallback 状态。
- plugin detail 应包含 exporter 状态、最近导出错误、metric/event drop 和高基数字段拒绝统计。
- plugin detail 应包含 event subscriptions、delivery mode、queue depth、drop count、dead letter count 和最近投递错误。
- plugin detail 应包含最近 slow/error trace 摘要、trace sampling 状态和诊断入口。
- plugin detail 应包含 plugin_data schema version、data class 汇总、配额使用、GC candidate 和 orphaned data 状态。
- plugin detail 应包含 file resource 摘要、data/cache/tmp/log/diagnostic 用量、配额、retention、GC candidate 和 orphaned runtime dir 状态。
- background task detail 应包含 schedule、run_on_start、manual_trigger、timeout、non_reentrant、last/next run、consecutive failures、skipped count、最近错误和最近运行摘要。
- dispatch plan API 必须能解释每个 extension point 的最终 handler 顺序、scope 命中范围、blocking/warning conflict 和 shadowed handler。
- 多实例模式下 plugin detail 应区分 global desired state 和 node runtime state。
- build detail 返回日志摘要和日志下载/查看入口；默认不返回完整日志，避免泄露环境信息。
- secret API 永远不返回明文；更新成功只返回状态、版本 ID、rotation state、not_before/not_after 和更新时间。
- secret rotate API 必须返回受影响插件、reload 策略、grace period、是否需要 backend 同步和 operation ID。
- admin-ui API 只返回声明式 schema 和状态摘要，不返回插件自定义 HTML/JS。
- action API 必须检查权限、confirm token、timeout 和并发限制，并写入审计日志。
- action API 返回值必须脱敏，不能返回 secret、token、完整日志或未分页大列表。
- background task manual run API 必须检查权限、confirm token、任务是否允许手动触发、non-reentrant 状态、timeout 和 idempotency key，并写入审计日志。
- preflight/self-test API 必须有 timeout、并发限制、脱敏结果、operation ID 和稳定错误码；protocol-smoke/integration profile 需要管理员确认或发布门禁上下文。
- plugin_data API 只返回摘要和统计，不默认返回 value_json；导出 value 需要 admin 权限和 data_class 校验。
- plugin files API 默认只返回路径摘要、namespace、data class、大小和清理候选；不返回文件内容，导出文件需要后续单独权限和脱敏流程。
- policy API 必须返回策略快照版本，避免管理员审批后策略已变化却继续使用旧判断。
- review API 的批准结果只绑定 artifact sha256、config hash、scope hash、rollout hash、runtime limits hash、features hash 和 policy hash。
- revoke API 必须阻止后续 enable/rollback 到被撤销 artifact；已加载 native code 需要提示重启彻底清理。
- promotion export 不能返回 secret 明文、secret 密文、KMS key 或运行时状态。
- repository import 只生成本地 artifact，不自动创建 desired state；导入记录必须保存当前策略下的 admission preview，包含 policy hash、risk、阻断 issue 和 auto-enable=false。
- repository import apply 必须使用目标环境提供的 config，重新执行 config dry-run 和当前 governance；成功时只写入 disabled desired state，不自动 enable active 流量，响应不能回显 config 明文或 secret 值。
- promotion import 必须返回 artifact/config/scope/rollout/runtime limits/features/policy diff、缺失 secret、兼容性错误和发布门禁结果。
- promotion apply 必须使用目标环境提供的 config，与 bundle `config_hash` 匹配后重新执行 dry-run 和 governance；响应只能返回 desired metadata、hash、check 和审计摘要，不能回显 config 明文或 secret 值。
- promotion import 必须返回 external dependency endpoint、secret ref、fail policy 和 timeout diff。
- config snapshot diff 必须使用 canonical hash 和脱敏 diff；rollback 必须重新执行当前准入策略和发布门禁，不能因为历史快照曾经可用就绕过策略。
- drift API 必须使用 artifact sha256、canonical hash 和 desired fingerprint，不应只比较 plugin version 字符串。
- DR drill report 默认只包含摘要和校验结果，不包含敏感配置值。
- trace API 默认只返回脱敏摘要；完整 trace 需要 admin 权限，且不得包含 secret、token、完整 packet 或玩家隐私原文。
- events API 默认只返回脱敏摘要、低基数属性和 trace/connection 关联 ID；不返回明文玩家身份、session response 或 packet payload。
- event subscription API 默认只返回投递状态和脱敏事件摘要；死信重放必须检查权限、幂等键和订阅仍然存在。
- custom metrics API 返回声明、最近值和 drop/error 计数；不返回未声明 label 或高基数原始值。
- repository API 第一版可以只做设计预留；如果实现，导入后也必须走本地 artifact 审核和启用流程。
- admin-auth provider API 第一版可以只做设计预留；如果实现，登录 session 仍由 gateway 签发，插件只返回外部身份结果和脱敏证据。
- admin-auth provider 状态不可影响 Minecraft 连接路径；OIDC/LDAP/SSO 不可用只影响管理页外部登录方式，本地 break-glass admin 登录必须保留。

API 错误响应应包含稳定错误码，便于管理页和 CLI 处理：

```json
{
  "error": {
    "code": "validation_failed",
    "message": "manifest runtime.entry is missing",
    "details": {
      "field": "runtime.entry"
    }
  }
}
```

推荐错误码：

| Code | HTTP | 说明 |
| --- | --- | --- |
| `validation_failed` | 400 | manifest、config 或请求参数校验失败 |
| `permission_denied` | 403 | 当前用户无权限 |
| `not_found` | 404 | plugin、artifact、build 或 secret 不存在 |
| `conflict` | 409 | generation 冲突或操作正在进行 |
| `precondition_failed` | 412 | 依赖、secret、runtime、Go/API 版本不满足 |
| `feature_missing` | 412 | 插件 required feature 当前 gateway 不支持 |
| `payload_too_large` | 413 | 上传包或解压后内容超过限制 |
| `runtime_failed` | 500 | plugin.Open、Init、HealthCheck 或 runtime adapter 失败 |
| `restart_required` | 202 | 操作已接受，但需要重启彻底生效 |

## Admin 页面

新增插件页面，放在管理员可见区域。

列表字段：

- 插件名
- 插件 ID
- 版本
- artifact 类型：binary/source
- 来源：manual upload、source build、repository import
- 启用状态
- 加载状态
- Go 版本
- API 版本
- extension points
- capabilities 摘要
- license
- signature 状态
- priority
- scope / rollout / dry-run
- restart required
- runtime state：enabled、disabled、degraded、draining、failed
- health：ready、degraded、not_ready
- alert 状态
- active calls / active proxy connections
- background task 状态
- error rate、timeout count、panic count
- 最近错误
- 环境漂移状态：identical、artifact drift、config drift、missing secret 等
- 是否有可用更新，未来仓库能力

详情页能力：

- 展示 manifest metadata。
- 展示 supply chain：来源、homepage、repository、commit、license、SBOM、checksums 和 signature 状态。
- 展示安全公告命中、severity、影响范围、fixed version、推荐动作和缓解状态。
- 展示 runtime 类型和 capabilities 声明。
- 展示 gateway 插件服务启动模式：active mode、desired mode、是否需要重启、plugin-host/supervisor 状态和迁移能力。
- 展示 required/optional features、缺失 feature、compat warning 和当前 gateway supported features。
- 展示 Minecraft 协议能力：protocol versions、tested versions、states、auth modes、forwarding、modded 和 packet features。
- 展示准入策略结果、risk level、review 记录、denylist 命中原因和策略快照版本。
- 展示 dependencies、secret 配置状态和 runtime limits。
- 展示 secret type、current/previous version、rotation state、grace period、reload 策略和到期提示。
- 展示 plugin_data：schema version、data class、配额、大小、retention、GC candidate 和 orphaned 状态。
- 展示插件文件资源：包内 resources、data/cache/tmp/log/diagnostic 用量、配额、retention 和 GC candidate。
- 展示 external dependencies：endpoint 摘要、用途、required、timeout、fail policy、secret refs、data classes 和健康状态。
- 展示 composition constraints、dispatch order、conflict status 和 shadowed handler 诊断。
- 展示声明式 Admin UI：config layout、status panels、docs links 和受控 action。
- 展示资源治理实际生效值，并明确 native plugin 不能强制 CPU/内存隔离。
- 展示当前 artifact 和可切换 artifact。
- 展示 artifact provenance：上传者、上传时间、构建任务、builder image、source sha256、artifact sha256。
- 展示 artifact 保留策略、GC candidate、pending cleanup 和预计释放空间。
- 展示 benchmark 摘要、profile、P95/P99、容量估算、基线对比和报告下载入口。
- 展示 promotion provenance：源环境、目标环境、bundle ID、导入时间、应用人和基线指纹。
- 展示跨环境 diff：artifact、config、scope、rollout、priority、runtime limits 和 secret refs。
- 展示源码包构建历史、构建状态、构建日志摘要和错误。
- 展示运行时治理指标、熔断状态和 draining 状态。
- 展示插件业务事件 schema、最近事件摘要、事件 drop 计数和 trace/connection 关联入口。
- 展示自定义指标 schema、最近值、drop/error 计数和高基数标签拒绝摘要。
- 展示最近 slow/error trace 摘要、connection ID、trace ID 和关联日志入口。
- 展示性能预算、slow call、P95/P99 延迟和告警静默状态。
- 展示 benchmark 退化比例、是否超过 runtime limits 和是否需要高风险确认。
- 展示 scope、rollout percentage、sticky key、dry-run 状态和命中统计。
- 展示 scope 中 protocol version 与插件 Minecraft 能力声明的兼容性提示。
- 展示 health check 状态、最近检查时间、失败摘要和手动检查按钮。
- 展示外部依赖健康状态、最近错误、熔断状态和手动探测按钮。
- 展示后台任务列表、最近运行时间、耗时、失败次数和 skipped 次数。
- 展示插件声明式 action，并对 dangerous action 做二次确认。
- 展示插件日志和诊断包入口。
- 展示发布门禁检查结果和高风险变更二次确认。
- 展示 dispatch plan 预览、scope overlap、blocking conflict、warning conflict 和建议修复方式。
- 展示 Admin 外部登录 provider 状态、login method、账号绑定摘要和本地 break-glass 可用性，未来能力。
- 支持重新执行策略评估、提交 review、拒绝 artifact、撤销 artifact 和隔离插件。
- 支持导入本地安全公告、重新扫描 artifact、确认/忽略/标记缓解 advisory match。
- 编辑 `config_json`，后续可根据 `config_schema` 渲染表单。
- 渲染 config schema UI hint：secret ref、sensitive、advanced、restart/reload required、CIDR、host、duration 等。
- 展示配置版本，插件升级时支持配置迁移预览。
- 展示插件私有数据迁移预览、快照状态和回滚风险。
- 支持 artifact 回滚和配置快照选择。
- 支持 secret 配置、版本轮换、撤销、reload 结果和删除确认。
- 支持导出 promotion bundle，导入 bundle，完成 secret mapping 和环境覆盖。
- 支持展示配置漂移、重新设定基线和查看灾备演练报告。
- 支持查看仓库候选版本和手动导入，未来能力。
- 加载、启用、禁用、删除。
- 清理未使用 artifact、source package 和过期构建日志。
- 展示是否需要重启以及原因。
- 多实例模式下展示节点维度状态、partial rollout failure 和每个节点的 restart required。

页面文案需要明确：

- 上传不会执行插件代码。
- 源码包上传后需要先构建，构建成功后才会出现可加载 artifact。
- 构建隔离不等于运行隔离；`source + go-plugin` 运行后仍是 trusted native plugin。
- native `go-plugin` 的 capabilities 第一版只做声明和审计，不是强制沙箱。
- `go-plugin-process` 是未来可选服务启动模式，需要重启 gateway 生效；它通过退出子进程回收已加载 `.so`，不代表 Go plugin 原生支持 unload，也不等同于 sandbox。
- 第一版不运行插件自带 HTML/JavaScript；插件 UI 只能通过声明式 schema 和受控 action 渲染。
- secret 不会在页面、日志或审计中明文展示。
- trace 页面默认展示脱敏摘要，trace attr 不包含玩家名、UUID、IP 原文、secret、token 或完整 packet。
- promotion bundle 不包含 secret 明文或密文；跨环境导入必须重新映射目标环境 secret。
- 插件可能把 data_classes 中声明的数据发送到外部依赖；管理页应提示外部数据传输风险。
- drift 状态只说明期望状态是否偏离基线，不代表插件运行健康。
- 签名、SBOM、license 和 documentation 第一版默认记录、展示并进入 warning/review；如果没有组织策略，不作为启用阻断条件。
- 如果组织准入策略设置为 restricted，policy blocking risk 会阻断启用和回滚。
- 撤销已加载 Go plugin 不能从内存卸载代码；隔离后仍建议重启彻底清理。
- 仓库只负责发现和下载，不能绕过管理员 review 直接启用。
- Admin 外部登录插件只影响管理页登录，不参与 MC 正版/三方登录；本地 admin 兜底账号必须保留。
- 加载会执行插件代码。
- 禁用不会从进程中卸载 Go plugin。
- 删除已加载插件后，可能需要重启才能彻底清理。

## 能力边界

### 插件可以独立实现

这些能力可以通过 trusted native plugin 和当前设计的 extension point 实现，不要求 gateway core 理解具体业务协议：

- 自定义 upstream 拨号。
- 自定义 tunnel、代理和服务发现。
- connection takeover mode 下的完整 Minecraft 协议代理。
- MC 正版/三方登录、身份转发和登录后协议处理。
- 玩家维度白名单、黑名单、ban、会员、权限、风控和分流策略，前提是由 connection takeover 插件自己解析并持有玩家身份。
- Minecraft status ping、MOTD、维护模式、unsupported version 提示和 modded handshake 兼容，第一版可由 connection takeover 插件自行实现。
- 在 gateway 已有 TCP/KCP/QUIC/WebSocket 入口上按 transport/upstream protocol 做差异化策略。
- 连接级策略、限流、黑白名单。
- 动态后端选择、灰度、蓝绿、fallback。
- 周期同步外部路由、白名单、封禁列表、资源文件、证书或缓存预热。
- 受控 Admin action，例如刷新缓存、探测 backend、清理 profile cache 和生成诊断摘要。
- 外部系统集成，例如数据库、HTTP API、消息队列、告警、监控。

### 需要 gateway core 提供稳定支撑

这些不是业务逻辑，但必须由 gateway core 稳定提供，否则插件难以可靠实现：

- extension point dispatch table 快照和确定性排序。
- handler 调用不能持有全局锁。
- panic、超时和错误边界。
- 初始 handshake 包回放到插件返回的 `net.Conn`。
- 插件生命周期、启用/禁用、版本切换和状态快照。
- 运行时治理：超时、并发限制、熔断、draining。
- Admin 上传、构建、artifact 管理、审计日志和权限控制。
- 插件配置解码、schema 展示和错误诊断。
- 配置迁移和插件私有数据存储。
- secret 管理、依赖检查、回滚和备份恢复。
- 外部依赖受控调用、后台任务调度、PluginDataStore、PluginFileStore 和配额治理。
- 业务事件、自定义指标、trace、审计 sink 和脱敏诊断摘要。
- 声明式 Admin UI、受控 action、权限检查、confirm token 和审计记录。
- 入口服务生命周期、端口冲突检查、TCP/Admin 端口复用分流和服务启停状态。

### 不适合当前 native plugin 直接承诺

这些能力需要 sandbox-process、WASM、container runtime 或 build-time instrumentation 等后续设计：

- 安全运行不可信第三方插件。
- 真正热卸载 Go plugin 代码。
- 跨语言插件直接返回 `net.Conn`。
- 运行时 monkey patch gateway 内部函数。
- 对 gateway 没有 extension point 的内部位置做热插拔修改。
- 插件直接新增监听端口或长期入口服务；未来需要 `ingress.service/v1` 和 supervisor/service 管理。
- 普通插件携带构建期插桩规则直接修改 gateway binary；只允许官方或组织 CI 的 build-time instrumentation。
- 让 gateway core 在第一版消费 MC 登录结果、玩家身份或 packet payload 后继续编排业务流程。
- 把应用层连接过滤当作完整 DDoS 防护、地理合规系统或反作弊系统；这些只能作为插件策略输入或外部系统集成，不能替代网络层防护和专门反作弊能力。
- 第三方插件市场、签名分发和自动升级。

## 安全与运维约束

### 威胁模型

第一版插件是可信 native code，不能把它当作不可信第三方代码隔离运行。但可信不等于无风险，设计需要覆盖以下威胁：

| 威胁 | 风险 | 第一版缓解 |
| --- | --- | --- |
| 恶意或被篡改插件 | 读取 secret、执行任意代码、破坏进程 | 只允许 admin 管理、记录 sha256、展示供应链元数据、预留签名 |
| 已知恶意 artifact 被再次启用 | 回滚或跨环境导入重新引入风险代码 | denylist、准入策略、撤销审计、阻断 enable/rollback |
| Go ABI 不兼容 | 加载失败或启动失败 | Go/API/GOOS/GOARCH preflight，manifest schema 校验 |
| 插件 panic 或阻塞 | 连接路径故障、goroutine 堆积 | panic recover、timeout、并发限制、熔断 |
| connection takeover 插件实现错误 | 玩家无法登录、身份转发错误 | 示例、测试 harness、health check、dry-run 限制 |
| secret 泄漏 | forwarding secret、外部 API token 泄漏 | SecretStore、日志脱敏、审计不记录明文、诊断包脱敏 |
| 源码包构建供应链风险 | 构建时访问外网、私有 token 泄漏 | builder 隔离、环境变量白名单、vendor 模式、日志限制 |
| 上传包攻击 | zip slip、zip bomb、特殊文件 | 静态包校验、大小限制、拒绝特殊文件 |
| 资源耗尽 | CPU、内存、连接、日志、磁盘被耗尽 | 应用层配额、GC、日志限速、runtime limits |
| 配置误操作 | 全站流量被错误插件影响 | scope、rollout、dry-run、配置快照、回滚 |
| 多实例不一致 | 部分节点加载失败或版本不一致 | node runtime state、partial rollout 展示、节点级 rollback |

明确不缓解的风险：

- native 插件可以执行任意 Go 代码。
- native 插件可以访问进程权限内的文件、网络和环境。
- native 插件可以通过全局状态影响同进程其他代码。
- native 插件可能导致进程级资源耗尽。

这些风险需要通过组织信任、代码审查、构建流水线、签名和未来 sandbox runtime 控制。

### 数据分类和隐私

插件系统会处理多类数据，需要明确哪些可以记录、展示和导出。

| 数据 | 示例 | 处理策略 |
| --- | --- | --- |
| 公开元数据 | plugin ID、name、version、license | 可展示和审计 |
| 供应链数据 | sha256、SBOM、signature、repository URL | 可展示，注意内部仓库 URL 可按权限隐藏 |
| 配置数据 | host、upstream、策略参数 | 可审计摘要，sensitive 字段脱敏 |
| secret | forwarding secret、API token | 只在 SecretStore 中保存和读取，不展示明文 |
| 玩家身份 | username、UUID、profile properties | 默认不进指标标签；日志需脱敏或采样 |
| 网络数据 | source IP、route、host | 可用于排障和 scope，长期保留需受日志策略限制 |
| 协议载荷 | Minecraft packet payload、session response | 默认不记录；debug 时需显式打开并限时 |
| 构建数据 | module list、go version、build log | 可展示摘要，剔除环境变量和凭据 |

隐私和日志规则：

- 指标标签不得包含玩家名、UUID、IP 原文或 host 原文，除非明确接受高基数和隐私风险。
- 审计日志只记录操作和目标，不记录 secret 明文或完整协议载荷。
- 插件日志默认不记录完整 Minecraft packet。
- 诊断包必须脱敏 secret、token、session response 和 sensitive config。
- 数据保留策略应覆盖 plugin logs、build logs、diagnostics 和 audit logs。
- 如果部署环境有合规要求，插件作者需要在 README 中说明其外部数据传输和保留行为。

- 插件是可信代码，等同于 gateway 进程权限。
- 只有 admin 可以管理插件。
- secret 只能通过受控 SecretStore 读取，不能写入普通配置或日志。
- 插件目录不能允许普通用户写入。
- 上传解压必须防路径穿越。
- artifact 使用 sha256 content-addressed 存储，避免覆盖已加载路径。
- 管理页必须展示 sha256，方便运维核对。
- 插件 extension point handler 在连接路径执行时，必须避免长时间阻塞。
- gateway 不应在持有全局锁时调用插件代码。
- 插件 panic 应被边界捕获并记录，不能直接打崩连接处理 goroutine。
- 插件不应在 package `init()` 中启动 goroutine、读取 secret、打开长期连接或注册不可撤销的全局副作用。
- 插件不应把可变业务状态放在 package 级全局变量；如确需全局缓存，必须声明风险并确保不会跨 generation 污染。
- 插件的 goroutine、文件句柄、网络连接必须在 `Destroy()` 中释放。
- `Destroy()` 必须幂等，超时或返回错误时 gateway 仍应移除 dispatch table 中的 handler。
- 禁用插件时先停止新流量进入插件，再调用 `Destroy()`。

## 运维 Runbook

插件系统需要给管理员明确故障处置路径。

### 插件导致连接失败

处置步骤：

1. 在插件列表查看 error rate、timeout、panic、active proxy connections。
2. 如果是灰度插件，先把 rollout percentage 调到 0。
3. 如果是普通 hook，执行 disable，让新连接绕过插件。
4. 如果是 connection takeover 插件，先 disable 新流量，再观察 draining。
5. 必要时执行 force-close draining connections。
6. 回滚到上一个 artifact 或配置快照。
7. 导出诊断包，保留 artifact sha256、日志摘要和审计记录。

### 插件启用失败

排查顺序：

1. manifest 和包校验错误。
2. Go/API/GOOS/GOARCH 兼容性。
3. 缺失 dependency 或 secret。
4. `ReloadConfig()` dry run 错误。
5. `plugin.Open`、Plugin symbol 错误。
6. `Init()` 或 HealthCheck 错误。
7. extension point 未声明或 handler 签名不匹配。

启用失败不得影响旧 dispatch table。

### 源码包构建失败

处置步骤：

1. 查看 build log excerpt 和 builder image。
2. 确认 Go version、GOOS/GOARCH、CGO 和 build tags。
3. 检查 GOPROXY/vendor/private dependency 配置。
4. 使用 CLI 在本地或 CI 复现 `gateway plugin build`。
5. 修正源码包后重新上传，或 retry 同一 build job。

构建失败不应改变 active artifact。

### secret 正常轮换

处置步骤：

1. 查看 secret 引用关系，确认受影响插件、backend 和 external dependency。
2. 对 `dual_read` secret，先更新 backend 或外部系统使新旧值都可接受。
3. 在 Admin 中创建新 secret version，设置 grace period。
4. 触发 hot reload 或标记 reload required。
5. 观察插件 health、auth failure、forwarding failure 和 secret reload 审计事件。
6. grace period 结束后撤销旧版本。
7. 对 connection takeover 插件，确认新连接使用新版本，旧连接自然结束或按维护窗口关闭。

### secret 泄漏怀疑

处置步骤：

1. 立即将相关 secret version 标记为 revoked。
2. 创建新 secret version，并按策略 hot reload、reload 或 disable 依赖插件。
3. 如果是 forwarding secret 泄漏，后端也必须同步轮换；无法 dual-read 时先关闭新流量。
4. 检查插件日志、审计日志和诊断包是否出现明文。
5. 对可能受影响的 connection takeover 连接执行 draining 或 force-close。
6. 导出审计记录，标记相关 artifact、插件版本和 secret version。

### 插件安全公告命中

处置步骤：

1. 查看 advisory severity、affected rule、match type 和 recommended action。
2. 如果有 fixed version，先执行 compat、policy evaluation 和 staging smoke test。
3. 对 `block_new_enable` 或更高动作，阻断 rollback 和 promotion apply 到受影响 artifact。
4. 对 `quarantine` 或 `revoke`，从 dispatch table 移除插件并处理 connection takeover draining。
5. 如果 advisory 要求 secret rotation，执行对应 secret 正常轮换或泄漏处置 Runbook。
6. 标记 advisory match 为 mitigated、acknowledged 或 ignored；ignored 必须有有效期和原因。
7. 导出诊断和审计记录，记录旧 artifact、新 artifact、advisory ID 和处理人。

### 磁盘占用过高

处置步骤：

1. 查看 artifact、source package、build log 和 diagnostics 占用。
2. 查看 GC candidates。
3. 确认 active、desired、snapshot referenced 和 loaded artifact 不在清理列表。
4. 执行 artifact GC。
5. 如存在 pending cleanup，安排 gateway 重启窗口。

### 多实例部分失败

处置步骤：

1. 查看每个 node 的 plugin runtime state、artifact ID 和 generation。
2. 对失败节点执行 reload 或重启。
3. 如果失败集中在某个 artifact，停止全局 rollout。
4. 回滚失败节点或全局 desired artifact。
5. 确认所有节点 draining 完成后再清理旧 artifact。

## 示例插件

第一版需要提供一个简单示例和一个能力上限示例。后续可以增加 Minecraft status 示例。

### Upstream Rewrite 示例

需要提供 `examples/plugins/upstream-rewrite`：

```text
examples/plugins/upstream-rewrite/
  go.mod
  main.go
  main_test.go
  manifest.json
  README.md
  testdata/
    config.json
    fixtures/
```

示例能力：

- 读取配置：

```json
{
  "match_host": "play.example",
  "upstream": "127.0.0.1:25566"
}
```

- 注册 `legacy upstream-connect contract` extension point handler。
- 当 `ServerHost` 命中配置时，插件自己 `net.Dial` 到配置 upstream 并返回连接。
- 不命中时返回 pass，让 gateway 走默认 upstream。

### Minecraft Auth Proxy 示例

需要提供 `examples/plugins/mc-auth-proxy`，用于展示 connection takeover mode 的能力边界。它可以先作为文档级或实验性示例存在，不要求第一阶段完整生产可用。

```text
examples/plugins/mc-auth-proxy/
  go.mod
  main.go
  main_test.go
  manifest.json
  README.md
  testdata/
    config.json
    fixtures/
```

示例能力：

- 注册 `legacy upstream-connect contract` extension point handler。
- 命中指定 host 时返回插件自管 `net.Conn`。
- 插件内部解析 handshake 和 login start。
- 插件内部完成正版或三方 Yggdrasil session 校验。
- 支持至少一个 third-party Yggdrasil-like endpoint 配置示例。
- 支持 profile cache、negative cache 和认证源超时配置示例。
- 认证成功后连接 backend。
- 使用 Velocity modern forwarding 或等价机制把 profile 转发给 backend。
- 认证失败时返回 Minecraft 登录失败响应。
- 通过 `EmitEvent` 上报 `auth.success`、`auth.failure`、`forwarding.failed` 等脱敏业务事件。
- 通过 `RecordMetric` 上报 auth attempts、session server latency 和 backend dial result 等低基数指标。
- 展示 forwarding secret 通过 SecretStore 引用，不写入普通配置。
- 展示 session server 超时、backend dial 失败和 unsupported protocol version 的失败策略。

该示例说明：`legacy upstream-connect contract` 不只是拨号替换点，也可以作为完整 stream endpoint，让插件实现自己的 Minecraft 协议代理。

### Minecraft Status 示例，未来能力

后续如果实现 `status.ping/v1`，可以提供 `examples/plugins/mc-status-motd`：

```text
examples/plugins/mc-status-motd/
  go.mod
  main.go
  main_test.go
  manifest.json
  README.md
  testdata/
    config.json
    fixtures/
```

示例能力：

- 按 host 返回不同 MOTD。
- 按维护窗口展示维护状态。
- 自定义 favicon。
- 根据 backend health 展示 online/max players。
- 根据 protocol version 返回升级提示。

如果第一版不实现 `status.ping/v1`，该示例可以暂不落地，仅保留为 extension point 设计参考。

示例文档必须包含：

1. 使用与 gateway 相同 Go 版本构建。
2. 生成二进制 `.mcgp`。
3. 生成源码 `.mcgp`。
4. 通过 Admin 上传。
5. 源码包查看构建状态和日志。
6. 点击加载。
7. 填写配置。
8. 启用插件。
9. 查看插件状态和 extension point。
10. 禁用和删除插件。

## 落地计划

阶段实施拆分以 [插件系统阶段实现计划](plugin-implementation-plan.md) 为准。本文保留的是目标设计和能力全集；阶段文档负责定义每个实现阶段的可用边界、任务、验收和回滚策略。

### 从探索代码迁移的结果

并行运行时迁移已经完成，当前约束如下：

1. 保留现有 `api.Plugin`、`api.Gateway`、`RegisterHookHandler` 和 `HookUpstream`，避免已打包插件失效。
2. Plugin Manager 接管插件加载、实例创建、JSON 配置解码、生命周期和 runtime state。
3. `cmd/gateway/plugin.go` 及其全局 `plugins`、`hooks`、`pluginLock` 已删除，连接路径只读取 Plugin Manager 发布的 dispatch snapshot。
4. 旧 `HookUpstream` 由 Plugin Manager 映射到 `legacy upstream-connect contract` 的请求上下文；兼容逻辑不在 gateway 数据面重复实现。
5. `gatewayconfig.Config.Plugin`、`DecodePluginConfig` 和 `[plugin.*]` TOML loader 已删除，不提供兼容期或迁移告警。
6. SQLite/Admin 是上传、构建、加载、启用、禁用、配置、删除和回滚的唯一管理入口，Admin API 改变 desired state 后由 Plugin Manager 收敛。
7. 生产制品统一为 `.mcgp` artifact；正式 runtime adapter 仍使用 `plugin.Open` 加载包内已校验的 `plugin.so`。
8. managed 插件未接管上游时，gateway 直接进入 TCP、QUIC、KCP 或 HAProxy 原生上游路径。

### 阶段 1：运行时模型和数据结构

- 新增 `plugin_artifacts`、`plugins` migration。
- 新增 `plugin_builds` migration。
- 新增 `plugin_operations` migration，用于长操作进度、幂等和取消。
- 新增 `plugin_background_task_runs` migration，用于后台任务最近运行摘要和手动触发审计。
- 新增 `plugin_event_deliveries` migration，用于事件订阅投递状态、drop 和 dead letter 摘要。
- 新增 `plugin_route_decisions_recent` migration，用于动态路由和上游覆盖的最近决策摘要。
- 新增 `plugin_data`、`plugin_secrets` 和配置快照 migration。
- 新增 `plugin_security_advisories` 和 `plugin_advisory_matches` migration。
- 扩展 `audit_logs.metadata_json`，用于插件管理、发布门禁和 route decision 的结构化审计字段。
- 扩展 `plugin_data` schema version、data class、size、expires_at 和配额统计。
- 预留 `plugin_repositories` 和 `plugin_repository_cache` migration。
- 预留多实例 `plugin_node_states` migration。
- 预留 `admin_external_identities` migration，用于未来 Admin SSO/OIDC/LDAP 账号绑定；不用于 Minecraft 玩家身份。
- 预留插件自定义入口服务状态模型，但第一版不实现插件新增 listener。
- 新增 Plugin Manager，负责 desired state 到 runtime state 的同步。
- 定义 runtime status snapshot。
- 定义 artifact/build/plugin desired/policy/runtime/review 分离状态机。
- 定义 runtime adapter 抽象，第一版实现 `go-plugin` adapter。
- 定义 desired_generation、reconcile、幂等操作和事务边界。
- 定义 operation ID、idempotency key、长操作查询和协作取消语义。
- 定义 config snapshot/desired snapshot、snapshot diff、config-only rollback 和 full desired rollback 语义。
- 启动时按 SQLite 中 enabled 插件顺序加载。
- 调整 extension point dispatch 为只读快照和确定性顺序。
- 接入插件运行时 metrics、trace 摘要和审计事件。
- 支持 scope、rollout、dry-run 在 dispatch 前生效。
- 支持性能预算、slow call 记录和告警状态。

### 阶段 2：ABI 和 SDK

- 定义 `.mcgp` 包格式。
- 定义 manifest schema。
- 定义 `.mcgp` 静态校验规则、大小限制和 zip slip 防护。
- 定义插件 ID、handler ID、task ID、secret name 和 extension point 命名规范。
- 支持 `artifact_type=binary/source` 和 `runtime.type=go-plugin`。
- 确认插件只需要导出 `Plugin` factory，包内元数据只来自 canonical `manifest.json`。
- 定义 Go plugin ABI fingerprint schema、计算规则、compat diff 和开发模式 override 语义。
- 在 `plugin/api` 中补齐 `APIVersion`、extension point metadata、`ErrPass` 等。
- 把 upstream hook 收敛为 request struct。
- 定义第一批预留 extension point 类型：middleware、provider、event subscriber。
- 提供 manifest 校验工具和插件测试 harness。
- 提供 connection takeover mode 的测试 helper。
- 提供 benchmark harness 和 regression benchmark profile。
- 定义配置迁移接口和插件私有数据接口。
- 定义 plugin_data schema、迁移、快照、配额、retention 和导入导出规则。
- 定义 `PluginFileStore`、包内 resources、runtime data/cache/tmp/log/diagnostic 目录、配额、retention 和 GC 规则。
- 定义 SecretStore、依赖声明和 runtime limits schema。
- 定义 secret version、rotation strategy、SecretReloader 和 grace period 语义。
- 定义 external dependencies schema、fail policy、dependency health 和 data classes。
- 定义 `ExternalClient` SDK 接口、受控 HTTP/TCP 调用、错误码、指标和 trace 语义。
- 定义 composition constraints、冲突分析和 dispatch plan schema。
- 定义入口传输和上游协议能力 schema：TCP/KCP/QUIC/WebSocket、TCP/Admin 端口复用、TCP/KCP/QUIC/HAProxy upstream。
- 定义 route decision schema、SQLite route snapshot 与 route provider 合成规则。
- 定义 Minecraft protocol capability schema、forwarding mode 和 modded 支持声明。
- 定义 `admin.auth.provider/v1` 的预留契约、账号绑定模型、本地 admin break-glass 和 session 签发边界。
- 定义供应链元数据、provenance 字段和兼容性校验错误模型。
- 定义插件 documentation schema、README/Runbook/data handling 门禁和安全 Markdown 展示规则。
- 定义 license policy、SBOM 摘要、SBOM 解析错误和 override 语义。
- 定义安全公告 schema、受影响范围匹配和漏洞响应策略。
- 定义 Logger、Tracer、HealthCheck、PreflightChecker、SelfTester、BackgroundTask、BackgroundTaskSchedule 和 PluginRuntimeInfo 接口。
- 定义 preflight/self-test profile、检查项、脱敏证据和发布门禁复用规则。
- 定义后台任务 interval/cron/manual schedule、run-on-start、jitter、non-reentrant、manual trigger、lease 和最近运行摘要语义。
- 定义 trace/span 语义、context propagation、采样策略和隐私规则。
- 定义 Admin metrics、Prometheus scrape、OpenTelemetry metrics/traces 和 event sink 的导出边界。
- 定义插件业务事件、自定义指标、事件 schema、脱敏和限流规则。
- 定义 event subscription、delivery mode、队列、retry、overflow、dead letter 和弱一致语义。
- 定义 feature key、required/optional feature 协商和 `ErrFeatureUnavailable`。
- 定义 canonical hash、desired fingerprint 和脱敏 diff 规则。
- 定义 provider 协作规则和 config schema UI hint。
- 定义声明式 Admin UI schema、status panel 和 plugin action API。
- 定义 API 兼容、废弃和降级策略。
- 定义机器可读契约文件、conformance suite、CLI golden 输出和 SDK 发布节奏。

### 阶段 3：源码构建流水线

- 实现 source `.mcgp` 上传后的 build job。
- 实现 local-process builder 作为开发入口。
- 实现 container builder 作为生产推荐入口。
- 记录构建日志、builder 元数据、module list 和构建错误。
- 生成并保存 ABI fingerprint、abi JSON 和 `go version -m` 摘要。
- 记录 supply chain、documentation、license、SBOM、source sha256 和 artifact provenance。
- 构建成功后产出统一 `plugin_artifacts`。
- 实现 artifact/source/build log 保留策略和 GC candidate 计算。

### 阶段 4：Admin API

- 实现 artifact 上传。
- 实现包校验错误返回和 artifact GC API。
- 实现稳定 API 错误码和 generation conflict 处理。
- 实现 operation list/detail/cancel API 和 idempotency key 复用。
- 实现准入策略评估、risk level、review 记录、denylist/revoke 和 quarantine API。
- 实现 admission policy profile、policy snapshot、warning override TTL 和 reason code API。
- 实现 security advisory 导入、rescan、match ack 和 policy evaluation 集成。
- 实现审计日志 `metadata_json` 迁移、插件审计 metadata 写入、查询过滤和 audit sink 结构化 payload。
- 实现 build list、build detail、retry。
- 实现 load、enable、disable、delete。
- 实现 dependencies 检查、secret 管理和 artifact rollback。
- 实现 config snapshot 列表、diff、config-only rollback 和 full desired rollback API。
- 实现 secret version、rotate/revoke、hot reload、reload required 和 rotation audit API。
- 实现 plugin_data 摘要、数据迁移、GC、快照和回滚兼容检查 API。
- 实现 plugin file resource 摘要、运行目录配额、GC candidate、cache/tmp/log/diagnostic 清理和 orphaned runtime dir API。
- 实现 composition/conflict check、dispatch plan 和 shadowed handler 诊断 API。
- 实现发布门禁检查结果 API。
- 实现插件 metrics、trace 查询、审计事件枚举和权限矩阵。
- 实现 exporter 状态、Prometheus/OTel 配置摘要、导出失败和高基数字段拒绝统计 API。
- 实现插件业务事件 recent API、自定义指标摘要 API、drop/error 计数和权限检查。
- 实现动态 route decision recent API、route provider cache 状态和 SQLite fallback 展示。
- 实现 event subscription 投递状态、queue depth、drop/dead letter 摘要、重放和丢弃 API。
- 实现 permission key 鉴权层，先映射到 admin/member/guest，避免 API handler 写死角色。
- 实现 features API、compat feature 检查和 artifact detail feature 协商结果。
- 实现 transport/upstream protocol 能力展示、scope 兼容检查和 HAProxy/真实 IP 转发冲突提示。
- 实现 health check、background task 状态、手动触发/取消、最近运行摘要、插件日志摘要和 scope/rollout 配置 API。
- 实现 preflight/self-test API、operation 记录、发布门禁集成和最近结果摘要。
- 实现 external dependency 状态、手动探测、fail policy 和跨环境 diff API。
- 实现受控 external dependency 调用统计、熔断状态和最近错误 API。
- 实现 Minecraft 协议能力展示、scope/protocol version 兼容检查和 forwarding 提示。
- 实现声明式 Admin UI schema 和 plugin action 执行 API。
- 实现资源限制命中状态和 config schema UI hint 返回。
- 实现性能预算、告警规则和告警静默 API。
- 实现 benchmark result 存储、基线对比和发布门禁 API。
- 实现 promotion bundle export/import、diff、apply 和 drift report API。
- 实现 canonical hash、desired fingerprint、脱敏 diff 和 review hash 校验。
- 实现灾备演练任务和报告 API。
- 预留或实现仓库 list、check、import API；第一版不做自动启用。
- 所有写操作接入审计日志。
- 插件状态进入 `/admin/api/plugins`。

### 阶段 5：Admin 页面

- 新增插件页面。
- 支持上传、加载、启用、禁用、切换版本和删除。
- 展示上传包校验错误和 artifact GC candidate。
- 展示源码包构建状态、日志摘要和错误。
- 展示 ABI 校验错误、加载错误、restart required 原因。
- 展示准入策略、risk level、review 状态、denylist 命中和 quarantine 状态。
- 展示安全公告命中、fixed version、推荐动作、缓解状态和 override 审计。
- 展示 README、Runbook、support contact、data handling 声明和文档门禁结果。
- 展示组合约束、冲突分析、dispatch plan 和遮蔽诊断。
- 展示入口传输和上游协议能力：TCP、KCP、QUIC、WebSocket、TCP/Admin 端口复用和 HAProxy upstream。
- 展示动态路由决策摘要：SQLite route 命中、provider override、fallback、cache stale 和 effective upstream protocol。
- 展示 Minecraft 协议能力、协议版本兼容、forwarding 和 modded 支持边界。
- 展示运行时状态、错误率、超时、panic、熔断、trace 摘要和 active proxy connections。
- 展示插件业务事件、自定义指标、drop 计数和脱敏提示。
- 展示 required/optional features、missing features、compat warnings 和 supported features。
- 展示 desired state、policy state、active artifact、desired artifact、loaded artifact 和 review 状态差异。
- 展示 preflight/self-test 最近结果、阻断项、warning、证据 ID 和重新运行入口。
- 展示 benchmark 结果、容量估算、基线对比和性能回归风险。
- 支持配置版本展示和迁移确认。
- 支持 plugin_data schema、配额、retention、迁移、GC 和回滚风险展示。
- 支持插件 runtime 文件配额、cache/tmp/log/diagnostic 清理和 orphaned runtime dir 提示。
- 支持 secret 状态展示、依赖影响提示、artifact 回滚和配置快照选择。
- 支持 secret version、rotation state、grace period、reload result 和到期提示展示。
- 支持 external dependency、数据传输提示、fail policy 和健康探测展示。
- 支持受控 external dependency 调用统计、熔断状态、inflight 和最近错误展示。
- 支持 scope、rollout、dry-run、health、background task 和日志诊断展示。
- 支持声明式 Admin UI、状态面板和受控 action 渲染。
- 支持 runtime limits 实际生效值、资源限制命中和 config schema 表单 hint 展示。
- 展示性能预算、P95/P99、slow call、告警和静默状态。
- 展示 supply chain、documentation、license、SBOM、signature、repository source 和 update availability。
- 展示 exporter 状态、最近导出错误、metric/event drop 和脱敏策略摘要。
- 展示 promotion bundle 导入导出、secret mapping、环境覆盖、diff 和 drift 状态。
- 展示 canonical hash、desired fingerprint、review 绑定 hash 和脱敏 diff。
- 展示灾备演练报告和缺失 artifact/secret/兼容性问题。
- 展示插件权限矩阵对应的禁用按钮和风险提示。
- 展示发布门禁、威胁提示、数据脱敏提示和 Runbook 入口。

### 阶段 6：示例和文档

- 提供 upstream rewrite 示例插件。
- 提供 mc-auth-proxy connection takeover 示例插件。
- 示例插件支持二进制包和源码包两种 `.mcgp` 打包方式。
- 示例插件纳入 conformance suite，作为 SDK/API 契约回归基线。
- 示例插件提供 benchmark profile，至少覆盖 upstream handler 和 connection takeover smoke。
- 提供插件开发文档。
- 提供能力矩阵、Extension Point 清单、能力边界和常见场景文档。
- 提供源码包构建环境、供应链元数据、权限、审计和排障文档。
- 提供插件 README、Runbook、data handling、support metadata 和安全 Markdown 编写规范。
- 提供多实例部署、资源治理边界、插件间协作和配置 UI schema 文档。
- 提供威胁模型、数据分类、测试矩阵、发布门禁和运维 Runbook。
- 提供跨环境 promotion、导入导出、secret mapping、配置漂移和灾备演练文档。
- 提供 CLI 工具文档：init、validate、package、inspect、compat、build、test。
- 提供 plugin_data inspect/export/gc CLI 文档，明确权限、data_class 和 dry-run 规则。
- CI 或本地脚本验证示例插件能构建，并运行 conformance suite。

### 阶段 7：仓库和供应链增强，未来能力

- 实现 official/internal/file/url 仓库 index 拉取。
- 实现仓库 index schema 解析、sha256 校验、repository conflict 诊断和 trust policy。
- 实现仓库 artifact 下载到本地 `plugin_artifacts`。
- 实现 signature 验证和组织信任根。
- 实现 SBOM 漏洞扫描和许可证策略。
- 实现 CycloneDX 支持、license transitive scan 和组织级 license policy 同步。
- 实现官方/内部 advisory feed 同步和自动匹配。
- 实现 update availability 提示和 staged 更新。
- 保持人工 review 和手动启用，不默认自动切换生产流量。

### 阶段 8：Sandbox Runtime，未来能力

- 定义 gateway 插件服务启动模式配置：`in-process`、`go-plugin-process`、`sandbox-process`。
- Admin 页面支持保存 desired service mode、展示 active mode 和 restart required。
- 定义 `go-plugin-process` supervisor、plugin-host 生命周期、fd passing、drain-only、fd-live 和 fd-live-shm 迁移契约。
- 定义共享内存迁移 state schema、safe point、quiesce/snapshot/restore conformance。
- 定义 `sandbox-process` control RPC protocol。
- 定义跨进程 stream relay 和 `stream.proxy/v1` 或 `legacy upstream-connect contract`。
- 实现 sandbox supervisor、心跳、crash loop 检测和进程级资源限制。
- 将 capabilities 映射到文件系统、网络、环境变量、secret、CPU 和内存强制策略。
- 实现 secret handle/RPC，不通过环境变量注入长期 secret。
- 定义 `wasm` host ABI，先支持 route/rule/config validate 等轻量 extension point。
- 扩展 `.mcgp` 校验，支持 sandbox-process entry 和 WASM module 校验。
- Admin 页面展示 runtime 强制权限状态、supervisor 状态和 sandbox 故障原因。

## 验收标准

- 重启 gateway 后，SQLite 中 enabled 的插件按 priority 自动加载并启用。
- Plugin Manager 使用 desired_generation 收敛；重复 enable/disable、并发更新和重启恢复都是幂等且可解释的。
- runtime instance 有 runtime_instance_id 和 applied_generation；旧 generation 的异步回调不能覆盖新 generation 状态。
- artifact、build、plugin desired/policy、runtime instance 和 review 使用分离状态机；API 能展示 active/desired/loaded artifact 差异。
- 构建、启用、回滚、promotion import/apply、GC 和 DR drill 等长操作有 operation ID、进度、结果、错误码、幂等键和协作取消语义。
- `policy_state=blocked/revoked/quarantined` 会覆盖 desired state，新连接不得进入被阻断插件。
- 上传插件不会执行插件代码。
- 上传 `.mcgp` 会拒绝 zip slip、特殊文件、缺失 manifest、缺失 runtime entry、文件过大和解压后过大的包。
- 上传源码包会创建 build job，构建成功后生成可加载 artifact。
- 构建失败不会影响已有 active artifact。
- 构建日志、builder 版本和产物 sha256 可在管理页追溯。
- artifact/source/build log GC 不会删除 active、desired、snapshot referenced 或已加载 artifact。
- supply chain、documentation、license、SBOM、signature 和 provenance 信息可在 artifact 详情页追溯。
- README、Runbook、support contact 和 data handling 声明可由 manifest 结构化引用，并按安全 Markdown 子集展示。
- 准入策略可以评估 source、documentation、license、SBOM、signature、capabilities、extension points、artifact sha256 和 upgrade risk。
- admission policy 支持 dev/staging/prod profile、policy snapshot、reason code 和 warning override TTL。
- warning override 绑定 artifact/config/scope/risk reason/policy snapshot；过期后不能继续 enable、rollback、repository import apply 或 promotion apply。
- 安全公告可以按 artifact sha256、plugin/version range、SBOM dependency 或 source metadata 匹配本地 artifact，并影响 enable、rollback、repository import apply、promotion apply 和 quarantine。
- SBOM 解析失败、缺失 SBOM、未知 license 和 license policy 命中会进入 policy result，并在 Admin 展示。
- 高风险 artifact/config/scope/runtime limits 组合需要 review 时，审批记录绑定对应 hash；任一输入变化后审批失效。
- review、promotion、drift、rollback 和审计使用统一 canonical hash、desired fingerprint 和脱敏 diff；CLI 与 Admin 结果一致。
- config snapshot 保存 artifact/config/scope/rollout/runtime limits/features/policy hash；回滚快照时重新执行当前策略和发布门禁。
- denylist 命中的 artifact 不能 load、enable、rollback 或通过 promotion import 应用到生产。
- 已启用插件被撤销后，新连接不再进入该插件，connection takeover 连接进入 draining 或 force close，并提示重启彻底移除已加载 native code。
- runtime adapter 明确 `go-plugin`、`sandbox-process`、`wasm` 和 build-time instrumentation 的能力边界。
- 文档明确 `go-plugin-process` 是部分可用的可选服务启动模式：当前已有 plugin-host command/handshake/UDS control、supervisor start/stop foundation、host 内 lifecycle、loaded host crash summary refresh、persisted configurable restart backoff/max/window policy、crash-loop auto-isolation、per-node crash isolation、service-level last error persistence、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket handshake orphan cleanup、Linux `/proc` process-table orphan discovery、`legacy upstream-connect contract` dialer bridge 和 connection takeover drain-only stream bridge；fd/live migration、sandbox enforcement、完整不可信隔离和非 Linux process-table orphan discovery 仍是未来能力。
- Admin 能保存 gateway 插件服务 desired mode，展示 active mode、restart required、plugin-host/supervisor 状态和迁移能力；切换 `in-process`/`go-plugin-process` 不承诺运行中生效。
- `go-plugin-process` 连接迁移分为 `drain-only`、`fd-live` 和 `fd-live-shm`；默认 `drain-only`，live migration 必须由插件显式声明并通过 conformance。
- 文档明确 fd 只迁移内核 socket，共享内存只迁移稳定格式的用户态 buffer/state，不能共享 Go heap 对象。
- external CI binary artifact 的 supply-chain assessment 会校验签名验证、source/artifact sha、run/builder identity 和 trusted 标记；未满足时阻断 enable、rollback、repository import apply 和 promotion apply。
- container source build 的 prod governance 会校验 builder image digest pinning、plugin API version 和 Go version release binding；未绑定时进入 warning/override，而官方 release-pinned builder image 发布本身仍是后续发布链工作。
- build-time instrumentation 被明确为未来官方构建期能力；它产出 gateway binary，不进入 `.mcgp` 热加载 lifecycle，也不能通过 Admin 页面按插件启用/禁用。
- 如果未来启用 build-time instrumentation，构建产物必须记录 instrumentation manifest、builder identity、source sha256、generated diff hash、Go/API 版本，并通过 conformance、benchmark 和 smoke test。
- sandbox-process 设计明确 control RPC、stream relay、supervisor、secret RPC、资源限制和 crash loop 策略。
- WASM 设计明确只适合 route/rule/config validate 等轻量能力，不承诺完整 connection takeover。
- 跨进程 connection takeover 不复用 `legacy upstream-connect contract` 的 `net.Conn` 返回语义，需要新的 extension point 版本或 stream proxy 模型。
- sandbox runtime 如果无法强制 manifest 声明的必需 capability，启用会被阻断而不是降级为只审计。
- 插件 ID、handler ID、task ID、secret name 和 extension point key 有明确命名规范。
- API 兼容、废弃和降级策略明确，旧 artifact 回滚不会绕过 config_version 检查。
- required feature 缺失会阻断 enable；optional feature 缺失会进入 warning，并通过 `PluginRuntimeInfo.Features` 和 compat 输出展示。
- manifest、extension point、错误码和 CLI 输出有机器可读契约或 golden fixture。
- conformance 的 `invalid_config` 和 `missing_secret` fixture 必须来自 manifest `config_schema` 和 required secret 声明，不能无条件标记通过。
- conformance suite 能在 release 前验证 manifest、extension point 语义、Admin API 错误码和 CLI 输出。
- 示例插件是契约测试的一部分；示例插件不能构建或不能通过 compat/conformance 时视为 API 回归。
- 发布前兼容性报告能列出新增、弃用、移除和破坏性变更；破坏性变更没有新 version 或迁移说明时阻断发布。
- 加载兼容插件可以不重启完成。
- 重新启用已加载 native artifact 会创建新实例但复用已加载代码；文档和 Admin 明确展示包级全局状态风险。
- Go 版本、ABI fingerprint 或 API 版本不兼容时，管理页展示明确差异项和错误。
- 管理页展示 runtime 类型和 capabilities；启用插件前管理员能看到插件声明的能力。
- 插件可以声明支持的 ingress transport、upstream protocol、TCP/Admin 端口复用和真实 IP forwarding 方式；启用前会与 scope/route/upstream 做兼容检查。
- transport/upstream protocol scope 覆盖 TCP、KCP、QUIC、WebSocket 和 HAProxy upstream；不兼容时进入 warning 或 blocking。
- SQLite route snapshot 是第一版默认路由真相；动态 route provider/route resolver 必须产生可解释的 route decision，不能静默覆盖 Admin routes。
- route provider 外部依赖、缓存、fallback 和 fail policy 可在 Admin 查看；provider 不能绕过 Admin API 直接修改 SQLite routes。
- 管理页明确区分 native plugin 可执行的应用层限制和无法强制的 CPU/内存/文件系统隔离。
- 管理页展示发布门禁检查结果，高风险变更需要二次确认。
- 管理页展示 dependencies 和 secret 状态；缺失必需依赖或 secret 时启用失败。
- external dependencies 能声明 endpoint、purpose、required、timeout、max_concurrent、retry、circuit breaker、fail policy、secret refs 和 data classes。
- external dependencies 与 capabilities.network.outbound 不一致时会提示或阻断；sandbox runtime 下不能强制 egress 时启用失败。
- required external dependency not_ready 时会按 fail policy 阻断启用、degraded 或要求管理员确认。
- SDK 提供 `ExternalClient`，对声明的 external dependency 执行 endpoint 校验、timeout、并发限制、retry、熔断、trace、metrics 和脱敏。
- 官方插件和示例插件访问 session server、三方认证源、CMDB、告警 API 等外部依赖时使用 `ExternalClient`；文档明确 native `go-plugin` 无法强制阻止绕过。
- 插件间协作只能通过 dependencies、provider、event subscriber 等显式机制完成，不能直接访问其他插件实例。
- mock/mixin/monkey patch 不作为生产插件机制；mock 只用于测试，mixin/source weaving 只能作为受控 build-time instrumentation。
- composition constraints 能声明 exclusive extension point、conflicts_with、before/after、provides/consumes。
- dispatch plan 能解释同一 extension point 下 handler 的最终顺序、scope 重叠、blocking/warning conflict 和 shadowed handler。
- connection takeover scope 重叠、provider 单例冲突和 middleware 排序环会阻断启用，旧 dispatch table 不受影响。
- config schema UI hint 可以驱动 secret ref、sensitive、advanced、restart/reload required 等表单行为。
- 声明式 Admin UI 可以渲染配置布局、状态面板、文档链接和受控 action，但第一版不会执行插件包内 HTML/JavaScript。
- 插件 action 必须鉴权、二次确认 dangerous action、超时、panic recover、脱敏返回并写审计日志。
- 管理页展示插件 runtime state、调用统计、超时、panic 和熔断状态。
- 插件性能预算、slow call、P95/P99 延迟和告警状态可在管理页查看。
- benchmark harness 能输出 micro、integration、protocol smoke、soak 和 regression profile 的摘要结果。
- artifact 能保存 benchmark 摘要和报告路径，管理页能展示基线对比和退化比例。
- 发布门禁能基于 benchmark 结果识别超过 runtime limits、P99 明显退化和 connection takeover 容量不足。
- 发布门禁能执行通用 preflight；插件实现 Preflight/SelfTest 时能复用其结果，并展示脱敏证据和阻断原因。
- scope、rollout 和 dry-run 可以限制插件只影响指定 host、route、source 或百分比流量。
- dry-run 模式不会改变真实连接结果，且 connection takeover 插件不能以接管连接的方式 dry-run。
- 插件 HealthCheck 的 ready/degraded/not_ready 会影响启用状态和管理页告警。
- 后台任务支持 interval/manual schedule、run-on-start、jitter、timeout、non-reentrant、最近运行摘要和 skipped 记录。
- 后台任务在 disable、shutdown 和 artifact switch 时会被取消或等待超时，不会阻塞连接路径。
- 手动触发后台任务需要权限、confirm token、idempotency key、审计日志和脱敏结果摘要。
- 多实例未来模式下后台任务明确区分 per-node、singleton 和 sharded，并需要 lease 防止重复执行。
- 插件日志和诊断包会脱敏 secret 和敏感配置。
- 威胁模型和数据分类明确说明哪些风险第一版缓解、哪些风险需要组织信任或未来 sandbox runtime。
- 插件 metrics 至少包含 handler calls、duration、errors、panic、timeout、active calls、active proxy connections、build duration 和 build failures。
- external dependency metrics 至少包含 requests、duration、inflight、rate limited、circuit state、status 和 errors。
- Admin metrics API、Prometheus/OTel exporter 和诊断包使用同一指标/trace 语义，exporter 失败不影响连接路径。
- exporter 状态、最近导出错误、metric/event drop 和高基数字段拒绝统计可在 Admin 查看。
- 插件可以通过受控 API 上报业务事件和自定义指标；gateway 只做脱敏、限流、摘要展示和转发，不消费这些事件决定 MC 登录结果。
- 自定义事件和指标必须按 manifest 声明 schema，未声明或高基数字段会被拒绝或丢弃并计入 drop 指标。
- event subscriber 异步弱一致，支持 best_effort/at_least_once、队列、重试、drop 计数和 dead letter 摘要；不能用于同步认证或路由决策。
- 本地审计日志是审计真相来源；audit sink 投递失败只影响外部副本，并在 Admin 展示失败和重试状态。
- tracing 能关联 connection、plugin handler、external dependency、backend dial、plugin action、background task 和 health check。
- trace/span 上下文通过 `context.Context` 传递，日志带 trace/connection ID，指标不把 trace ID 作为标签。
- trace 摘要和诊断包会脱敏玩家身份、IP、host、secret、token、session response 和 packet payload。
- member 可以查看插件状态但不能上传、加载、启用、禁用、删除或修改配置；guest 不能查看插件页。
- Admin API 使用稳定 permission key 做鉴权；admin/member/guest 只是默认角色映射。
- `admin.auth.provider/v1` 如果实现，必须保留本地 SQLite admin break-glass 登录；外部身份源不可替代初始 setup，也不能导致所有管理员被锁定。
- Admin 外部登录 session 必须由 gateway 签发；插件只返回外部身份、groups/claims 摘要和脱敏认证证据，不能绕过 CSRF、权限检查和审计。
- Admin auth provider 状态只影响管理页外部登录方式，不影响 Minecraft 连接路径、MC 正版/三方登录插件或 `legacy upstream-connect contract` 调度。
- plugin_data value 导出需要 `plugin.data.export`，member 默认只能查看摘要。
- plugin runtime files 默认只展示摘要、用量、namespace、data class 和 GC candidate；文件内容导出需要单独权限和脱敏流程。
- secret 更新、依赖阻断、熔断、自动禁用和强制关闭连接都有审计日志。
- secret 支持 version、rotation state、dual-read grace period、hot reload/reload required/restart required 和 revoke；connection takeover 长连接默认不被轮换强制改变协议状态。
- 插件配置迁移失败时不会切换 active artifact。
- plugin_data schema 迁移失败时不会切换 active artifact；回滚前会检查旧 artifact 是否支持当前 data schema。
- plugin_data 有 data class、schema version、配额、retention 和 GC 规则；超过配额时写入失败且不会无限增长 SQLite。
- PluginFileStore 有只读 resources、data/cache/tmp/log/diagnostic 工作目录、路径逃逸防护、配额、retention 和 GC 规则。
- artifact 回滚可以切回已存在且校验通过的旧版本。
- 配置快照回滚可以选择 config-only 或 full desired rollback；成功后生成新的 desired generation 和审计日志。
- 备份恢复后会校验 artifact sha256、重建 runtime file resource 摘要；缺失 artifact、必需 data 目录缺失或 secret 无法解密的插件不会自动启用。
- promotion bundle 导出包含 desired state、artifact 引用、config hash、scope、rollout、runtime limits 和 provenance，但当前实现不导出 config 明文、secret 明文、secret 密文、KMS key 或运行时状态。
- promotion bundle 导入会校验 artifact sha256、Go/API/runtime 兼容性、config hash、config schema、secret mapping 和环境覆盖；本地 apply 需要目标侧提供 config。
- 跨环境导入能展示 artifact/config/scope/rollout/runtime limits diff，缺失 secret 会阻断启用。
- drift 检测使用 artifact sha256、canonical config/scope/rollout/runtime limits/features/policy hash 和 desired fingerprint，不依赖 version 字符串。
- 灾备演练能在不接入真实流量的情况下验证 artifact、manifest、config、secret rebind、load dry-run、health check 和 connection takeover smoke test。
- preflight/self-test 结果不包含 secret、token、完整外部 response、完整 packet payload 或玩家隐私原文。
- 插件删除时可以选择保留或删除 `plugin_data`。
- 插件删除时可以选择保留或删除 runtime data 目录；cache/tmp/log/diagnostic 可以按策略清理。
- promotion bundle 默认不导出 plugin_data 和 runtime files；只有 exportable 且 data class 允许迁移的数据可导出。
- 启用插件失败时，旧 dispatch table 不受影响。
- 禁用插件后，新连接不再调用该插件 extension point handler。
- 删除插件后，DB 记录和可删除文件被清理；已加载插件提示重启后彻底清理。
- `legacy upstream-connect contract` 返回的 `net.Conn` 契约明确覆盖初始 handshake 回放、deadline、关闭、初始写入失败和错误 fallback。
- `legacy upstream-connect contract` request contract 明确包含 connection ID、trace ID、source、host、protocol version、route、initial data、scope 和 metadata，并规定隐私/只读约束。
- `legacy upstream-connect contract` 示例插件可以按文档完整跑通。
- connection takeover mode 示例能证明插件可以接管完整 Minecraft 字节流并自行实现登录代理。
- connection takeover 插件可以声明 Minecraft protocol versions、states、auth modes、forwarding、modded 和 packet features。
- 管理页能提示 scope/protocol version 超出插件支持范围、forwarding mode 不匹配和 unsupported protocol policy。
- `mc-auth-proxy` 设计模板覆盖 auth mode、session server、profile cache、forwarding secret、backend 保护、失败策略和 health check。
- `mc-auth-proxy` 示例至少展示正版或三方 Yggdrasil-like 登录、Velocity modern forwarding secret 引用和登录失败响应。
- 文档明确 status ping、MOTD、modded handshake 和 packet observe/filter 的扩展边界；第一版可由 connection takeover 插件自行实现。
- 文档包含按用户功能点反推的覆盖清单，覆盖连接治理、MC 登录和运营、动态路由、外部依赖、后台同步、观测导出、Admin 扩展、自定义入口、sandbox/WASM 和构建期增强。
- 每个功能点都标明推荐实现形态、第一版能力边界和未来 extension point，不能只有抽象 hook 名称。
- 玩家维度策略、风控、白名单、ban、会员和分流被明确归入 connection takeover 插件内部或未来 MC 协议扩展；第一版 core 不维护玩家身份上下文。
- Admin action、后台任务、外部依赖、PluginDataStore、PluginFileStore 和声明式 UI 被明确作为插件实际落地复杂功能所需的支撑能力。
- 自定义入口、跨语言插件、不可信插件和构建期插桩被明确区分为不同未来路线，不能复用第一版 `go-plugin` ABI 做含糊承诺。
- manifest 校验工具能发现缺字段、版本不兼容和 unsupported extension point。
- 插件测试 harness 能覆盖 route.resolve/v1 provider 和 connection takeover mode。
- 测试矩阵覆盖包格式、ABI、生命周期、配置、extension point、入口传输、上游协议、connection takeover、治理、secret、artifact 和 Admin 权限。
- 运维 Runbook 覆盖连接失败、启用失败、构建失败、secret 泄漏怀疑、磁盘占用过高和多实例部分失败。
- CLI 工具至少覆盖 manifest validate、build/package、inspect、compat、test、promotion export/import/diff、drift 和 dr-drill 的设计。
- CLI 工具覆盖 plugin_data inspect/export/gc，且 data gc 支持 dry-run。
- Admin API 错误响应有稳定 code，管理页和 CLI 不依赖错误字符串解析。
- 文档能明确区分第一版能力、预留 extension point 和未来 runtime。
- 并行的 `cmd/gateway/plugin.go` 运行时和旧 config 插件入口已经移除；SQLite/Admin 和 Plugin Manager 是唯一管理与分发路径。
- 单实例是第一版主路径；如果进入多实例模式，文档定义了 artifact 分发、构建协调、节点 runtime state 和 partial rollout failure 的语义。
- 第一版默认同一 plugin ID 只有一个启用实例；未来多实例需要 instance ID、数据隔离和排序规则。
- 如果实现仓库能力，仓库导入只能生成本地 artifact，不能绕过管理员 review 直接启用。
- 仓库 index 有稳定 schema、sha256 校验、trust policy 和 repository conflict 诊断；仓库更新不会自动切换生产流量。
- 所有插件管理写操作都有审计日志。

## 第一版默认策略

以下默认值用于减少第一版实现歧义。它们都应支持配置覆盖，但默认行为要保守、可审计、便于本地开发。

### 包和构建

| 项 | 第一版默认值 |
| --- | --- |
| 生产上传格式 | 只接受 `.mcgp` |
| raw `.so` | 仅开发模式允许，必须显式开启 |
| `.mcgp` 上传包大小 | 64 MiB |
| 解压后总大小 | 256 MiB |
| zip entry 数量 | 2048 |
| `manifest.json` 大小 | 256 KiB |
| 单个非运行文件大小 | 16 MiB |
| Go 版本兼容 | 默认精确匹配 gateway 构建 Go 版本 |
| ABI fingerprint | 生产默认精确匹配；开发模式允许 override 后尝试 `plugin.Open` |
| builder | local-process 用于开发；生产推荐 container builder 或外部 CI |
| 源码构建网络 | 默认使用配置的 `GOPROXY`；生产可要求 vendor 或内网 proxy |
| build-time instrumentation | 第一版不启用；未来只允许官方/组织 CI profile，不允许普通上传包直接织入 gateway |
| 契约文件 | 第一版手写维护 JSON schema/contract；后续可从 Go 类型和 manifest schema 生成并做 diff 校验 |
| SDK 发布节奏 | gateway release 与 plugin SDK release 默认绑定；SDK 使用 SemVer，gateway 记录支持范围 |
| conformance suite | release 前必须运行并产出报告；第一版可先作为 release gate，CI 阻断按模块成熟度逐步打开 |
| CLI 形态 | 第一版作为 gateway 二进制的 `gateway plugin` 子命令；独立 `gateway-plugin` 作为未来分发形态 |
| 插件服务启动模式 | 第一版固定 `in-process`；Admin 可预留 desired mode 配置，`go-plugin-process`/`sandbox-process` 未来生效且切换需要重启 |

### 准入和权限

| 项 | 第一版默认值 |
| --- | --- |
| 准入策略 | dev 为 `permissive`，staging/prod 为 `review_required` |
| 高风险 review | 第一版单 admin 审批；双人审批作为未来能力 |
| policy profile | dev/staging/prod 三档；每次评估记录 policy snapshot hash |
| warning override | 默认需要原因和 7 天内 TTL；critical 风险默认不可 override |
| optional dependencies | 默认由插件 manifest 声明降级策略；管理员只能在启用时选择更保守策略，不能放宽插件声明的安全边界 |
| denylist | 第一版本地配置和 SQLite 记录；组织中心同步作为未来能力 |
| 权限模型 | 使用 permission key，默认映射到 admin/member/guest |
| dangerous action | 需要 confirm token、超时和审计 |
| confirm token | 默认由 gateway 生成一次性 token，绑定 actor、plugin ID、action ID、desired fingerprint 和 5 分钟 TTL |
| 签名 | 第一版记录和展示，不强制校验，除非组织策略开启 |
| README | 高风险插件缺 README 进入 review_required；restricted 可配置为阻断 |
| Runbook | connection takeover 插件缺 Runbook 进入 review_required |
| data handling | 访问 external dependency、导出事件或处理玩家数据时必须声明；缺失进入 review_required |
| license | manifest 使用 SPDX expression；未知为 `NOASSERTION` 并进入 warning |
| SBOM | 第一版优先支持 SPDX JSON；缺失默认 warning，高风险插件可配置为 review_required |
| secret rotation | `api_token` 默认 replace + reload_required；`shared_secret` 默认 dual_read + 10m grace period |
| security advisory | 第一版支持本地 JSON 导入；critical 默认 `block_new_enable`，可配置为 `quarantine` |
| API 废弃策略 | 按 plugin API minor 版本计算兼容窗口；至少保留一个 minor，破坏性移除需要 major 或新 extension point version |
| warning override 模板 | 必填 reason、risk accepted by、expires_at、policy hash、artifact/config/scope hash；审计展示到字段级摘要 |

### 运行时治理

| 项 | 第一版默认值 |
| --- | --- |
| `legacy upstream-connect contract` handler timeout | 3s |
| `route.resolve/v1` 预留 timeout | 500ms |
| event subscriber timeout | 1s，默认 fail open |
| background task timeout | 30s |
| background task schedule | 第一版支持 interval 和 manual；cron 只预留 schema |
| background task jitter | interval 任务默认 10% interval，上限 30s |
| background task reentry | 默认 non-reentrant，上次未完成时 skipped |
| stale generation event/metric | 默认丢弃并计入 stale counter；诊断中保留摘要 |
| Destroy timeout | 默认 5s，超时后移除 dispatch 并标记 cleanup warning |
| max concurrent handler calls | 128 |
| max active connection takeover connections | 1024 |
| 熔断 | panic、timeout、error rate 超阈值进入 degraded；安全类插件可配置 fail closed |
| HealthCheck not_ready | 阻止新启用；已启用插件按策略 degraded 或移出 dispatch |
| ExternalClient | 官方插件和示例插件默认使用；native 插件绕过时只做风险提示 |
| auth external dependency fail policy | 默认 `fail_closed` |
| external dependency retry | 连接路径默认 0 或 1 次；只对幂等请求允许重试 |
| external dependency health | 插件 HealthCheck 负责业务语义，Plugin Manager 负责展示声明、最近受控调用和熔断状态 |
| external dependency 声明 | 插件声明 network outbound 或使用 ExternalClient 时必须声明；未声明外联在 native runtime 下进入风险提示，sandbox 下阻断 |
| external dependency fail policy | 按用途给默认值：auth/entitlement fail closed，route fallback，audit/metrics fail open，repository fail closed |
| trace ID | 第一版生成 gateway 内部随机 trace ID，保留与现有日志关联；后续可兼容 W3C traceparent |
| connection ID | 第一版生成短随机 ID，单进程内唯一；多实例未来加 node ID 前缀 |
| preflight timeout | quick 默认 5s，protocol-smoke 默认 15s，integration 默认 30s |
| preflight enforcement | blocking/error 阻断 enable；warning 需要管理员确认 |
| benchmark profile | 默认分 local-fast、ci-contract、staging-capacity、prod-canary 四档 |
| performance regression | P95/P99 或错误率相对基线退化超过 20% 进入 warning；超过 50% 或超过 runtime limit 阻断高风险发布 |
| connection takeover benchmark | 第一版 synthetic packet 必需；真实客户端回放作为 staging/prod 证据增强 |
| trace sampling | 默认采样错误、慢调用、管理操作和 1% 正常连接 |
| external `traceparent` | 默认不注入第三方依赖请求 |
| Admin metrics API | 第一版实现，作为统一观测模型的主出口 |
| Prometheus exporter | 第一版预留；如果实现，只做同语义 scrape adapter |
| OpenTelemetry exporter | 第一版预留；默认只保留内存 trace 摘要和诊断包 |
| alert silence | 第一版实现 Admin 本地静默窗口，必须有过期时间、范围、原因和审计；外部监控系统静默作为未来集成 |
| plugin ingress service | 第一版不允许插件新增监听端口；未来走 `ingress.service/v1` 和统一服务管理 |
| go-plugin-process migration | future 默认 `drain-only`；`fd-live` 和 `fd-live-shm` 需要插件声明、safe point、state schema 和 conformance |
| plugin event 保留 | 每插件最近 1000 条或 24 小时，先到为准 |
| plugin event 入队 | 队列满默认 drop event，不阻塞连接路径 |
| event subscription delivery | 默认 `best_effort`；审计 sink 可配置 `at_least_once` |
| event subscription queue | 每订阅默认 1000 条，队列满默认 `drop_oldest` |
| event subscription retry | `at_least_once` 默认最多 3 次，指数退避，上限 30s |
| `UpstreamConnectRequest.InitialData` | 第一版传复制切片；语义仍按只读处理，未来可换只读 buffer wrapper |
| `UpstreamConnectRequest.Source` | 第一版保留 `net.Conn` 只读属性访问约束；后续优先收窄为 connection metadata，禁止 handler 读写 |

### 数据和保留

| 项 | 第一版默认值 |
| --- | --- |
| plugin_data 每插件配额 | 16 MiB |
| plugin_data 单 key 大小 | 256 KiB |
| runtime data 每插件配额 | 64 MiB |
| runtime cache 每插件配额 | 256 MiB |
| runtime tmp 每插件配额 | 128 MiB，启动或插件 disable 后可清理 |
| 历史 artifact 保留 | 每插件最近 5 个版本，active/desired/snapshot referenced 永不自动清理 |
| config snapshot 保留 | 每插件最近 20 个或 180 天；引用 active rollback 的快照受保护 |
| source package 保留 | 90 天，或成功构建后仅保留 sha256/provenance |
| 完整 build log 保留 | 30 天，摘要随 build record 保留 |
| plugin log/diagnostic 保留 | 默认 7 天，可配置 |
| audit log 保留 | 默认 180 天，可配置 |
| promotion bundle | 默认 full bundle 可内嵌 artifact；不包含 secret 和默认不包含 plugin_data |
| secret 存储 | 第一版使用本地数据库密文和文件权限保护；外部 KMS/系统密钥环作为后续增强 |
| 隐私脱敏 | 默认保守脱敏；环境策略可按字段选择 hash、redact、truncate 或短期明文诊断窗口，短期明文必须有权限、TTL 和审计 |

### Scope 和发布

| 项 | 第一版默认值 |
| --- | --- |
| scope 维度 | host、route ID、route tag、source CIDR、protocol version、transport、service name、upstream protocol |
| rollout | percentage + source IP sticky；玩家名 sticky 由 connection takeover 插件自行实现或未来扩展 |
| dry-run | 非 connection takeover handler 可支持；connection takeover 默认禁止接管式 dry-run |
| composition conflict | connection takeover/provider 单例冲突强制阻断；其他 warning 可管理员确认 |
| warning override | 需要理由、有效期和审计日志 |
| transport support | 未声明时默认只按 TCP 兼容；启用到 KCP/QUIC/WebSocket scope 需要声明支持 |
| HAProxy upstream | 插件自定义真实 IP forwarding 与 route `haproxy://` 重叠时进入高风险确认或阻断 |
| rollback | 第一版实现 artifact rollback；配置快照 rollback 可作为同一 API 的可选参数 |
| 多实例 | 第一版不允许同一 plugin ID 多个启用实例 |
| 仓库 | 第一版不进入主路径；如果实现，只做发现、下载和本地导入，不自动启用 |
| config schema UI hint | 使用 `x-mc-gateway-*` 前缀；兼容 JSON Schema 子集，第一版 JSON 编辑器兜底 |
| Admin UI 首版 | 支持 config layout、status panels、docs links、action button；自定义 HTML/JS 不进入第一版 |
| 自定义前端 UI | 不进入第一版；未来只允许 iframe sandbox 或独立 origin capability |
| promotion/drift/DR | 作为 CLI/Admin 设计预留；第一版可先实现 export/import/diff dry-run，不要求完整跨环境发布闭环 |
| drift baseline | 默认由最近一次成功 promotion import/apply 建立；也允许管理员手动设定基线 |
| DR drill | 默认允许在当前 gateway 做不启用 dry-run；隔离环境作为生产建议 |
| scope overlap 静态分析 | 第一版至少覆盖 exact host、简单 wildcard、route ID、route tag、source CIDR、protocol version、transport/service/upstream protocol |
| 环境覆盖格式 | 使用字段路径 map，形式接近 `plugin_id -> field_path -> value`；JSON Patch 可作为未来导入格式 |
| forwarding route 交叉校验 | 第一版允许 route/upstream 可选声明 backend type、期望 forwarding mode、直连保护和真实 IP 信任边界；只用于 preflight/warning，core 不实现 MC 登录或 forwarding |
| modded handshake 示例 | 第一版不提供官方示例；交给 connection takeover 插件自行实现，官方只提供 fixture/声明字段 |

### 多实例和未来 runtime

| 项 | 默认决策 |
| --- | --- |
| future instance ID | 默认由系统生成稳定 instance ID；管理员可设置 display name，不直接控制 ID |
| 多实例 artifact 分发 | 优先对象存储或控制面分发到节点本地 content-addressed 目录；共享文件系统只作为简单部署选项 |
| sandbox control RPC | 优先 Unix domain socket + gRPC/Connect；Windows/跨主机再评估 TCP + mTLS |
| go-plugin-process fd passing | 本机 Unix 优先 UDS + `SCM_RIGHTS`；非 Unix 或不支持 fd 的连接类型降级为 drain-only 或 stream relay |
| go-plugin-process shared memory | 只用于 pending buffer 和稳定状态快照；不共享 Go heap 对象，必须有 schema version 和 checksum |
| sandbox stream relay | 本机优先 Unix domain socket；gRPC streaming 只用于低吞吐结构化流或跨语言控制面 |
| sandbox egress 限制 | 优先依赖部署平台/容器/network policy 强制；gateway 只做声明、审计和 ExternalClient 受控出口 |
| WASM runtime | 优先 wazero，先定义 host ABI 和 contract；wasmtime 作为需要原生性能或组件模型时的未来选项 |
| 跨进程协议代理扩展点 | 使用 `stream.proxy/v1` 表达跨进程 stream relay；`legacy upstream-connect contract` 保留给进程内上游连接结果增强 |
| SBOM 漏洞扫描 | 第一版支持本地/导入式/外部 feed 漏洞库按 SBOM dependency rescan 并进入治理；feed scheduler 为显式配置能力，完整自动扫描链后续实现 |
| license policy | 第一版支持本地 allowlist/denylist 配置；组织中心同步作为未来能力 |

## 已收敛决策

以下问题已经在当前设计中收敛，不再作为开放问题保留：

| 问题 | 收敛决策 |
| --- | --- |
| connection takeover dry-run | 第一版禁止接管式 dry-run；只允许非 connection takeover handler dry-run，或未来单独设计轻量 `Evaluate()` |
| 后台任务 cron | 第一版只支持 interval/manual；cron 只保留 schema |
| 多实例部署 | 第一版明确单实例主路径；已实现 gateway node heartbeat、插件维度 node runtime state、partial rollout 展示、local artifact package mirror 和 background task lease；远端分发和 cross-node apply 仍为后续工作 |
| 同一 plugin ID 多实例 | 第一版不允许；未来引入 instance ID、数据隔离和排序规则 |
| 源码包构建位置 | gateway 主进程不直接执行构建；开发可 local-process builder，生产推荐 container builder 或外部 CI |
| MC 登录业务边界 | 正版/三方登录、身份映射、forwarding 和登录后的协议处理都由 connection takeover 插件负责；core 只提供连接交接和治理能力 |
| 游戏侧 `auth.provider/v1` | 第一版只预留，MC 正版/三方登录不依赖它，由 connection takeover 插件完整实现 |
| `status.ping/v1` | 第一版预留；需要完整控制时由 connection takeover 插件处理 status state |
| packet filter | 第一版不开放 play 阶段 filter；只预留 observe/filter 设计 |
| sandbox/WASM runtime | 第一版 runtime adapter 只实现 `go-plugin`；sandbox-process/WASM 保留 manifest/runtime schema 和未来设计 |
| go-plugin-process runtime | 部分可用的可选服务启动模式，当前支持 Go plugin 进程级加载、host lifecycle、persisted configurable restart backoff/max/window policy、crash-loop auto-isolation、per-node crash isolation、service-level last error persistence、supervised/stale control socket cleanup、metadata-backed process orphan sweep、无 metadata 的 stale active control socket handshake orphan cleanup、Linux `/proc` process-table orphan discovery、`legacy upstream-connect contract` dialer bridge 和 connection takeover drain-only stream bridge；fd/live migration、sandbox enforcement、完整不可信隔离和非 Linux process-table orphan discovery 仍是未来能力 |
| 插件新增 listener | 第一版不允许；未来走 `ingress.service/v1` 和统一服务管理 |
| build-time instrumentation | 第一版不启用；未来仅官方/组织 CI profile，可观测不可热加载 |
| 仓库 | 第一版不进入主路径；即使实现也只导入本地 artifact，不自动启用 |
| raw `.so` | 生产不接受；仅开发模式显式开启 |
| 签名 | 第一版记录和展示，不强制校验，除非组织策略开启 |
| SBOM/license | 第一版记录、展示并进入 warning/review；本地/导入式漏洞库可按 SBOM dependency rescan，组织级策略同步作为后续增强 |
| `mc-auth-proxy` 示例范围 | 第一版至少实现正版或三方 Yggdrasil-like 之一，另一个保留配置模板；offline fallback 默认关闭且必须显式启用 |
| Minecraft protocol version 展示 | 第一版只展示数字 protocol version；名称映射作为未来便利功能 |
| unsupported protocol version | 由 connection takeover 插件按 manifest 策略处理，示例默认 kick；core 不代替返回 kick/pass/close |
| forwarding 示例 | 第一版示例优先 Velocity modern forwarding；BungeeCord/custom forwarding 保留模板或文档 |
| session/profile cache | 示例默认内存缓存；需要跨重启时使用 PluginDataStore，并受配额/retention 约束 |
| optional dependency 降级 | 插件 manifest 声明降级策略，管理员只能选择更保守策略 |
| benchmark profile | 默认 local-fast、ci-contract、staging-capacity、prod-canary 四档 |
| 性能回归阈值 | 退化超过 20% 进入 warning，超过 50% 或超过 runtime limit 阻断高风险发布 |
| connection takeover 容量证据 | 第一版 synthetic packet 必需，真实客户端回放作为 staging/prod 增强证据 |
| external dependency 声明 | 声明 outbound 或使用 ExternalClient 时必须声明；native 未声明只提示风险，sandbox 阻断 |
| external dependency fail policy | auth/entitlement 默认 fail closed，route 默认 fallback，audit/metrics 默认 fail open |
| trace/connection ID | 第一版 gateway 自生成随机 ID；未来再兼容 W3C traceparent 和多实例 node 前缀 |
| 自定义前端 UI | 第一版不支持；未来只能走 iframe sandbox 或独立 origin，不允许插件直接注入 Admin DOM |
| future instance ID | 多实例未来默认系统生成 instance ID，管理员只设置 display name |
| 多实例 artifact 分发 | 优先对象存储或控制面分发，节点本地 content-addressed 缓存；共享文件系统只作为简单部署选项 |
| sandbox stream relay | future 默认 Unix domain socket；gRPC streaming 不作为高吞吐 connection takeover 首选 |
| sandbox control RPC | future 默认 Unix domain socket + gRPC/Connect；跨主机才考虑 TCP + mTLS |
| sandbox egress enforcement | future 默认依赖容器/部署平台 network policy，gateway 提供声明、审计和受控 ExternalClient |
| WASM runtime | future 首选 wazero 并先固定 host ABI；wasmtime 作为后续可选 |
| stream proxy 命名 | 跨进程协议代理使用 `stream.proxy/v1`；`legacy upstream-connect contract` 只用于进程内连接结果增强 |
| 环境覆盖格式 | 第一版使用字段路径 map；JSON Patch 作为未来可选导入格式 |
| 隐私脱敏策略 | 默认保守脱敏；环境策略可放宽展示方式，但短期明文诊断必须有权限、TTL 和审计 |
| alert silence | 第一版实现 Admin 本地静默窗口；外部 Prometheus/Alertmanager 等系统静默只作为未来集成 |
| route/upstream backend metadata | 第一版允许结构化声明 backend type、forwarding mode、直连保护和真实 IP 信任边界；只做发布门禁和诊断提示，不让 core 接管 MC 登录或 forwarding |
| SBOM 漏洞扫描 | 第一版支持本地/导入式漏洞库和显式配置的本地/内网/外部 feed scheduler；完整外部服务集成和自动扫描链作为后续能力 |
| license allowlist/denylist | 第一版支持本地策略；组织中心同步作为未来能力 |

## 待确认问题

暂无。后续进入实现阶段时，如果出现具体代码、部署环境或安全策略约束，再按变更评审补充。
