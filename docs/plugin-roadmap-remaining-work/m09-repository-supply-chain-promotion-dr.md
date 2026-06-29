# M9：Repository、Supply Chain、Promotion 和 DR 完成记录

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

M9 补齐插件 repository 分发、供应链准入、多环境 promotion、多实例分发事实和灾备演练闭环。实现边界保持在插件管理控制面，不把 repository import、promotion apply 或 DR drill 直接接入生产流量。

## 完成范围

1. Repository sync/import/apply
   - 支持 `official`、`internal`、`file`、`url` repository type 的 index sync。
   - index sync 写入缓存；源不可用但有可用缓存时降级为 cached index，并记录 degraded 审计；源和缓存都不可用时记录 failed 审计。
   - update availability 只返回候选和更新原因，不修改 desired state、active state 或运行态。
   - import 只把候选包 materialize 为本地 artifact，保存 repository import 记录和 governance admission preview；不会创建插件 desired state。
   - import apply 先执行 artifact/hash 校验、config dry-run 和 governance gate；dry-run 或失败都不修改 desired/active state；即使请求 `enabled` 也会被 `repository_import_auto_enable` 阻断。

2. Signature/trust roots/advisory denylist
   - 支持 Ed25519 signature verify、trust root rotation、trust root revoke。
   - trust root distribution、policy hash 和 revoke 都写入操作审计。
   - revoked key 不能用同一 `root_id/key_id` 重新 rotate。
   - denylist/advisory/revoke 命中后阻断 rollback、repository import apply 和 promotion apply；阻断时不修改 desired/active state。

3. SBOM/license/CVE chain
   - 上传包内 SBOM JSON 会解析依赖并合并到 manifest supply chain，用于 vulnerability scan。
   - license policy 支持 allowlist、denylist、unknown handling 和 review-required warning。
   - 支持本地 advisory feed、导入式 vulnerability DB、外部 advisory/vulnerability feed sync 和 targeted rescan。
   - external feed scheduler 为 opt-in；启用后会把 external feed sync 接到 advisory/vulnerability rescan 链并记录 matches、blocking、warnings 和 quarantine runs。

4. Repository apply cross-node
   - desired state 写入 shared plugin repository 后记录 `cluster_desired_apply`，并暴露 artifact distribution、node ready/failed、partial failure 和 retry policy 事实。
   - `PluginRolloutStatus` 暴露 artifact distribution mode/status、node runtime states、partial rollout failure 和 stale node 信息。
   - 目标侧收敛仍会重新执行 dry-run、governance 和 runtime gate；单节点失败不会覆盖其他节点状态。

5. Promotion
   - promotion bundle 支持 environment override 和 required secret ref mapping。
   - target apply 只写 desired state，默认 `disabled`，不会自动启用 active traffic；bundle 请求 `enabled` 会被 `auto_enable_disabled` 阻断。
   - drift report 对 desired vs actual 的 artifact、package、runtime、API、config 和 governance fingerprints 输出原因。
   - unsupported target runtime 或 unsupported policy profile 会阻断 apply，并且不写入 desired state。
   - promotion apply 和 DR drill 对 governance blocking issue 以及 warning issue 都按阻断处理；warning override 不会让 promotion 路径推进 desired generation。

6. DR/build-time instrumentation
   - DR drill 校验 bundle、artifact、config hash、target runtime support、dry-run 和 governance；不会创建或修改 desired/active state，也不会接 production traffic。
   - instrumentation 是 build-time metadata，不作为 runtime plugin 出现在插件列表。
   - `available` instrumentation metadata 必须包含 generated diff hash、gateway binary digest、CI artifact digest、passing governance、conformance、benchmark 和 smoke evidence；缺失或 failed evidence 会拒绝 available 状态。

## 验收证据

- `internal/pluginmanager/future_test.go`
  - repository sync/import/apply/update availability：`TestRepositoryUpdateAvailabilityReportsNewerVersionWithoutChangingDesiredState`、`TestRepositoryImportAdmissionEvaluatesGovernanceWithoutDesiredState`、`TestRepositoryURLImportCreatesLocalArtifactWithoutEnable`。
  - promotion/DR：`TestPromotionApplyRequiresConfigHashAndUpdatesDesiredOnly`、`TestPromotionApplyWarningGovernanceDoesNotAdvanceDesired`、`TestPromotionApplyUnsupportedPolicyProfileDoesNotCreateDesired`、`TestPromotionApplyUnsupportedRuntimeDoesNotCreateDesired`、`TestPromotionDRDrillChecksTargetArtifactConfigAndDoesNotApply`。
  - trust roots/signature/advisory/supply chain：`TestTrustRootRotationVerifyRevokeAuditsLifecycle`、`TestSupplyChainAssessmentBlocksGovernance`、`TestExternalCIProvenanceBlocksRepositoryAndPromotionApply`、`TestAdvisoryFeedSyncRescansAndQuarantines`、`TestSBOMParseFeedsVulnerabilityScan`、`TestVulnerabilityDatabaseImportScansAndBlocksGovernance`。
  - instrumentation：`TestInstrumentationIsNotRuntimePlugin`、`TestInstrumentationAvailableRequiresReleaseEvidence`。
- `internal/pluginmanager/governance_test.go`
  - advisory revoke blocks rollback and quarantine removes dispatch.
- `internal/pluginmanager/manager_test.go`
  - external vulnerability feed sync/scheduler and rollback failure no desired-state mutation.
- CLI/Admin evidence remains through existing `cmd/gateway` plugin remote/runtime feature handlers and tests; M9 does not add a new production traffic path.

## 回滚和安全边界

- repository sync/import failure does not delete or mutate existing local artifacts.
- repository update availability is read/report-only for desired and active plugin state.
- repository import apply, promotion apply and DR drill failure leave desired and active state unchanged.
- DR drill never enables production traffic.
- M9 does not claim full binary distribution transport beyond local content store plus CLI/Admin cross-gateway transfer facts already exposed by the promotion path.
