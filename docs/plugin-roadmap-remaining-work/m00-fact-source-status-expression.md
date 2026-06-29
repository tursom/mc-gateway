# M0：事实源和状态表达未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

让 `plugin features`、Admin、CLI、API 和文档表达同一个事实，尤其不能把 `partial`、`reserved`、`stub` 能力展示成生产可用数据面。

## 已完成闭环

1. 固化 `plugin features` 作为机器可读事实源：
   - `pluginFeatureFacts()` 是 CLI JSON 和 `GET /admin/api/plugin-features` 的共享出口。
   - runtime、service mode、extension point、runtime adapter 和 schema export 均从 `pluginmanager` feature matrix 派生。
   - `implemented` 保持兼容；future 能力同时带 `maturity`、`data_plane`、`requires_restart` 和 `unsupported_reason`。
2. 增加表达漂移测试：
   - `TestPluginFeaturesAndManifestCommands` 覆盖 `go-plugin-process` partial process data-plane，以及 sandbox/WASM/ingress/admin auth provider 的 reserved/non-data-plane 状态。
   - `TestAdminPluginFeaturesExposeSharedFactSource` 覆盖 Admin API 与 CLI 共享事实源。
   - `TestPluginSchemaContractAndConformanceCommands` 覆盖 schema export 的 runtime/extension/API response schema 派生。
3. 把阶段退出清单接入文档和 release 检查：
   - Roadmap 入口文档已列出每阶段必须运行的命令、fixture 和失败路径。
   - `.github/workflows/build.yml` 增加 `plugin-m0-release-gate`，在 PR/release 路径运行 M0 fact/schema/UI gate。
   - reserved/stub 能力可从 CLI JSON、Admin API 和 UI runtime panel 文案确认。
4. 为 Admin runtime service panel 补回归断言：
   - runtime service panel 分开展示 desired、active、effective data-plane、adapter、crash policy 和 restart/pending。
   - `TestAdminPluginRuntimePanelStaticContract` 约束 pending/alert 文案、reserved extension point 文案和 data-plane 文案。

## Release 检查

```bash
git diff --check
npm run check:admin
go test ./cmd/gateway ./internal/pluginmanager
go run ./cmd/gateway plugin features
go run ./cmd/gateway plugin schema export --section admin-api
```

重点确认：

- `go-plugin-process` 输出 `implemented=true`、`maturity=partial`、`data_plane=true`，并带 drain-only/unsupported reason。
- `sandbox-process`、WASM 和 `ingress.service/v1` 输出 reserved/non-data-plane。
- `admin.auth.provider/v1` 输出 reserved/non-data-plane，并说明本地 break-glass 仍是已实现管理登录路径。
- Admin UI 文案包含 desired/active/effective data-plane、adapter、crash policy、restart/pending 和 reserved extension point 表达。

## 回滚边界

- 保留旧 `implemented` 字段，出现兼容问题时可以隐藏新增 UI 字段，但不能改变真实插件数据面。
