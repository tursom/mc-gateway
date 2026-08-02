# 阶段 7：Extension Ecosystem

## 目标

在稳定的插件主路径上扩展生态能力：route resolver、status ping、event subscriber、provider、middleware、rule/policy engine、Admin auth provider 和更多官方示例插件。

阶段结束后，用户可以不写完整 connection takeover，也能用更低成本 extension point 完成常见运维需求。

## 可用性检查点

阶段结束时必须能做到：

- 官方 rule/policy 插件能完成 host rewrite、IP 黑白名单、简单限流或维护模式。
- route provider 能从外部源或缓存产生可解释 route decision。
- status ping 插件能自定义 MOTD/版本提示。
- event subscriber 能异步投递审计或插件事件。
- Admin auth provider 如果启用，不影响 MC 连接路径，本地 admin break-glass 保留。

## 范围

### Route

- `route.resolve/v1`
- `route.resolver/v1`
- route decision schema。
- provider cache。
- SQLite fallback。
- refresh action。

### Status

- `status.ping/v1`
- MOTD。
- favicon。
- online/max players。
- version text。
- maintenance window。

### Middleware

预留并选择性实现：

- `connection.filter/v1`
- `handshake.filter/v1`

必须有确定顺序、timeout、panic recover 和 fail policy。

### Provider

- provider singleton。
- priority/fallback。
- plugin dependencies。
- `auth.provider/v1` 只作为插件间认证来源复用，不给 gateway core 组装 MC 登录流程。

### Event subscriber

- best_effort。
- at_least_once。
- queue。
- retry。
- dead letter。
- replay/drop action。

### Rule / Policy Engine

官方插件形式提供：

- host rewrite。
- source CIDR allow/deny。
- simple rate limit。
- maintenance mode。
- upstream rewrite。

### Admin Auth Provider

预留或实现：

- OIDC。
- LDAP。
- external identity binding。
- break-glass local admin。
- gateway-issued session。

## 明确不做

- 不开放 play 阶段 packet filter 作为默认生产能力。
- 不让 `auth.provider/v1` 进入 gateway core MC 登录流水线。
- 不允许插件自定义 Admin 权限绕过 gateway 权限模型。
- 不允许插件注入 Admin 自定义 HTML/JS。

## 实现任务

1. 实现 route decision 模型。
2. 实现 route provider cache 和 refresh action。
3. 实现 status ping extension point。
4. 实现 event subscriber delivery。
5. 实现 provider registry。
6. 实现 rule/policy 官方插件。
7. 预留或实现 Admin auth provider。
8. 增加示例插件和 conformance fixture。

## 验收

- route provider 返回 override/fallback/reject/pass 都能在 Admin 解释。
- 外部 route source 不可用时能使用 cache 或 SQLite fallback。
- status 插件能按 host 返回不同 MOTD。
- event subscriber 失败不影响连接路径。
- rule 插件配置错误不会破坏默认路由。
- Admin auth provider 不可用时，本地 admin 仍可登录。

## 回滚策略

- route provider disable 后恢复 SQLite route snapshot。
- status 插件 disable 后恢复默认 status。
- event subscriber disable 后只停止外部投递，不删除本地审计。
- rule 插件冲突时通过 priority/scope 修复或禁用。

## 实现说明

- Route/status/middleware/provider/event subscriber 仍复用插件 `Gateway.Hook` 注册模型，新增 typed SDK 结构保持和 `upstream.connect/v2` 一致。
- 官方 rule/policy 以内置官方插件 `official.rule-policy` 提供，管理员启用后通过插件配置完成 host rewrite、source CIDR allow/deny、simple rate limit、maintenance mode 和 upstream rewrite。
- Admin auth provider 当前作为 provider registry 能力预留和展示，不进入 MC 连接路径，也不替代本地 admin break-glass 登录。
- Route provider 失败时优先使用 provider cache，未命中时回退到 SQLite route snapshot。
