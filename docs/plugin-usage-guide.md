# 插件系统使用指南

本文面向网关管理员，说明如何上传、构建、启用、禁用、回滚、治理和排障插件。插件作者请看 [插件系统开发指南](plugin-development-guide.md)。

目标设计、阶段计划和剩余路线分别见 [插件系统设计](plugin-system-design.md)、[阶段实现计划](plugin-implementation-plan.md) 和 [Roadmap 实施计划](plugin-roadmap-implementation-plan.md)。

## 能力边界

插件系统以 `.mcgp` 包为交付单位。当前主路径是可信 Go native 插件：

- runtime：`go-plugin`
- binary 入口：`plugin.so`
- Go 符号：`Plugin`
- API 版本：`plugin-api/v1`
- schema 版本：`mc-gateway.plugin/v1`
- 主要插件数据面入口：`upstream.connect/v1`

Go native 插件和网关在同一信任边界内运行。它不是沙箱，不能隔离任意恶意代码。生产环境只应启用可信来源、经过 review 和治理检查的插件。

Go `plugin` 的限制仍然成立：

- 不支持 Windows 作为 Go plugin 运行目标。
- `.so` 不能真正从进程中卸载；禁用只会让新连接不再进入该插件。
- 已加载 artifact 删除后，代码仍可能留在当前进程内，彻底清理需要重启。
- Go 版本、OS、架构、依赖 ABI 和 `plugin/api` 版本必须与网关兼容。

## 快速开始

以下命令假设在仓库根目录执行，并使用本地示例插件生成 binary 包：

```sh
go run ./cmd/gateway plugin test examples/plugins/upstream-rewrite --profile unit,manifest
go run ./cmd/gateway plugin build examples/plugins/upstream-rewrite --type both
go run ./cmd/gateway plugin inspect examples/plugins/upstream-rewrite/dist/upstream-rewrite.mcgp
go run ./cmd/gateway plugin compat examples/plugins/upstream-rewrite/dist/upstream-rewrite.mcgp
```

启动网关并取得 Admin token 后，可通过 CLI 远程管理插件：

```sh
export MC_GATEWAY_ADMIN_URL=http://127.0.0.1:25565/admin/
export MC_GATEWAY_ADMIN_TOKEN=<admin-token>

go run ./cmd/gateway plugin upload examples/plugins/upstream-rewrite/dist/upstream-rewrite.mcgp
go run ./cmd/gateway plugin status upstream-rewrite
go run ./cmd/gateway plugin config validate upstream-rewrite \
  --artifact <artifact-id> \
  --config examples/plugins/upstream-rewrite/testdata/config.json
go run ./cmd/gateway plugin enable upstream-rewrite \
  --artifact <artifact-id> \
  --config examples/plugins/upstream-rewrite/testdata/config.json
go run ./cmd/gateway plugin disable upstream-rewrite
```

`--gateway` 和 `--token` 也可以直接传给每条远程命令。`--gateway` 可以写到 `/admin/` 或 `/admin/api`，CLI 会规范化到 Admin API 前缀。

## 生命周期

插件生命周期分为 artifact 状态、desired 状态和 runtime 状态。

| 层级 | 常见状态 | 含义 |
| --- | --- | --- |
| Artifact | `uploaded`、`validated`、`loadable`、`loaded`、`rejected`、`deleted` | `.mcgp` 包的静态校验、可加载性和存储状态 |
| Desired | `enabled`、`disabled`、`deleted` | 管理员希望插件处于什么状态 |
| Runtime | `not_loaded`、`loaded`、`enabled`、`failed`、`disabled`、`draining` | 当前进程中插件实际执行状态 |

推荐上线流程：

