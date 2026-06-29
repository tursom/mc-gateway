# M9：Repository、Supply Chain、Promotion 和 DR 未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

在本地主路径、治理和运维稳定后，补齐插件分发、供应链准入、多环境 promotion、多实例自动分发和灾备演练闭环。

## 未完成工作

1. 补 repository 自动同步：
   - official/internal/file/url index 的同步、缓存、失败降级和审计。
   - repository update availability 只报告候选更新，不改变 desired 或 active state。
   - repository import 后只生成本地 artifact，并保存 governance admission preview。
2. 补签名和信任根生命周期：
   - signature verify、key rotation、revoke。
   - 信任根分发和本地策略变更审计。
   - denylist/advisory 命中后阻断 rollback、repository import apply 和 promotion apply。
3. 补 SBOM/license/CVE 链：
   - SBOM parse。
   - license allow/deny。
   - 本地、导入式和外部 vulnerability feed sync/rescan。
   - opt-in external feed scheduler 到完整自动 CVE/SBOM 扫描链的发布流程。
4. 补 repository apply 跨节点编排：
   - 自动集群级 apply。
   - 自动 artifact 分发。
   - 节点级失败重试、partial rollout 展示和回滚。
   - 目标侧仍需重新执行 dry-run 和 governance。
5. 补 promotion 多环境闭环：
   - environment override 和 secret ref mapping。
   - target desired-state apply 后不自动启用 active traffic。
   - drift report 能解释 desired 与 actual 差异。
   - 目标环境不支持的 runtime 或 policy 不会写入 desired state。
6. 补 DR 和 build-time instrumentation：
   - DR drill 验证 artifact、config hash、runtime 支持度、dry-run 和 governance，不接 production traffic。
   - instrumentation 需要真实 CI 插桩产物生成和 gateway binary 发布链。
   - `available` instrumentation metadata 必须绑定 generated diff hash、gateway binary digest、CI artifact digest、conformance、benchmark 和 smoke evidence。

## 验收

- repository import/apply 不自动启用生产流量，失败不改变 desired 或 active state。
- repository update availability 不改变任何运行态。
- denylist/advisory 阻断 rollback、repository import apply 和 promotion apply。
- target 环境不支持的 runtime 不会被 promotion 写入 desired state，更不会自动启用。
- drift report 能解释 desired 与 actual 差异。

## 回滚边界

- repository 功能失败不影响本地 artifact。
- repository import apply、promotion apply 或 DR drill 失败不改变当前 desired 或 active state。
- DR drill 不接生产流量。
