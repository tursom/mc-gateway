# 管理页面设计

## 背景

当前 gateway 已经具备 TCP 与 HTTP/WebSocket 的端口复用能力：

- Minecraft 原始 TCP 连接进入 `handleRequest`，再按握手包里的 host 路由到上游。
- WebSocket 连接先进入 HTTP handler，再 upgrade 为 WebSocket，最后同样复用 `handleRequest`。
- `tcp_web_port_reuse.go` 已经提供首包识别、连接回放和 `http.Server.Serve(chanListener)` 这条分流路径。

新的管理方案不再以 `config.toml` 为中心。进程默认启动即可工作：

- 默认在 `25565` 端口启动 Minecraft TCP 监听。
- 同一个 `25565` 端口同时承载后台管理页面和管理 API。
- 用户、权限、路由、其他协议服务配置都持久化到 SQLite。
- 其他服务和路由通过后台管理配置，不再依赖配置文件。

## 功能需求对齐

第一版管理页面按以下边界设计。

### 必须支持

- 进程启动不依赖配置文件；缺省状态下直接监听 `25565`。
- TCP 转发和后台管理默认共用同一个 TCP listener。
- 管理入口必须有认证保护。复用公网 Minecraft 端口时，不能出现无认证管理 API。
- 管理页面内置用户和权限系统，角色分为管理员、成员和游客。
- 所有持久化状态进入 SQLite，包括用户、路由、服务配置和审计日志。
- 页面提供一个实际可用的后台界面，而不是只返回 JSON：
  - 概览：进程 PID、运行时长、SQLite 路径、TCP/Admin 端口、各协议服务状态。
  - 路由管理：展示、搜索、新增、修改、删除 SQLite 中的路由记录。
  - 服务管理：配置 KCP、QUIC、WebSocket 的启用状态、端口和协议参数。
  - 用户管理：管理员可以新增、禁用、修改角色和重置密码。
  - 连接统计：展示总连接数、当前活跃连接数、路由命中次数、上游连接失败次数。
- 游客登录后可以查看当前 SQLite 路由记录，但不能修改路由、配置服务或查看用户列表。
- 路由记录必须持久化到 SQLite，进程重启后仍然生效。
- 服务配置必须持久化到 SQLite，进程重启后按后台配置恢复。
- 所有写操作需要记录审计日志，至少包含操作者、来源 IP、操作类型、目标对象和结果。

### 暂不支持

- 不再支持 `config.toml` 作为启动配置、热加载配置或路由来源。
- 不做多租户、组织空间和自定义细粒度权限。
- 不做 HTTPS/TLS 终止；需要 HTTPS 时由外部反向代理或负载均衡器处理。
- 不做 Minecraft 玩家在线列表、踢人、封禁等游戏服管理能力。
- 不做插件管理、二进制升级、进程重启。
- 不把每条连接的完整客户端地址、目标地址长期保存在内存里。

## 默认启动设计

无配置文件时使用以下默认值：

| 项 | 默认值 | 环境变量 |
| --- | --- | --- |
| SQLite 数据库 | `mc-gateway.sqlite3` | `MC_GATEWAY_DB` |
| TCP/Admin 监听端口 | `25565` | `MC_GATEWAY_TCP_ADMIN_PORT` |
| Admin 页面路径 | `/admin/` | `MC_GATEWAY_ADMIN_PATH` |
| Admin API 前缀 | `/admin/api` | `MC_GATEWAY_ADMIN_API_PREFIX` |
| KCP | 默认禁用 | 后台配置 |
| QUIC | 默认禁用 | 后台配置 |
| WebSocket | 默认禁用 | 后台配置 |
| 路由表 | 默认空表 | 后台配置 |
| 会话有效期 | 8 小时 | 后台配置 |

启动期环境变量规则：