1. 插件作者或 CI 提供已通过 `plugin test/build/validate/compat` 的 binary `.mcgp`。
2. 上传 binary `.mcgp`：`plugin upload <artifact.mcgp>`。
3. 用 `plugin status <plugin-id>` 查到 `artifact.id`。
4. 执行配置 dry-run：`plugin config validate <plugin-id> --artifact <artifact-id> --config <config.json>`。
5. 对生产档位执行治理检查：`plugin preflight <artifact.mcgp> --profile prod --config <config.json>`。
6. 需要 review 时走 `plugin review approve` 或 Admin 页面审批。
7. 启用：`plugin enable <plugin-id> --artifact <artifact-id> --config <config.json>`。
8. 观察 `plugin logs/events/metrics/diagnose` 和 Admin 页面状态。

回退路径：

```sh
go run ./cmd/gateway plugin disable <plugin-id>
go run ./cmd/gateway plugin rollback <plugin-id> --artifact <old-artifact-id>
go run ./cmd/gateway plugin rollback <plugin-id> --snapshot <snapshot-id>
go run ./cmd/gateway plugin delete <plugin-id>
```

`rollback --artifact` 回退 desired artifact；`rollback --snapshot` 回退配置快照。回滚会重新进入当前门禁，不应绕过 governance、advisory 或供应链阻断。

## Source 包和构建

源码包也是 `.mcgp`，区别由包内 `manifest.json.artifact_type` 决定。管理员可以上传 source 包并由 gateway 构建：

```sh
go run ./cmd/gateway plugin upload examples/plugins/upstream-rewrite/dist/upstream-rewrite-source.mcgp --source
```

Admin API 会把 source artifact 记录到 `/plugin-sources`，并通过 `/plugin-builds` 创建构建记录。构建失败不会修改当前 active 插件。生产默认应使用 container builder；local-process builder 适合开发和本地复现。

本地复现 source build：

```sh
go run ./cmd/gateway plugin build --from-source \
  examples/plugins/upstream-rewrite/dist/upstream-rewrite-source.mcgp \
  --out examples/plugins/upstream-rewrite/dist/upstream-rewrite-built.mcgp
```

## 常用 CLI

本地检查命令：

| 命令 | 用途 |
| --- | --- |
| `plugin features` | 输出当前插件能力事实源 |
| `plugin inspect <artifact.mcgp>` | 查看包内 manifest |
| `plugin validate <dir-or-artifact>` | 校验源码目录、manifest 或包 |
| `plugin compat <artifact.mcgp>` | 检查 artifact 与当前 gateway 兼容性 |
| `plugin preflight/self-test/benchmark <target>` | 本地治理、插件自检和性能门禁报告 |
| `plugin sbom generate <target>` | 生成 SBOM 摘要 |
| `plugin sign verify <artifact.mcgp> --signature <sig> --public-key <key>` | 验证签名和信任元数据 |

远程管理命令：

| 命令 | Admin API |
| --- | --- |
| `plugin status [plugin-id]` | `GET /plugins`、`GET /plugins/{id}` |
| `plugin upload <artifact.mcgp>` | `POST /plugin-artifacts` |
| `plugin upload <source.mcgp> --source` | `POST /plugin-sources` |
| `plugin enable <id> --artifact <artifact-id>` | `PUT /plugins/{id}` |
| `plugin disable <id>` | `POST /plugins/{id}/disable` |
| `plugin delete <id>` | `POST /plugins/{id}/delete` |
| `plugin rollback <id> --artifact <artifact-id>` | `POST /plugins/{id}/rollback/artifact` |
| `plugin rollback <id> --snapshot <snapshot-id>` | `POST /plugins/{id}/rollback/config` |
| `plugin config validate <id>` | `POST /plugins/{id}/config/dry-run` |
| `plugin secret check <id>` | `GET /plugins/{id}/secrets` |
| `plugin logs/events/metrics <id>` | `GET /plugins/{id}/operations` |
| `plugin diagnose <id>` | `GET /plugins/{id}/diagnostics` |
| `plugin task list/run/cancel` | `GET /plugins/{id}/operations`、`POST /plugins/{id}/operations/tasks/*` |
| `plugin data/files inspect/gc` | `GET /plugins/{id}/operations`、`GET/POST /plugins/{id}/operations/gc` |
| `plugin gc` | `GET/POST /plugin-gc` |
| `plugin runtime status/mode/apply` | `GET/PUT/POST /plugin-service` |
| `plugin review status/approve/reject/override` | `/plugins/{id}/governance/*` |
| `plugin advisory/vulnerability scan|import|sync|rescan` | `/plugin-advisories`、`/plugin-vulnerabilities` |
| `plugin repo list/import/apply/updates` | `/plugin-repositories/imports` |
| `plugin transfer <artifact-id>` | 下载源网关 artifact 并上传到目标网关 |
| `plugin apply <promotion.json>` | `POST /plugin-promotions` |

