# 阶段 1：Managed Binary Plugin MVP

## 目标

交付最小可用的受管理插件系统：管理员可以上传二进制 `.mcgp`，gateway 能校验、登记、加载、启用、禁用和删除可信 Go plugin。第一阶段只要求 `upstream.connect/v1` 的 dialer mode 可用，用于替换上游拨号或实现简单 upstream rewrite。

本阶段完成后，插件系统已经从探索代码进入 SQLite/Admin 管理路径，但不承诺源码包构建、完整 protocol-proxy、复杂治理和 Admin 完整页面。

## 可用性检查点

阶段结束时必须能做到：

- gateway 无插件时行为不变。
- 管理员上传一个二进制 `.mcgp` 后，可以通过 Admin API 或 CLI inspect artifact。
- 管理员可以加载并启用 `upstream-rewrite` 示例插件。
- 命中插件 scope 的连接走插件返回的 upstream conn；不命中时走原默认 upstream。
- 禁用插件后，新连接不再调用该插件。
- 插件启用失败或 handler panic 不破坏旧 dispatch table。
- 重启后，SQLite 中 enabled 的插件按 priority 恢复。

## 范围

### 包和 artifact

- 支持 `.mcgp` zip 上传。
- `artifact_type=binary`。
- `runtime.type=go-plugin`。
- 包内必须包含 `manifest.json` 和 `plugin.so`。
- 上传阶段只解析 zip 和 manifest，不执行插件代码。
- 记录 artifact sha256、plugin ID、version、Go version、GOOS/GOARCH、API version、extension points 和 capabilities 摘要。

### 数据模型

实现最小表：

- `plugin_artifacts`
- `plugins`
- `plugin_operations`
- `plugin_config_snapshots`
- `audit_logs.metadata_json` 扩展或等价结构化审计字段

字段必须能表达：

- artifact 状态：uploaded、validated、loadable、loaded、rejected、deleted。
- plugin desired state：enabled、disabled、deleted。
- runtime state：not_loaded、loaded、enabled、failed、disabled。
- desired generation 和 applied generation。
- active artifact、desired artifact、loaded artifact 的差异。

### Runtime 和 dispatch

- 新增 Plugin Manager。
- 保留现有 `api.Plugin` 和 `Gateway.Hook` 兼容层。
- 将现有 `HookUpstream` 收敛为 `upstream.connect/v1` 注册路径。
- dispatch table 使用只读快照，更新时整体替换。
- handler 排序规则：priority 升序，priority 相同按 plugin ID。
- handler 返回 `ErrPass` 时继续后续 handler；返回 `net.Conn` 时停止；返回普通 error 时本次连接失败。
- handler 调用必须有 panic recover、timeout 和错误计数。

### Admin API / CLI

最小接口：

- 上传 artifact。
- 查看 artifact。
- 创建或更新 plugin desired state。
- load。
- enable。
- disable。
- delete。
- 查看 plugin runtime state。
- 查看 dispatch plan 摘要。

CLI 可以先作为开发工具，覆盖：

- `plugin inspect`
- `plugin validate`
- `plugin compat`

### 示例

提供 `examples/plugins/upstream-rewrite`：

- 读取 `match_host` 和 `upstream` 配置。
- 注册 `upstream.connect/v1`。
- 命中时 `net.Dial` 到 upstream 并返回连接。
- 不命中时返回 pass。

## 明确不做

- 不支持 source `.mcgp` 构建。
- 不支持 protocol-proxy mode。
- 不支持 SecretStore。
- 不支持完整 Admin 页面。
- 不支持准入 review、SBOM、license 策略和仓库。
- 不支持真正热卸载。
- 不支持 sandbox、WASM 或 `go-plugin-process`。

## 实现任务

1. 增加 `.mcgp` 静态校验：zip slip、大小、manifest、runtime entry。
2. 增加 manifest schema v1 的最小字段校验。
3. 增加 Plugin Manager 和 runtime adapter 抽象，只实现 `go-plugin`。
4. 增加 SQLite migration。
5. 增加 desired state reconcile。
6. 把连接路径接入 dispatch table snapshot。
7. 实现 `upstream.connect/v1` dialer mode contract。
8. 实现 load/enable/disable/delete API。
9. 增加基础审计事件。
10. 增加 upstream-rewrite 示例插件。

## 验收

- `upstream-rewrite` 能通过 `.mcgp` 上传、load、enable。
- 启用后指定 host 连接到新 upstream。
- disable 后新连接恢复默认 upstream。
- plugin.Open 失败返回稳定错误，不影响其他插件和默认连接路径。
- gateway 重启后 enabled 插件恢复。
- `git diff --check` 和现有测试通过。

## 回滚策略

- 删除或 disable 插件即可恢复默认 upstream。
- 如果 artifact 已加载，delete 后标记 pending cleanup，提示重启彻底清理。
- 如果 Plugin Manager 初始化失败，gateway 应可在禁用插件系统配置下启动，并保留原有路由能力。