- `MC_GATEWAY_TCP_ADMIN_PORT` 只在启动时读取，必须是 `1-65535` 的整数；为空时使用 `25565`。
- `MC_GATEWAY_ADMIN_PATH` 只在启动时读取，必须以 `/` 开头，规范化为以 `/` 结尾；为空时使用 `/admin/`。
- `MC_GATEWAY_ADMIN_API_PREFIX` 只在启动时读取，必须以 `/` 开头，规范化为不以 `/` 结尾；为空时使用 `/admin/api`。
- `MC_GATEWAY_ADMIN_API_PREFIX` 不能等于 `MC_GATEWAY_ADMIN_PATH`，也不能落在静态资源路径下。
- 环境变量覆盖的是本次进程的 Admin 入口；第一次创建 SQLite 默认服务配置时，应把解析后的 TCP/Admin 端口写入 `services.tcp_admin.port`。

启动流程：

1. 读取并校验启动期环境变量，得到 SQLite 路径、TCP/Admin 端口、Admin 页面路径、Admin API 前缀。
2. 打开 SQLite 数据库；不存在时自动创建。
3. 执行 schema migration。
4. 确保默认服务配置存在：TCP/Admin listener 启用，端口为启动期解析后的端口。
5. 如果用户表为空，进入首次初始化模式。
6. 加载启用的路由记录为内存只读快照。
7. 启动 TCP/Admin 共享 listener。
8. 按 SQLite 中的服务配置启动 KCP、QUIC、WebSocket。

首次初始化：

- 当用户表为空时，启动期解析后的 Admin 页面路径显示初始化管理员页面。
- 初始化接口只允许创建第一个管理员账号。
- 第一个管理员创建成功后，初始化接口永久关闭。
- 也可以通过环境变量 `MC_GATEWAY_ADMIN_PASSWORD` 配合默认用户名 `admin` 在启动时创建初始管理员；这不是配置文件，只是无交互部署入口。

SQLite 路径：

- 默认使用当前工作目录下的 `mc-gateway.sqlite3`。
- 如需改路径，第一版只接受启动参数或环境变量，例如 `MC_GATEWAY_DB`；不引入配置文件。

## 运行模型

TCP/Admin listener 是基础入口：

```text
net.Listen(:startup env port or db service setting)
        |
        v
Accept
        |
        v
读取首批字节
        |
        +-- HTTP/Admin/WebSocket
        |       -> HTTP channel listener
        |       -> http.Server.Serve
        |
        +-- Minecraft TCP
                -> 回放首批字节
                -> handleRequest
                -> mapToHost
                -> route snapshot
                -> proxyConnections
```

核心规则：

- TCP/Admin listener 默认永远启用，避免后台管理入口丢失。
- TCP/Admin 端口可以由启动期环境变量覆盖，也可以保存在 SQLite；最终监听端口以启动期解析结果优先。
- TCP/Admin 端口修改后第一版按重启后生效处理。
- Admin 页面路径和 API 前缀只由启动期环境变量控制，不通过后台页面修改，避免运行中替换管理入口导致当前会话失效。
- KCP、QUIC、WebSocket 由后台管理配置启用状态和端口。
- 如果某个服务的端口或协议参数无法热更新，页面必须标记为“重启后生效”或提供明确的重启服务操作。
- 路由变更不需要重启，也不需要 reload；SQLite 写入成功后刷新路由快照即可影响新连接。

## HTTP 路由与页面

页面使用 Go `embed` 打包到单个二进制，不引入 Node 构建链。

建议目录：

```text
cmd/gateway/admin.go
cmd/gateway/admin_api.go
cmd/gateway/admin_auth.go
cmd/gateway/admin_db.go
cmd/gateway/admin_static.go
cmd/gateway/admin_static/
  index.html
  app.css
  app.js
```

