# M3：Source Build 生产化完成记录

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把 source `.mcgp` 从开发能力推进到生产可控能力，重点是发布级 builder image、external CI 产物信任链和跨环境验收。

## 完成范围与证据

1. 发布官方 release-pinned builder image：
   - builder image 必须绑定 gateway release、plugin API version、Go version、GOOS 和 GOARCH。
   - release 文档必须要求 digest-pinned 引用。
   - 浮动 tag 或 release 绑定缺失只能进入 warning/override 路径。
   - 官方 builder image 命名约定为 `ghcr.io/tursom/mc-gateway-plugin-builder:release-<gateway-release>-<plugin-api>-go<go-version>-<goos>-<goarch>@sha256:<digest>`；发布说明不得只给 floating tag，生产配置必须复制 digest-pinned 引用。
   - `.github/workflows/plugin-builder-image.yml` 是官方发布入口；每个 GOOS/GOARCH 生成单独 release-bound tag，并把 digest-pinned 引用上传为 release artifact。
   - 证据：`.github/workflows/plugin-builder-image.yml` 解析 gateway release、plugin API、Go version 和平台矩阵，输出 `@${{ steps.build.outputs.digest }}` 引用；`Dockerfile.plugin-builder` 写入同一组 OCI labels；`TestPluginBuilderImageWorkflowReleaseContract` 防止 workflow 退化为 floating tag 或漏掉 digest artifact。
2. 完整 external CI artifact 发布链：
   - 签名、attestation、SBOM、source sha、artifact sha、CI run identity、builder identity 和 release provenance 进入官方发布流程。
   - hash 不匹配或 provenance 不完整必须阻断 enable、rollback、repository apply 和 promotion apply。
   - external CI binary `.mcgp` 的 `provenance.json` 必须包含 `signature`、`sbom` 和 `external_ci`，其中 `external_ci` 至少包含 `source_sha256`、`artifact_sha256`、`run_id`、`builder_id`、`attestation`、`sbom` 和 `release_provenance`。
   - 证据：`ArtifactStore` 合并 `provenance.json` 的 `signature`、`sbom`、`external_ci`；`externalCITrustIssues` 要求顶层签名/SBOM 和 external CI release chain；`TestSupplyChainAssessmentAllowsTrustedExternalCIArtifact`、`TestExternalCIProvenanceRequiresTopLevelSignatureAndSBOM`、`TestExternalCIProvenanceMetadataBlocksEnableWithoutRequiredFlag`、`TestExternalCIProvenanceBlocksRepositoryAndPromotionApply` 和 rollback hash mismatch 测试覆盖阻断路径。
3. 补跨环境 source build 验收：
   - dev local-process、prod container、external CI 三条路径分别有 smoke test。
   - 生产 profile 下 local-process source build 在构建和启用门禁均被阻断。
   - 证据：`TestManagerBuildSourceCreatesBinaryArtifact` 覆盖 dev local-process；`TestManagerProdDefaultsSourceBuildToContainer`、`TestManagerProdRejectsExplicitLocalProcessBuild`、`TestManagerRunBuildRechecksProdLocalProcessPolicy` 和 `TestGovernanceProdBlocksLocalProcessSourceBuildProvenance` 覆盖 prod gate；`TestContainerBuilderBuildsUpstreamRewriteSourcePackageSmoke` 是 opt-in Docker smoke；external CI trusted/blocked 测试覆盖 CI artifact 路径。
4. 补构建日志和环境边界验收：
   - build log 不包含 secret、token、credential 或完整私有路径。
   - GOPRIVATE/GONOSUMDB/GONOPROXY 只展示策略摘要。
   - 环境变量白名单和 builder workspace 只读/输出目录边界可验证。
   - 证据：`sanitizeLog` 对 secret/token/credential/private path 和 Go private policy 做脱敏；`buildEnvironment` 只转发白名单和请求显式指定的 Go policy；container builder 使用只读 rootfs、`/src:ro` 和独立 `/out`；`TestSanitizeLogRedactsBuildSecretsAndPrivatePolicy`、`TestBuildEnvironmentUsesWhitelistAndExplicitGoPolicy`、`TestDockerRunArgsEnforceContainerBuildBoundaries`、`TestSourceBuildModModeDefaultsReadonlyAndHonorsVendor` 覆盖这些边界。
5. 补 build cancel/retry 和 GC 失败路径：
   - queued、running、cancelled、succeeded、failed 状态可审计。
   - queued/running build 的 source package 被 GC 保护。
   - completed build log 可由 artifact GC 清理。
   - source/artifact GC 不删除 active、desired、snapshot 引用。
   - 证据：`CancelBuild`、`RetryBuild`、`RunBuild` 写审计操作；`ReferencedArtifactIDs` 保护 queued/running source 和 succeeded build artifact；`ClearBuildLog` 不清理 in-flight build；`TestManagerBuildCancelRetryAuditsStates`、`TestManagerArtifactGCProtectsQueuedBuildSource`、`TestManagerArtifactGCProtectsRunningBuildSourceAndLog`、`TestManagerArtifactGCClearsCompletedBuildLog`、`TestManagerArtifactGCProtectsDesiredAndSnapshotReferences` 覆盖失败路径。

## 验收

- container builder 能构建 upstream-rewrite source package。
- 构建失败不影响 active artifact。
- external CI binary artifact 缺失签名、hash 或 trusted builder provenance 时产生 blocking supply-chain issue。
- artifact GC 保护仍被 build 或 desired/snapshot 引用的包，并能清理 completed build log。
- `go test ./internal/pluginmanager ./cmd/gateway` 通过。

## 回滚边界

- container builder 可通过配置禁用。
- binary `.mcgp` 和 local dev path 必须保持可用。
