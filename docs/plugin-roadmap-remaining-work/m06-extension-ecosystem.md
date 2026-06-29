# M6：Extension Ecosystem 收尾未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

按实际使用价值收尾 route、status、rule、event、middleware、provider 等扩展点，让常见需求有示例、fixture、冲突治理和 Admin 操作入口。

## 未完成工作

1. 给每个 extension point 补独立可执行 conformance：
   - route decision。
   - status ping。
   - rule/policy evaluation。
   - connection/handshake middleware。
   - event subscriber。
   - provider registry。
2. 补 route/status/rule 示例闭环：
   - external source refresh、cache TTL、SQLite fallback、decision explain。
   - MOTD、favicon、online/max players、version text 和 maintenance message。
   - host rewrite、source CIDR allow/deny、simple rate limit、upstream rewrite。
3. 补 event subscriber 生产语义：
   - 持久化 dead letter。
   - replay/drop 审计。
   - at-least-once 跨节点投递策略。
   - subscriber 失败不影响连接路径。
4. 补 middleware 和 provider 冲突治理：
   - deterministic ordering、handshake rewrite 传递、fail-open/fail-closed。
   - provider singleton、priority/fallback、dependency declaration。
   - provider 冲突通过 priority/scope 或 disable 修复。
5. 保持 Admin auth provider 边界：
   - 外部 provider 不可用时，本地 break-glass 仍可登录。
   - 未接入真实管理登录数据面前不得展示为 implemented data-plane。
6. 补 Admin 操作入口验收：
   - Dispatch plan 面板可触发 route refresh。
   - subscriber dead-letter replay/drop 有返回结果和审计。

## 验收

- 每个启用的 extension point 至少有一个示例和 conformance fixture。
- `examples/plugins/extension-ecosystem` 覆盖 route/status/event/provider 行为。
- 插件 disable 后恢复默认行为。
- event subscriber 失败不影响连接路径，死信 replay/drop 可审计。

## 回滚边界

- rule、status、route、event 插件都必须可独立 disable。
- extension ecosystem 失败不能影响默认 route 和已稳定的 `upstream.connect/v1` 主路径。
