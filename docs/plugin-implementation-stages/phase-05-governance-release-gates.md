# 阶段 5：Governance And Release Gates

## 目标

把插件从“能运行”提升到“可安全进入生产”。本阶段实现准入策略、review、风险分级、冲突分析、发布门禁、preflight/self-test、benchmark 结果和安全公告响应。

阶段结束后，管理员可以解释一个插件为什么能启用、为什么被阻断、启用会影响哪些流量，以及如何回滚。

## 可用性检查点

阶段结束时必须能做到：

- 高风险插件启用前需要 review。
- protocol-proxy scope 重叠会阻断启用。
- 必需 secret、依赖、feature 缺失会阻断启用。
- preflight/self-test 失败会阻断或进入 warning。
- benchmark 结果超过阈值会进入 warning/blocking。
- denylist/advisory 命中后不能 rollback 到受影响 artifact。

## 范围

### 准入策略

实现：

- dev/staging/prod profile。
- risk level。
- policy snapshot hash。
- warning override TTL。
- review 记录绑定 artifact/config/scope/rollout/runtime limits/features/policy hash。
- denylist、quarantine、revoke。

### 冲突分析

实现：

- scope overlap。
- protocol-proxy singleton 冲突。
- provider singleton 冲突。
- middleware ordering cycle。
- shadowed handler warning。
- dispatch plan API/UI。

### Preflight 和 SelfTest

实现通用门禁：

- config。
- secret。
- external dependency 声明。
- runtime limits。
- scope/rollout。
- Minecraft capability。
- backend forwarding warning。

插件实现 `PreflightChecker` 或 `SelfTester` 时复用结果。

### 性能门禁

记录：

- benchmark profile。
- P95/P99。
- error rate。
- active proxy capacity。
- baseline diff。

默认策略：

- 退化超过 20% warning。
- 退化超过 50% 或超过 runtime limit blocking。

### 安全公告

支持本地 advisory：

- artifact sha256 match。
- plugin/version range match。
- SBOM dependency match。
- recommended action。
- fixed version。
- mitigation status。

## 明确不做

- 不强制签名。
- 不接外部漏洞库作为强依赖。
- 不做双人审批。
- 不自动升级或自动启用仓库版本。

## 实现任务

1. 实现 policy engine。
2. 实现 review API/UI。
3. 实现 denylist/quarantine/revoke。
4. 实现 conflict check。
5. 实现 preflight API 和结果存储。
6. 实现 self-test profile。
7. 实现 benchmark result 存储和门禁。
8. 实现 security advisory import/rescan/ack。
9. 将门禁接入 enable、rollback、promotion import apply。

## 验收

- 未 review 的高风险 protocol-proxy 插件不能在 prod profile 启用。
- 两个同 scope protocol-proxy 插件不能同时启用。
- required feature 缺失返回 `feature_missing`。
- secret 缺失阻断启用。
- advisory revoke 后不能 rollback 到受影响 artifact。
- warning override 过期后重新阻断相关操作。

## 回滚策略

- policy 变更不应立即删除运行中插件；先标记 drift/review_required 或 quarantine。
- quarantine 从 dispatch table 移除插件，新连接不进入；已有 protocol-proxy 连接按策略 drain/force close。
- 管理员可以回滚到未受阻断的旧 artifact。