远程命令需要管理员 token。写操作通常要求 `admin` 角色；成员角色可以查看状态、artifact、构建、运维摘要等只读信息。

## 配置和 Secret

插件配置必须是 JSON。CLI 支持两种传入方式：

```sh
go run ./cmd/gateway plugin config validate <plugin-id> --artifact <artifact-id> --config config.json
go run ./cmd/gateway plugin config validate <plugin-id> --artifact <artifact-id> --config-json '{"upstream":"127.0.0.1:25566"}'
```

dry-run 会检查 JSON、manifest schema、必需 secret 和配置 diff，并返回脱敏结果。启用时同样传入配置：

```sh
go run ./cmd/gateway plugin enable <plugin-id> --artifact <artifact-id> --config config.json
```

secret 不应写入普通 config、日志、事件字段或诊断包。插件应在 manifest `secrets` 声明引用，并通过 Admin 页面或 Admin API 写入。CLI 当前提供只读检查：

```sh
go run ./cmd/gateway plugin secret check <plugin-id>
```

## 运行排障

先看事实源和状态：

```sh
go run ./cmd/gateway plugin features
go run ./cmd/gateway plugin status
go run ./cmd/gateway plugin status <plugin-id>
go run ./cmd/gateway plugin runtime status
```

再看插件运维摘要：

```sh
go run ./cmd/gateway plugin logs <plugin-id>
go run ./cmd/gateway plugin events <plugin-id>
go run ./cmd/gateway plugin metrics <plugin-id>
go run ./cmd/gateway plugin diagnose <plugin-id>
go run ./cmd/gateway plugin task list <plugin-id>
go run ./cmd/gateway plugin external list <plugin-id>
go run ./cmd/gateway plugin data inspect <plugin-id>
go run ./cmd/gateway plugin files inspect <plugin-id>
```

常见问题：

| 现象 | 排查 |
| --- | --- |
| `manifest.json is required` | `.mcgp` 包内缺少 canonical manifest；要求插件作者用 `plugin build` 重新打包 |
| `Plugin` 符号缺失 | Go 代码未导出 `func Plugin() api.Plugin`，或 `runtime.entry_symbol` 不匹配 |
| 兼容性失败 | 检查 Go 版本、GOOS/GOARCH、`api_version`、SDK module 和 ABI |
| enable 失败但旧插件仍可用 | 这是预期行为；启用失败不能破坏旧 dispatch table |
| disable 后旧连接仍存在 | protocol-proxy 连接会进入 draining，新连接不再进入插件 |
| delete 后内存仍占用 | Go plugin 不能真卸载；重启后彻底释放 |
| 生产启用被阻断 | 查看 `plugin review status`、`plugin advisory scan`、`plugin vulnerability scan` 和 `plugin verify` |

## 生产建议

- 只启用可信插件，并固定 artifact sha256、package sha256、Go 版本和 SDK/API 版本。
- 上传、启用、禁用、回滚、secret 更新和构建操作都走 Admin/API，让审计日志完整。
- source 包生产构建使用 container builder，不使用开发机 local-process 结果直接上线。
- protocol-proxy 插件必须覆盖 malformed packet、timeout、panic、disable/drain、backend unavailable 等失败路径。
- 事件和指标标签保持低基数，不写入玩家名、UUID、token、session response、secret 或 packet payload。
- 配置变更先 dry-run，再启用；回滚前同样重新执行当前治理门禁。
- 删除已加载 Go plugin 后安排重启窗口，避免误以为代码已从进程中卸载。
