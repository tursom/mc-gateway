# M4：Governance 和 Conformance 完成证据

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把治理从模型和局部门禁推进为所有高风险操作的统一 release gate，并让 conformance 从声明文件变成可执行验证。

## 完成状态

1. 可执行 conformance 已落地：
   - `go run ./cmd/gateway plugin conformance <target>` 会读取目录或包内 `conformance.json`，并对 connection takeover、route、status、rule、connection/handshake middleware、event/provider 和 governance gate 场景执行真实 harness 输入。
   - `invalid_config` 和 `missing_secret` 使用 `Manager.DryRunConfig` 走运行时 dry-run，而不是只看 manifest 静态字段。
   - conformance CLI 使用固定测试时钟，`extension-ecosystem` M4 fixture 连续运行可得到字节一致 JSON，适合 CI golden 比对。
2. missing fixture 策略已收紧但保留兼容窗口：
   - 默认不要求历史包携带 `conformance.json`。
   - `--require-conformance-fixture`、`Options.RequireConformanceFixture` 和 prod/strict 策略可把缺失 fixture 升级为 `conformance_fixture_missing` blocking。
   - 官方示例 `examples/plugins/upstream-rewrite`、`examples/plugins/mc-auth-proxy`、`examples/plugins/extension-ecosystem` 均携带 conformance fixture。
3. 统一 governance 评估路径已接入高风险操作：
   - enable、rollback、repository import apply、promotion apply 和 promotion DR drill 均通过 `EvaluateReleaseGate` 执行同一套 preflight、advisory、supply-chain、benchmark、conflict、review 和 warning override 语义。
   - repository import apply 只写入本地/共享 desired state，不自动启用生产流量；跨节点证据通过 plugin node runtime state 和 rollout summary 表达 artifact distribution、cross-node apply 和 partial failure。
   - instrumentation metadata 不是运行时插件，`available` 状态必须带 policy hash、governance、gateway binary、CI artifact、conformance、benchmark 和 smoke 证据。
4. 高风险操作证据已补齐：
   - review fingerprint 绑定 artifact、config、scope、rollout、runtime limits、features 和 policy hash；任一关键输入变化会重新触发 review gate。
   - warning override 绑定 policy hash 和 TTL；过期后重新阻断，并保留 override、benchmark 和 enable gate 操作审计。
   - benchmark 阈值超限会产生 warning/blocking；warning 只有活动 override 可放行。
5. advisory/quarantine 验收已补齐：
   - revoke/quarantine 命中后移除 upstream dispatch 和 extension dispatch。
   - route cache 会清空，插件后台任务会停止，runtime state 标记为 `draining`。
   - rollback 到受影响 artifact 会被 governance gate 阻断，且 desired/active 状态保持不变。

## 验收命令

- `go run ./cmd/gateway plugin conformance examples/plugins/extension-ecosystem`
- `cmp -s /tmp/mc-gateway-m4-conformance-a.json /tmp/mc-gateway-m4-conformance-b.json`
- `go test ./internal/pluginmanager ./cmd/gateway ./plugin/api`
- `git diff --check`

## 回滚边界

- conformance 命令仍作为 CI/开发工具，不改变默认运行路径。
- 新 blocking 项均通过 preflight/governance issue 暴露 code、severity、message 和 details，避免无解释地阻断已有操作。
