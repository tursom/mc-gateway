# mc-gateway

一个简易的 Minecraft 网关，通过客户端握手里的 host 将流量转发到对应的后端 Minecraft 服务器。

## 启动

mc-gateway 默认不依赖配置文件。直接启动后会在 `25565` 端口同时提供 Minecraft TCP 转发入口和后台管理入口：

- Admin 页面：`/admin/`
- Admin API：`/admin/api`
- SQLite 数据库：`mc-gateway.sqlite3`

首次启动时，如果用户表为空，可以通过 Admin 页面初始化管理员账号。也可以用 `MC_GATEWAY_ADMIN_PASSWORD` 在启动时创建默认管理员用户 `admin`。

### 启动期环境变量

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MC_GATEWAY_TCP_ADMIN_PORT` | `25565` | TCP/Admin 共享监听端口 |
| `MC_GATEWAY_ADMIN_PATH` | `/admin/` | Admin 页面路径 |
| `MC_GATEWAY_ADMIN_API_PREFIX` | `/admin/api` | Admin API 前缀 |
| `MC_GATEWAY_ADMIN_STATIC_DIR` | `cmd/gateway/admin_static` | Admin 前端静态文件目录；Docker 镜像中为 `/usr/share/mc-gateway/admin_static` |
| `MC_GATEWAY_DB` | `mc-gateway.sqlite3` | SQLite 数据库路径 |
| `MC_GATEWAY_ADMIN_PASSWORD` | 空 | 首次启动时创建默认管理员密码 |

服务启停、KCP/QUIC/WebSocket 参数、用户、权限和路由都通过后台管理写入 SQLite，不再使用 `config.toml` 作为启动配置或路由来源。

### Docker Compose

生产部署只需要 `compose.yaml`，可以直接用 Docker Compose 拉取已发布镜像并启动：

```sh
docker compose up -d
```

源码目录中包含 `compose.override.yaml`，Docker Compose 会自动加载它，因此本地构建测试仍然可以直接运行：

```sh
docker compose build
```

如果构建环境不能直接访问默认 Go 模块代理或 Docker Hub，可以通过宿主机环境变量透传构建镜像和 Go 模块代理：

```sh
GOPROXY=https://goproxy.cn,direct docker compose up -d --build
```

可选构建变量：

```env
GOPROXY=https://proxy.golang.org,direct
NODE_IMAGE=node:24.11.1-alpine
GO_IMAGE=golang:1.25.0-alpine
RUNTIME_IMAGE=alpine:3.22
```

默认使用 host network，在宿主机 `25565/tcp` 提供 Minecraft TCP 转发入口和后台管理入口，后台地址为：

```text
http://<host>:25565/admin/
```

Compose 使用本地 `./data` 目录持久化 SQLite 数据库和 WAL 文件，不挂载旧 `config.toml`。首次启动可以在后台页面初始化管理员，也可以通过 `.env` 预置默认管理员 `admin` 的密码：

```env
MC_GATEWAY_ADMIN_PASSWORD=change-me
```

常用可选项：

```env
MC_GATEWAY_TCP_ADMIN_PORT=25565
MC_GATEWAY_ADMIN_PATH=/admin/
MC_GATEWAY_ADMIN_API_PREFIX=/admin/api
MC_GATEWAY_ADMIN_STATIC_DIR=/usr/share/mc-gateway/admin_static
MC_GATEWAY_DB=/data/mc-gateway.sqlite3
```

### Admin 前端开发

Admin 前端源码位于 `cmd/gateway/admin_frontend/src`，使用 TypeScript 拆分为原生 ES modules。构建产物输出到 `cmd/gateway/admin_static/js`，Go 服务不会 embed 前端文件，而是从 `MC_GATEWAY_ADMIN_STATIC_DIR` 指向的目录透传静态响应。

修改前端后运行：

```sh
npm install
npm run build:admin
```

`master` 分支和 `v*` tag 会通过 GitHub Actions 构建并推送 Docker 镜像到 GitHub Container Registry：

```text
ghcr.io/tursom/mc-gateway:latest
```

## 权限

后台管理内置三类角色：

| 角色 | 能力 |
| --- | --- |
| 管理员 | 管理用户、服务、路由和审计日志 |
| 成员 | 查看状态，管理路由 |
| 游客 | 查看当前路由 |

## 路由

路由记录存储在 SQLite 中，后台修改后会刷新内存快照。默认 fallback 路由的 host 为 `default`。

### upstream

路由上游支持多种协议。TCP 上游直接填写地址，其他协议在地址前加协议前缀：

| 协议 | 前缀 | 说明 |
| --- | --- | --- |
| tcp | 无 | 原始 TCP 连接 |
| kcp | `kcp://` | 使用 KCP 协议连接到服务器 |
| quic | `quic://` | 使用 QUIC 协议连接到服务器 |
| haproxy | `haproxy://` | 使用 HAProxy 协议连接到服务器 |

HAProxy 协议头会保存客户端真实 IP，适合需要在后端服务端获取真实客户端 IP 的场景。

## 插件

插件系统通过 `.mcgp` 包扩展网关能力，当前主路径是可信 `go-plugin` 插件和 `upstream.connect/v1` 扩展点。管理员可以通过 Admin 页面或 `gateway plugin` CLI 上传、构建、启用、禁用、回滚和排障插件；开发者可以用同一套 CLI 初始化、测试、打包和做治理检查。

管理员操作见 [插件系统使用指南](docs/plugin-usage-guide.md)，插件作者见 [插件系统开发指南](docs/plugin-development-guide.md)。

## 可选服务

TCP/Admin listener 是基础入口，默认启用。KCP、QUIC、WebSocket 默认禁用，可以在后台管理中启用并配置端口和参数；配置修改后第一版按重启后生效处理。

| 服务 | 默认端口 | 说明 |
| --- | --- | --- |
| TCP/Admin | `25565` | Minecraft TCP 转发和后台管理共享入口 |
| KCP | `25565` | 可选 KCP 入口 |
| QUIC | `25565` | 可选 QUIC 入口 |
| WebSocket | `25566` | 可选 WebSocket 入口 |

## 日志

默认日志级别为 `info`，输出到标准输出。日志文件重开逻辑支持 logrotate 场景：

```logrotate
/var/log/mc-gateway.log {
    copytruncate
    daily
    missingok
    rotate 14
    compress
    compresscmd /usr/bin/zstd
    compressext .zst
    compressoptions -T0 --long
    uncompresscmd /usr/bin/unzstd
    notifempty
    delaycompress
    dateext
    postrotate
        if [ -f /dev/shm/mc-gateway.pid ]; then
            kill -SIGHUP $(cat /dev/shm/mc-gateway.pid)
        fi
    endscript
}
```
