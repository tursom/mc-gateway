# M6：Extension Ecosystem 完成记录

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

按实际使用价值收尾 route、status、rule、event、middleware、provider 等扩展点，让常见需求有示例、fixture、冲突治理和 Admin 操作入口。

## 完成范围

1. 每个启用的 extension point 都有独立可执行 conformance：
   - `route.resolve/v1`：`route.resolve/v1.override/fallback/reject/pass` 和 `route decision independent` fixture。
   - `status.ping/v1`：`status.ping/v1.host` 和 `status ping independent` fixture。
   - `rule.evaluate/v1`：`allow/deny/error_fail_closed/timeout_fail_closed/bad_config_fallback` fixture。
   - `connection.filter/v1`、`handshake.filter/v1`：allow/reject/fail-open/fail-closed 和 handshake rewrite fixture。
   - `event.subscriber/v1`：best-effort、at-least-once、dead-letter、replay、drop、cross-node at-least-once、failure non-blocking fixture。
   - `provider/v1`：singleton、priority、fallback、dependency、scope、disable fixture，实际 provider 类型覆盖 `admin.auth.provider/v1` 但不宣称其管理登录数据面已实现。
2. `examples/plugins/extension-ecosystem` 提供 route/status/rule/event/provider 示例闭环：
   - route external source refresh、cache TTL、SQLite fallback、decision explain、upstream rewrite。
   - status MOTD、favicon、online/max players、version text、maintenance message。
   - rule/connection middleware host/source CIDR allow/deny、simple rate limit。
3. event subscriber 具备生产语义：
   - 死信持久化到 `plugin_subscriber_dead_letters`。
   - replay/drop 通过 `ReplaySubscriberDeadLetters`、`DropSubscriberDeadLetters` 返回结果，并通过 `plugin_operations` 与 Admin audit 写入审计。
   - cross-node at-least-once 策略通过共享 SQLite 死信记录和 `node_id` 证明，节点 B 可 replay 节点 A 写入的 pending dead letter。
   - 事件投递异步执行，subscriber 失败不会阻塞 event emitter 或连接路径。
4. middleware/provider 冲突治理已收口：
   - middleware 按 priority 稳定排序，支持 fail-open/fail-closed。
   - handshake rewrite 会传递给后续 handshake filter。
   - provider registry 记录 singleton、priority、fallback、dependencies、scope metadata，并通过 disable 从 dispatch plan 移除。
5. Admin auth provider 边界保持不变：
   - `admin.auth.provider/v1` 只作为 provider registry/status 预留边界。
   - feature matrix 仍标记为 reserved、`implemented=false`、`data_plane=false`。
   - 外部 provider 不可用时，本地 admin break-glass 登录仍是唯一已实现认证路径。
6. Admin 操作入口完成：
   - Dispatch plan API/panel 支持 `refresh-routes`。
   - subscriber dead-letter replay/drop 返回数量，并记录 audit。

## 代码证据

- 示例与 conformance：`examples/plugins/extension-ecosystem/manifest.yaml`、`examples/plugins/extension-ecosystem/conformance.json`、`examples/plugins/extension-ecosystem/main.go`。
- CLI conformance 执行器：`cmd/gateway/plugin_cli_toolchain.go`。
- Admin Dispatch plan 操作入口：`cmd/gateway/admin_plugin_handlers.go`、`cmd/gateway/admin_frontend/src/views/plugins.ts`。
- extension runtime：`internal/pluginmanager/extensions.go`。
- subscriber persistent dead letter：`internal/pluginmanager/operations.go`、`internal/pluginmanager/repository.go`。
- feature boundary：`internal/pluginmanager/features.go`。
- 测试覆盖：`internal/pluginmanager/manager_test.go`、`cmd/gateway/admin_api_test.go`、`cmd/gateway/plugin_cli_toolchain_test.go`。

## 验收命令

```bash
go test ./internal/pluginmanager ./cmd/gateway
go run ./cmd/gateway plugin conformance examples/plugins/extension-ecosystem
git diff --check
```

## 回滚边界

- route、status、rule、event、middleware、provider 插件都可通过 disable 从 dispatch plan 移除。
- extension ecosystem 失败不影响默认 route fallback，也不影响已稳定的 `upstream.connect/v1` 主路径。
