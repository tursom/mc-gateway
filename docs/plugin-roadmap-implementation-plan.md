# 插件系统 Roadmap 实施计划

本文是插件系统剩余工作的入口文档。阶段细节已按里程碑拆到 [plugin-roadmap-remaining-work](plugin-roadmap-remaining-work) 目录，每个阶段只保留还需要闭环的工作、验收标准和回滚边界。

原始判断来自 [plugin-roadmap-priority-audit.md](plugin-roadmap-priority-audit.md) 和 [plugin-implementation-plan.md](plugin-implementation-plan.md)：先压实现有 `in-process go-plugin` 主路径，再补 source builder、governance、operations 和 extension ecosystem，最后推进 future runtime、仓库分发、供应链、promotion、多实例和 DR。

## 拆分原则

- 阶段文件只列未完成或仍需验收闭环的工作，已经落地的内容只作为边界条件保留。
- 每个阶段都必须保持 gateway 可启动、可回滚、默认路由可用。
- 每个功能进入“完成”状态前，必须有 Admin/API/CLI 一致的状态表达。
- reserved、stub、模型预留能力不能在 UI/CLI 中被表达成生产可用。
- 每次提交尽量围绕一个阶段或子阶段，不把 UI 文案、runtime 架构和治理策略混在同一个大改里。

## 阶段文件

| 里程碑 | 文件 | 剩余工作重点 |
| --- | --- | --- |
| M0 | [事实源和状态表达](plugin-roadmap-remaining-work/m00-fact-source-status-expression.md) | 固化 `plugin features` 事实源、防止 UI/CLI/API 表达漂移 |
| M1 | [当前数据面验收](plugin-roadmap-remaining-work/m01-current-data-plane-validation.md) | 补真实 MC 异常路径、跨版本 smoke、dispatch/drain 验收 |
| M2 | [Admin、配置、Secret 和回滚](plugin-roadmap-remaining-work/m02-admin-config-secret-rollback.md) | 补 UI 自动化、dry-run、secret、rollback 和权限失败路径 |
| M3 | [Source Build 生产化](plugin-roadmap-remaining-work/m03-source-build-production.md) | 发布级 builder image、external CI 产物链、GC 和跨环境验收 |
| M4 | [Governance 和 Conformance](plugin-roadmap-remaining-work/m04-governance-conformance.md) | 可执行 conformance、strict fixture 策略、跨节点 repository apply 门禁 |
| M5 | [Operations 生产验收](plugin-roadmap-remaining-work/m05-operations-production.md) | 外部 exporter、长期保留、多实例运维和 ExternalClient 验收 |
| M6 | [Extension Ecosystem 收尾](plugin-roadmap-remaining-work/m06-extension-ecosystem.md) | 每个 extension point 的示例、conformance、冲突和跨节点语义 |
| M7 | [`go-plugin-process` Drain-only](plugin-roadmap-remaining-work/m07-go-plugin-process-drain-only.md) | 跨节点 crash policy、跨平台 orphan discovery、进程态回归验收 |
| M8 | [Stream、Sandbox、WASM 和 Ingress](plugin-roadmap-remaining-work/m08-stream-sandbox-wasm-ingress.md) | 真正隔离 runtime、WASM ABI、stream 协议和 ingress listener 生命周期 |
| M9 | [Repository、Supply Chain、Promotion 和 DR](plugin-roadmap-remaining-work/m09-repository-supply-chain-promotion-dr.md) | 自动分发、签名/SBOM/CVE 链、promotion、多实例和灾备闭环 |

## 全局验收命令

每个阶段至少运行：

```bash
git diff --check
go test ./internal/pluginmanager ./cmd/gateway ./plugin/api
go run ./cmd/gateway plugin features
```

涉及示例插件时增加：

```bash
go run ./cmd/gateway plugin test examples/plugins/upstream-rewrite --profile manifest
go run ./cmd/gateway plugin build examples/plugins/upstream-rewrite --type both
go run ./cmd/gateway plugin test examples/plugins/mc-auth-proxy --profile manifest
```

