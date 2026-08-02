> **Archived:** This document records the superseded pre-v2 plugin design. Current behavior is defined by `docs/plugin-development/extension-points.md`.

# 阶段 2：Protocol Proxy MVP

## 目标

在阶段 1 的管理和生命周期基础上，让 `legacy upstream-connect contract` 支持 connection takeover mode。插件可以返回自管 `net.Conn`，gateway 将已读取的 initial handshake bytes 回放给该连接，并把后续客户端字节转发给插件 endpoint。

本阶段使 MC 正版/三方登录插件具备技术可行性：登录、身份映射、forwarding 和登录后的协议处理都由插件完成，gateway core 只负责连接交接和治理。

阶段 2 实现后，gateway core 不解析 login/encryption/session，不消费插件内部认证结果，也不根据玩家名、UUID、权限或 session 状态改变后续路由。connection takeover 插件接管连接后，Minecraft 登录业务完全属于插件；core 只保留 initial data replay、双向 copy、draining、force close 和低基数运行摘要。

## 可用性检查点

阶段结束时必须能做到：

- route.resolve/v1 provider 仍可用。
- connection takeover 插件可以接管完整 MC 字节流。
- 初始 handshake 不丢失、不重复。
- connection takeover 插件禁用后，新连接不再进入插件；已有连接进入 draining 或按管理员操作 force close。
- `mc-auth-proxy` 示例至少能跑通一个登录失败响应或简单 session fixture。

## 范围

### `net.Conn` 接管契约

实现：

- `UpstreamConnectRequest.InitialData` 复制语义。
- returned conn 初始写入 deadline。
- 初始写入失败后的关闭和错误记录。
- 双向 copy、half-close 退化、字节数统计、耗时统计。
- active proxy connection 计数。
- connection takeover 连接 draining 状态。

### 请求字段

补齐 request 字段：

- connection ID。
- trace ID。
- source addr。
- normalized server host 和 raw server host。
- protocol version。
- next state。
- route ID、route tags、upstream raw/protocol/address。
- transport、service name、listener port。

字段新增必须只追加，不改变阶段 1 语义。

### 示例插件

提供 `examples/plugins/mc-auth-proxy` 初版：

- 注册 `legacy upstream-connect contract`。
- 使用 `net.Pipe` 或等价 endpoint。
- 读取 handshake/login start。
- 对不支持或 fixture 失败场景返回 login disconnect/kick。
- 连接 backend 并做最小透明转发。
- 通过事件或日志上报低基数失败原因。

本阶段不要求完整生产级 Mojang/Yggdrasil 实现，但示例结构必须能承载后续认证源。

### Minecraft 能力声明

manifest 支持 `minecraft` 字段：

- protocol versions。
- states。
- auth modes。
- forwarding supported/default。
- unsupported policy。
- modded 声明。

Admin API 可以先展示摘要，不要求完整 UI。

## 明确不做

- 不让 gateway core 解析 login/encryption/session。
- 不让插件返回 `AuthResult` 给 core。
- 不实现 `auth.provider/v1`。
- 不做 play 阶段 packet filter。
- 不做真实客户端大规模压测门禁。

## 实现任务

1. 定义 `UpstreamConnectRequest` 稳定 struct。
2. 增加 `ErrPass`、`ErrBlocked` 和普通 error 行为。
3. 实现 initial data replay。
4. 实现 connection takeover connection lifecycle。
5. 实现 draining 和 force close API。
6. 增加 connection takeover metrics。
7. 增加 Minecraft capability manifest schema。
8. 增加 mc-auth-proxy 示例。
9. 增加 protocol smoke test helper。

## 验收

- connection takeover 示例能读取 gateway 已解析前的完整 handshake bytes。
- 初始包只被插件处理一次。
- 插件返回不可读 conn 时，连接路径不会永久阻塞。
- 插件 panic 只影响当前连接。
- disable 后新连接不再进入插件。
- 文档明确 MC 登录业务完全属于插件。

## 回滚策略

- disable connection takeover 插件恢复默认 upstream。
- 对已有 connection takeover 连接，默认 drain；必要时 force close。
- 如果 connection takeover 功能引发问题，可以保留阶段 1 route.resolve/v1 provider 插件能力。
