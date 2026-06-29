# 开发快速开始

本文说明从零创建、运行测试、打包和本地调试一个插件的最短路径。

## 前提

- 在 `mc-gateway` 仓库根目录执行命令。
- 当前实现的开发主路径是 `go-plugin`。
- Go plugin 运行目标受 Go 标准库 `plugin` 限制，不支持 Windows。
- 开发产物必须通过 `.mcgp` 包交付，不应让管理员直接加载散落的 `.so` 文件。

查看当前事实源：

```sh
go run ./cmd/gateway plugin features
go run ./cmd/gateway plugin schema export --section cli
go run ./cmd/gateway plugin schema export --section manifest
```

## 创建插件

```sh
go run ./cmd/gateway plugin init ./my-plugin \
  --id my-plugin \
  --template upstream-dialer \
  --module example.com/my-plugin
```

当前模板：

| 模板 | runtime | 默认扩展点 | 适用场景 |
| --- | --- | --- | --- |
| `upstream-dialer` | `go-plugin` | `upstream.connect/v1` | 改写上游拨号、隧道、代理、服务发现 |
| `protocol-proxy` | `go-plugin` | `upstream.connect/v1` | 接管完整 Minecraft 字节流，自行实现登录、转发和后续代理 |

可选参数：

| 参数 | 说明 |
| --- | --- |
| `--id <id>` | 插件 ID，作为 manifest、Admin 和 CLI 主键 |
| `--name <name>` | 展示名；缺省由 ID 派生 |
| `--template <name>` | 模板名 |
| `--runtime <type>` | 默认 `go-plugin` |
| `--module <module>` | Go module path |
| `--extension <key>` | 覆盖默认 extension point |
| `--manifest-format yaml|toml|jsonc|json` | manifest source 格式 |

生成目录包含：

- `manifest.yaml` 或其它指定格式的 manifest source
- `go.mod`
- `main.go`
- `main_test.go`
- `README.md`
- `testdata/config.json`

同一源码目录只能保留一个人工维护的 `manifest.*`。如果确实需要同时保留多个格式，所有命令都必须传 `--manifest <path>` 显式选择。

## 开发循环

```sh
cd ./my-plugin
go test ./...
cd ..

go run ./cmd/gateway plugin validate ./my-plugin
go run ./cmd/gateway plugin test ./my-plugin --profile unit,manifest
go run ./cmd/gateway plugin contract ./my-plugin --config ./my-plugin/testdata/config.json
go run ./cmd/gateway plugin conformance ./my-plugin --config ./my-plugin/testdata/config.json
go run ./cmd/gateway plugin build ./my-plugin --type both
go run ./cmd/gateway plugin inspect ./my-plugin/dist/my-plugin.mcgp
go run ./cmd/gateway plugin compat ./my-plugin/dist/my-plugin.mcgp
```

开发期不要在 Go 代码中维护第二份 manifest 元数据。`gateway plugin build` 会把 manifest source 物化为包内 canonical `manifest.json`，并写入实际 Go 版本、OS、架构和 runtime entry。

## 本地调试

启动本地 gateway 后，使用 Admin token 走同一套 Admin API：

```sh
export MC_GATEWAY_ADMIN_URL=http://127.0.0.1:25565/admin/
export MC_GATEWAY_ADMIN_TOKEN=<admin-token>

go run ./cmd/gateway plugin upload ./my-plugin/dist/my-plugin.mcgp
go run ./cmd/gateway plugin status my-plugin
go run ./cmd/gateway plugin config validate my-plugin \
  --artifact <artifact-id> \
  --config ./my-plugin/testdata/config.json
go run ./cmd/gateway plugin enable my-plugin \
  --artifact <artifact-id> \
  --config ./my-plugin/testdata/config.json
go run ./cmd/gateway plugin logs my-plugin
go run ./cmd/gateway plugin disable my-plugin
```

本地调试也不要绕过上传、配置 dry-run 和 enable 路径。CLI 预检不是信任边界，服务端仍会重复校验 artifact、config、secret 和 governance。

## 示例插件

| 示例 | 目录 | 说明 |
| --- | --- | --- |
| Upstream Rewrite | [../../examples/plugins/upstream-rewrite](../../examples/plugins/upstream-rewrite) | `upstream.connect/v1` dialer mode，按 host 改写上游拨号 |
| MC Auth Proxy | [../../examples/plugins/mc-auth-proxy](../../examples/plugins/mc-auth-proxy) | `upstream.connect/v1` protocol-proxy mode，演示登录流接管、事件和指标 |
| Extension Ecosystem | [../../examples/plugins/extension-ecosystem](../../examples/plugins/extension-ecosystem) | route、status、rule、middleware、event、provider 等扩展点 fixture |

推荐从 `upstream-rewrite` 开始修改。只有需要完整 Minecraft 登录、forwarding 或 play 阶段代理时，才转向 protocol-proxy 模式。