涉及 future runtime、container builder、跨节点或 Admin UI 时，应在对应阶段文件中补充专门 smoke test。

## 阶段退出清单

| 里程碑 | 必跑命令 | Fixture / 证据 | 必验失败路径 |
| --- | --- | --- | --- |
| M0 | `go run ./cmd/gateway plugin features`; `go run ./cmd/gateway plugin schema export --section admin-api`; `go test ./cmd/gateway -run 'TestPluginFeaturesAndManifestCommands|TestAdminPluginFeaturesExposeSharedFactSource|TestAdminPluginRuntimePanelStaticContract'` | CLI JSON、`GET /admin/api/plugin-features`、Admin runtime panel 文案 | `go-plugin-process` 只能是 `partial`；`sandbox-process`、WASM、`ingress.service/v1`、`admin.auth.provider/v1` 不能显示为 active data-plane |
| M1 | `go test ./internal/pluginmanager ./cmd/gateway ./protocol/smoke`; `go run ./cmd/gateway plugin test examples/plugins/upstream-rewrite --profile manifest`; `go run ./cmd/gateway plugin test examples/plugins/mc-auth-proxy --profile manifest` | upstream-rewrite、mc-auth-proxy、protocol/smoke MC packet fixtures | malformed packet、timeout、panic、disable/drain、restart recovery 不破坏默认 route |
| M2 | `go test ./cmd/gateway ./internal/pluginmanager`; `npm run check:admin` | Admin config dry-run、secret、rollback、permission fixtures | invalid JSON、schema mismatch、missing secret、reload failure、rollback gate failure 不改变 active state |
| M3 | `go test ./internal/pluginmanager ./cmd/gateway`; `go run ./cmd/gateway plugin build examples/plugins/upstream-rewrite --type both` | source package、container builder provenance、external CI metadata fixtures | local-process prod gate、hash/provenance/signature 缺失、build cancel/retry、GC protected references |
| M4 | `go run ./cmd/gateway plugin conformance <fixture>`; `go test ./internal/pluginmanager ./cmd/gateway ./plugin/api` | packaged `conformance.json`、strict missing-fixture policy、governance gate scenarios | fixture failure blocks preflight/governance；missing fixture strict gate blocking；high-risk protocol-proxy without review blocking |
| M5 | `go test ./internal/pluginmanager ./cmd/gateway`; `go run ./cmd/gateway plugin features` | operations fact block、diagnostic package、GC dry-run/apply, background task fixtures | exporter failure, queue full, external dependency timeout, GC apply failure do not affect connection path |
| M6 | `go test ./internal/pluginmanager ./cmd/gateway`; `GOWORK=off go test .` in `examples/plugins/extension-ecosystem` | route/status/rule/event/provider example and conformance fixtures | extension plugin disable restores defaults; subscriber failure does not block connection path; provider conflict is explainable |
| M7 | `go test ./internal/pluginmanager ./cmd/gateway`; process runtime focused tests | plugin-host handshake, supervisor start-stop, process bridge, drain-only fixtures | host crash does not kill gateway/Admin; disable drains old host; cross-node crash state remains partial/not sandbox |
| M8 | `go test ./internal/pluginmanager ./cmd/gateway`; future runtime focused smoke tests | sandbox/WASM/ingress required-capability and schema fixtures | reserved/schema-only capability cannot enable data-plane; WASM trap/timeout isolated; ingress listener absent blocks enable |
| M9 | `go test ./internal/pluginmanager ./cmd/gateway`; promotion/repository CLI dry-runs | repository import/apply, supply-chain, promotion, DR drill fixtures | import/apply does not auto-enable production traffic; target unsupported runtime blocks desired write; advisory/denylist blocks rollback/apply |

## 完成定义

一个阶段只有同时满足以下条件才算完成：

1. 阶段文件中的未完成任务有代码、测试或可重复操作落地。
2. Admin/API/CLI 的状态表达一致。
3. 默认 `in-process go-plugin` 路径没有回归。
4. 失败路径有测试或可重复验证。
5. `git diff --check` 和相关 Go 测试通过。
6. 文档更新了使用、回滚和排障说明。
