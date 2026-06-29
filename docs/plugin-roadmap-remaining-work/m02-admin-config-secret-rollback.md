# M2：Admin、配置、Secret 和回滚未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把已有 Admin/API/CLI 管理闭环从“可操作”加固到“失败路径可证明”，并确保 UI 不误导 runtime、artifact、source build 和 governance 状态。

## 未完成工作

1. 补 UI 自动化验收：
   - artifact、source build、runtime、governance、secret 和 rollback 状态都展示成熟度。
   - future runtime 只能作为 desired/future mode 展示，不能暗示当前数据面已切换。
2. 补配置 dry-run 失败路径：
   - invalid JSON。
   - schema mismatch。
   - secret ref missing。
   - plugin `ReloadConfig` 失败。
   - dry-run 失败不能改变 active artifact 或 desired generation。
3. 补 secret 操作验收：
   - API/UI、审计、操作记录和日志摘要都不返回明文。
   - previous/current version 表达准确。
   - secret rotation 或 reload 失败不影响旧 active state。
4. 补 rollback 验收：
   - artifact rollback 前重新执行 dry-run 和 governance。
   - config-only rollback 和 full desired rollback 都要校验 sensitive diff 脱敏。
   - gate 失败不改变 desired state。
5. 补权限和审计验收：
   - member 可看不可写。
   - admin 写操作必须有审计。
   - 审计能解释谁改了什么、是否影响 active state。

## 验收

- 错误配置、缺失 secret、rollback gate 失败都不影响当前 active 插件。
- secret 不出现在 API 响应、审计、日志摘要或诊断摘要中。
- `go test ./cmd/gateway ./internal/pluginmanager` 通过。

## 回滚边界

- UI 问题不应影响 Admin API/CLI。
- 配置、secret、rollback 任一失败都必须保持旧 active state。
