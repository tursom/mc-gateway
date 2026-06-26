# 阶段 4：Admin UI、配置、Secret 和回滚

## 目标

把阶段 1 到 3 的能力做成管理员可用的管理闭环。管理页支持上传、构建状态、加载、启用、禁用、删除、配置编辑、secret 配置、配置快照和 artifact 回滚。

阶段结束时，普通运维不需要直接调用底层 API 才能完成插件日常管理。

## 可用性检查点

阶段结束时必须能做到：

- 管理员能在页面看到插件列表、artifact、runtime state 和最近错误。
- 管理员能上传 binary/source `.mcgp`。
- 管理员能编辑配置并执行 dry-run 校验。
- 管理员能配置插件 secret ref，不看到 secret 明文。
- 管理员能禁用、删除、切换 artifact 和回滚配置快照。
- 配置错误不会切换 active artifact。

## 范围

### Admin 页面

列表展示：

- plugin ID、name、version。
- artifact type。
- runtime state。
- desired state。
- active/desired/loaded artifact。
- extension points。
- priority。
- scope/rollout。
- restart required。
- health/最近错误。

详情页展示：

- manifest metadata。
- Go/API/ABI 兼容信息。
- capabilities 摘要。
- Minecraft capability 摘要。
- build 历史和日志摘要。
- current config。
- secret 状态。
- dispatch plan。
- active proxy connections。

### 配置

- 支持 JSON 编辑器兜底。
- 支持 JSON Schema 基础校验。
- 支持 `ReloadConfig()` dry-run。
- 支持 sensitive 字段脱敏 diff。
- 支持 config snapshot。
- 支持 config-only rollback 和 full desired rollback。

### Secret

实现最小 SecretStore：

- 创建/更新 secret。
- secret ref 校验。
- 当前/previous version。
- reload required/hot reload 标记。
- secret 不进入日志、审计明文和 API 响应。

### 回滚

支持：

- artifact rollback。
- config snapshot rollback。
- rollback 前重新执行兼容性和当前基础门禁。
- rollback 写审计。

## 明确不做

- 不做完整准入审批。
- 不做 SBOM/license 阻断。
- 不做外部 KMS。
- 不做复杂声明式 UI，自定义 HTML/JS 不支持。
- 不做 promotion bundle。

## 实现任务

1. 实现插件列表和详情页。
2. 实现上传和构建状态 UI。
3. 实现配置编辑、schema 校验和 dry-run。
4. 实现 secret 状态和编辑流程。
5. 实现 artifact rollback UI/API。
6. 实现 config snapshot diff/rollback。
7. 实现 restart required 展示。
8. 实现基础 permission key 映射到 admin/member/guest。
9. 所有写操作写审计。

## 验收

- 管理员可在 UI 上传并启用 upstream-rewrite。
- 管理员可在 UI 上传 source package 并查看 build result。
- 修改错误配置不会影响当前运行插件。
- secret 在页面和审计里不显示明文。
- rollback 到旧 artifact 后新连接使用旧版本。
- member 只能查看状态，不能执行写操作。

## 回滚策略

- UI 出问题时保留 Admin API/CLI 操作路径。
- 配置保存失败不改变 desired generation。
- 回滚失败不改变当前 active state。