API 路由：

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/admin/` | 公开页面 | 管理页面入口或首次初始化页面 |
| GET | `/admin/app.css` | 公开页面 | 页面样式 |
| GET | `/admin/app.js` | 公开页面 | 页面脚本 |
| POST | `/admin/api/setup` | 仅用户表为空 | 创建第一个管理员 |
| POST | `/admin/api/auth/login` | 未登录 | 用户名密码登录 |
| POST | `/admin/api/auth/logout` | 已登录 | 注销当前会话 |
| GET | `/admin/api/me` | 已登录 | 当前用户、角色和权限 |
| GET | `/admin/api/status` | 成员、管理员 | 进程、端口、数据库、服务状态 |
| GET | `/admin/api/routes` | 游客、成员、管理员 | 当前 SQLite 路由列表 |
| PUT | `/admin/api/routes/{host}` | 成员、管理员 | 新增或更新一条路由 |
| DELETE | `/admin/api/routes/{host}` | 成员、管理员 | 删除一条路由 |
| GET | `/admin/api/services` | 成员、管理员 | 服务配置和运行状态 |
| PUT | `/admin/api/services/{name}` | 管理员 | 修改 KCP、QUIC、WebSocket 配置 |
| POST | `/admin/api/services/{name}/restart` | 管理员 | 重启指定服务 |
| GET | `/admin/api/metrics` | 成员、管理员 | 连接和路由统计 |
| GET | `/admin/api/users` | 管理员 | 用户列表 |
| POST | `/admin/api/users` | 管理员 | 新增用户 |
| PATCH | `/admin/api/users/{username}` | 管理员 | 修改用户角色、状态或密码 |
| DELETE | `/admin/api/users/{username}` | 管理员 | 删除用户 |
| GET | `/admin/api/audit-logs` | 管理员 | 审计日志 |

页面形态：

- 用户表为空时只显示初始化管理员界面。
- 未登录且已初始化时只显示登录界面。
- 成员和管理员顶部固定显示 gateway 状态、SQLite 路径和服务状态。
- 路由表支持按 host、upstream、启用状态搜索。
- 游客登录后只展示当前路由表，不展示新增、编辑、删除、服务配置、用户管理入口。
- 新增、编辑、删除路由使用弹窗或行内表单，不单独跳转页面。
- 删除 `default` 路由需要二次确认，因为它是 fallback 路由。
- API 错误直接展示服务端返回的错误信息，便于运维判断问题。

## 用户与权限设计

第一版采用 SQLite 用户表和内存会话，不开放匿名管理 API。这里的“游客”是已登录用户的只读角色，不是未登录访问。

角色权限：

| 角色 | 权限 |
| --- | --- |
| 管理员 `admin` | 所有管理能力，包括用户管理、服务配置、路由管理、状态和指标查看 |
| 成员 `member` | 查看状态、指标和服务状态；新增、修改、删除路由 |
| 游客 `guest` | 只能登录、注销、查看自己的信息、查看当前 SQLite 路由 |

认证流程：

- `POST /admin/api/setup` 只在用户表为空时可用，用于创建第一个管理员。
- `POST /admin/api/auth/login` 使用用户名和密码登录。
- 登录成功后服务端生成随机会话 token，响应给前端。
- 前端把会话 token 保存在 `sessionStorage`，后续 API 使用 `Authorization: Bearer <session_token>`。
- 服务端在内存中保存会话，超过会话有效期后失效；进程重启后所有会话失效。
- 未登录返回 `401`，已登录但权限不足返回 `403`。

用户规则：

- 密码只保存哈希，建议使用 bcrypt 或 argon2id，不保存明文密码。
- 用户字段至少包含：`username`、`role`、`password_hash`、`disabled`、`created_at`、`updated_at`。
- 管理员不能删除或禁用最后一个可用管理员账号。
- 修改用户、重置密码、禁用用户都要记录审计日志。

## SQLite 存储设计

建议使用一个 SQLite 数据库保存所有后台状态。优先选择不依赖 CGO 的 SQLite driver，降低交叉编译和容器部署成本。

基础表：

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    username TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    disabled INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS routes (
    host TEXT PRIMARY KEY,
    upstream TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    note TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS services (
    name TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL,
    port INTEGER NOT NULL DEFAULT 0,
    options_json TEXT NOT NULL DEFAULT '{}',
    restart_required INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    source_ip TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    success INTEGER NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_routes_enabled ON routes(enabled);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
```

服务配置记录：

| name | 默认 enabled | 默认 port | 说明 |
| --- | --- | --- | --- |
| `tcp_admin` | 1 | 25565 | Minecraft TCP 和 Admin 共用入口 |
| `kcp` | 0 | 25566 | KCP 服务 |
| `quic` | 0 | 25565 | QUIC 服务，具体协议参数放在 `options_json` |
| `websocket` | 0 | 25566 | WebSocket 服务，path 放在 `options_json` |

注意：

- `tcp_admin` 是基础入口，不允许在后台禁用。
- `tcp_admin.port` 修改后第一版按重启后生效处理。
- KCP、QUIC、WebSocket 配置修改后，如果当前实现无法安全热更新，则标记 `restart_required = 1` 并在页面提示。
- 后续可以把服务运行状态放在内存中，不需要写入 SQLite。

SQLite 运行参数：

- 启动后设置 `PRAGMA journal_mode=WAL`。
- 设置合理的 `busy_timeout`，避免后台写入和读取短暂冲突直接失败。
- 所有写操作使用事务。
- 路由和服务配置写入成功后，刷新对应的内存快照。

## 路由存储与运行时读取

路由管理以 SQLite 为唯一持久化来源。

运行时读取：

- 启动时从 SQLite 加载所有 `enabled = 1` 的路由，发布为内存只读快照。
- `mapToHost` 从路由快照读取 host 到 upstream 的映射，不再读取 `config.Hosts`。
- 路由 API 写入 SQLite 成功后，重新加载路由快照；不需要 reload。
- 如果快照中没有目标 host，则尝试 `default`；没有 `default` 时按现有未命中逻辑关闭连接并记录日志。

写入流程：

1. 校验 host、upstream、enabled 和 note。
2. 在 SQLite 事务中执行 insert、update、delete 或 enable/disable。
3. 事务提交成功后重新加载路由快照。
4. 快照发布成功后返回 API 成功。
5. 如果快照发布失败，返回错误并保留 SQLite 已提交数据；旧路由快照继续服务已有逻辑。

并发与一致性：

- 后台写操作串行执行，避免并发修改同一 host 时互相覆盖。
- 路由快照使用 copy-on-write 发布，转发热路径只做只读 map 查询。
- 没有 `config.toml` 热加载，也不存在配置文件覆盖后台路由的问题。

## 路由校验规则

`host` 校验：

- 不能为空。
- 允许 `default`。
- 不允许包含空白字符。
- 不允许包含 `/`，避免和 URL 路由混淆。

`upstream` 校验：

- 不能为空。
- 支持现有前缀：无前缀 TCP、`kcp://`、`quic://`、`haproxy://`。
- 去掉协议前缀后，必须能解析为 `host:port`。
- 不在第一版新增 websocket upstream 前缀，因为当前 README 未列出该前缀的实际实现。

## 指标设计

第一版只做轻量统计，避免影响 TCP 转发热路径。

建议新增全局 `gatewayMetrics`，内部使用 `sync/atomic`：

- `total_connections`
- `active_connections`
- `tcp_connections`
- `websocket_connections`
- `route_hits{host}`
- `route_misses`
- `upstream_dial_errors`

实现原则：

- 只在连接进入、退出、路由成功或失败时更新计数。
- 不在每次 `copyForward` 读写时计数。
- route 维度只记录 host 计数，不记录完整客户端 IP。

## 测试计划

单元测试：

- 无配置文件时默认创建 SQLite 并写入默认服务配置。
- 默认启动配置包含 `tcp_admin` enabled 和端口 `25565`。
- Admin path、api_path 默认分别为 `/admin/` 和 `/admin/api`。
- `MC_GATEWAY_TCP_ADMIN_PORT`、`MC_GATEWAY_ADMIN_PATH`、`MC_GATEWAY_ADMIN_API_PREFIX` 能覆盖启动期入口配置。
- 启动期环境变量非法时返回明确错误，不创建含错误值的默认服务配置。
- 首次初始化管理员、重复初始化拒绝、管理员登录和禁用用户拒绝登录。
- 登录、注销、会话过期。
- 权限 middleware 的 200、401、403 场景。
- 管理员用户 API 的新增、改角色、重置密码、禁用和删除。
- 不能删除或禁用最后一个可用管理员。
- 路由 API 的新增、修改、删除和校验失败。
- SQLite schema migration、空库初始化、WAL 和 busy timeout 配置。
- 路由写入 SQLite 后能刷新内存路由快照。
- 服务配置 API 能更新 KCP、QUIC、WebSocket 配置并标记是否需要重启。
- 游客可以读取 routes，但不能写 routes、配置 services、读取 metrics 或 users。

集成测试：

- 不提供 `config.toml` 时，进程默认通过 `25565` 提供 TCP/Admin 入口。
- 设置 `MC_GATEWAY_TCP_ADMIN_PORT` 和 Admin path/API prefix 后，进程通过指定端口和路径提供后台入口。
- 设置自定义 Admin API prefix 后，登录、状态、路由等 API 都挂载到新的 prefix 下。
- 用户表为空时可访问初始化页面并创建第一个管理员。
- 管理员或成员登录后可访问 `/admin/api/status`。
- 游客账号登录后可以通过 `25565` 读取 `/admin/api/routes`。
- 成员通过后台新增或修改路由后，新的 Minecraft host 立即按 SQLite 记录转发。
- 同一端口下 Minecraft 握手仍然进入 TCP 分支，并能完成 host 路由。
- WebSocket 启用后，按后台配置的端口和 path 提供服务。

性能验证：

- 管理页面变更后继续运行现有 TCP 转发 benchmark。
- 重点确认路由快照查询和指标计数没有进入已建立连接后的 `copyForward` 热路径。

建议命令：

```bash
go test ./...
go test -race ./...
go test -run TestTcpWebPortReuse ./cmd/gateway
go test -bench 'Benchmark.*tcp' ./cmd/gateway
```

## 实施步骤

1. 移除 `config.toml` 启动依赖，改为无配置默认值启动。
2. 新增 SQLite 打开、schema migration、WAL 和默认服务配置初始化。
3. 增加启动期环境变量解析和校验，覆盖 TCP/Admin 端口、Admin 页面路径和 Admin API 前缀。
4. 将 TCP/Admin 共享 listener 固定为基础入口，默认端口 `25565`，并支持启动期端口覆盖。
5. 把 WebSocket handler 创建逻辑扩展为统一 `newGatewayHTTPHandler()`。
6. 增加用户表、首次初始化管理员、密码哈希和会话管理。
7. 增加 Admin 静态页面、初始化页面、登录页面和权限 middleware。
8. 增加 auth、me、users、status、routes、services、metrics、audit-logs API。
9. 将 `mapToHost` 改为读取 SQLite 路由快照，不再读取 `config.Hosts`。
10. 实现 routes 写入 SQLite、事务提交和路由快照刷新。
11. 实现 services 写入 SQLite，并接入 KCP、QUIC、WebSocket 启动配置。
12. 实现 users 写入 SQLite 和权限校验。
13. 移除或废弃配置文件 watcher 和基于文件的 reload 逻辑。
14. 补充测试和 benchmark 验证。

## 验收标准

- 没有 `config.toml` 时，gateway 默认在 `25565` 启动 TCP 转发和后台管理页面。
- `http://<host>:25565/admin/` 可访问初始化或登录页面。
- 设置 `MC_GATEWAY_TCP_ADMIN_PORT`、`MC_GATEWAY_ADMIN_PATH`、`MC_GATEWAY_ADMIN_API_PREFIX` 后，后台入口使用环境变量指定的端口和路径。
- 同一端口下 Minecraft 客户端连接和 host 路由行为正常。
- 用户表为空时只能创建第一个管理员；创建后初始化接口不可再次使用。
- 未登录的管理 API 请求返回 `401`，已登录但权限不足返回 `403`。
- 管理员可以新增用户、禁用用户、修改角色和重置密码。
- 管理员可以配置 KCP、QUIC、WebSocket 服务。
- 成员可以查看、搜索、新增、修改、删除 SQLite 路由记录，变更后立即影响新连接路由。
- 游客可以查看和搜索当前 SQLite 路由记录，但看不到写操作入口，直接调用写 API 返回 `403`。
- 删除或修改旧 `config.toml` 不影响后台管理中的用户、服务配置和路由。
- SQLite 写入失败或路由快照刷新失败时，页面展示明确错误，旧路由快照仍可继续工作。
- `go test ./...`、`go test -race ./...` 和 TCP benchmark 完成后无新增失败。
