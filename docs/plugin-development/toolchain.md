# 工具链命令

插件开发统一使用 `gateway plugin` CLI。命令既服务本地开发，也生成 CI、review 和 Admin API 能复用的证据。

## 事实源和 Schema

```sh
go run ./cmd/gateway plugin features
go run ./cmd/gateway plugin schema export --section cli
go run ./cmd/gateway plugin schema export --section manifest
go run ./cmd/gateway plugin schema export --section admin-api
go run ./cmd/gateway plugin schema export --section conformance-fixture
```

`features` 是能力事实源。文档、UI、CLI 和测试都不应把 reserved、partial 或 future-gated 能力表达成无条件可用。

## Manifest 命令

```sh
go run ./cmd/gateway plugin manifest format .
go run ./cmd/gateway plugin manifest format . --write
go run ./cmd/gateway plugin manifest format . --canonical-json --type binary
go run ./cmd/gateway plugin manifest explain runtime.type
go run ./cmd/gateway plugin manifest explain upstream.connect/v1
```

规则：

- JSON manifest 可以被 `--write` 格式化。
- YAML/TOML/JSONC 以保留注释为优先，不能安全保留时不覆盖源文件。
- `--canonical-json --type binary|source` 用于查看最终写入 `.mcgp` 的 `manifest.json`。

## 初始化

```sh
go run ./cmd/gateway plugin init ./my-plugin \
  --id my-plugin \
  --template upstream-dialer \
  --module example.com/my-plugin
```

当前实现的模板分支是 `upstream-dialer` 和 `protocol-proxy`。其它 runtime 模板属于设计预留，不要在开发文档中承诺可直接生成。

## 构建

```sh
go run ./cmd/gateway plugin build .
go run ./cmd/gateway plugin build . --type binary
go run ./cmd/gateway plugin build . --type source
go run ./cmd/gateway plugin build . --type both
go run ./cmd/gateway plugin build . --type both --out dist
go run ./cmd/gateway plugin build . --type binary --skip-tests
```

默认行为：

- 默认目标目录是 `.`。
- 默认类型是 `binary`。
- 不传 `--out` 时输出到 `dist/`。
- `--type both` 生成 `dist/<plugin-id>.mcgp` 和 `dist/<plugin-id>-source.mcgp`。
- 默认会先运行 `go test ./...`，除非传 `--skip-tests`。
- source 包默认 `--vendor=true`，会用 `go mod vendor` 收集 vendor。

binary 构建会：

1. 读取唯一 manifest source。
2. 运行测试。
3. 执行 `go build -buildmode=plugin -trimpath -buildvcs=false`。
4. 用 `go tool nm` 校验 `Plugin` 符号。
5. 写入稳定 zip 和 canonical `manifest.json`。

## Source build

```sh
go run ./cmd/gateway plugin build . --type source
go run ./cmd/gateway plugin build --from-source dist/<plugin-id>-source.mcgp --out dist/<plugin-id>-built.mcgp
go run ./cmd/gateway plugin source-build dist/<plugin-id>-source.mcgp dist/<plugin-id>-built.mcgp
go run ./cmd/gateway plugin source-validate dist/<plugin-id>-source.mcgp
```

`source-build` 是兼容命令；新文档和 CI 优先使用 `build --from-source`。

## 测试、校验和查看

```sh
go run ./cmd/gateway plugin test . --profile unit,manifest
go run ./cmd/gateway plugin validate .
go run ./cmd/gateway plugin validate dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin inspect dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin compat dist/<plugin-id>.mcgp
```

非目录目标支持的 test profile 较少，通常用于 manifest、compat 或 conformance。源码目录才运行单元测试和 harness。

## Contract 和 Conformance

```sh
go run ./cmd/gateway plugin contract . --config testdata/config.json
go run ./cmd/gateway plugin conformance . --config testdata/config.json
go run ./cmd/gateway plugin conformance . --fixture conformance.json
```

`contract` 检查 manifest、extension point、capabilities 和 config fixture。`conformance` 在 contract 基础上执行当前 fixture 和内置场景。

## 发布证据命令

```sh
go run ./cmd/gateway plugin preflight dist/<plugin-id>.mcgp --profile prod --config config/prod.json
go run ./cmd/gateway plugin self-test dist/<plugin-id>.mcgp --profile prod
go run ./cmd/gateway plugin benchmark dist/<plugin-id>.mcgp \
  --profile prod \
  --benchmark-profile ci \
  --p95-ms 5 \
  --p99-ms 20 \
  --error-rate 0
go run ./cmd/gateway plugin sbom generate dist/<plugin-id>.mcgp --out dist/sbom.json
go run ./cmd/gateway plugin sign verify dist/<plugin-id>.mcgp \
  --signature dist/<plugin-id>.mcgp.sig \
  --public-key keys/plugin-publisher.pub
```

## 本地开发 Gateway 操作

```sh
export MC_GATEWAY_ADMIN_URL=http://127.0.0.1:25565/admin/
export MC_GATEWAY_ADMIN_TOKEN=<admin-token>

go run ./cmd/gateway plugin upload dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin status <plugin-id>
go run ./cmd/gateway plugin config validate <plugin-id> --artifact <artifact-id> --config testdata/config.json
go run ./cmd/gateway plugin enable <plugin-id> --artifact <artifact-id> --config testdata/config.json
go run ./cmd/gateway plugin logs <plugin-id>
go run ./cmd/gateway plugin events <plugin-id>
go run ./cmd/gateway plugin metrics <plugin-id>
go run ./cmd/gateway plugin diagnose <plugin-id>
go run ./cmd/gateway plugin disable <plugin-id>
```

开发调试也走 Admin API，不绕过服务端校验。

## Repository、Promotion 和 Transfer

```sh
go run ./cmd/gateway plugin repo search --index repo.json --version 0.1.0
go run ./cmd/gateway plugin repo show --index repo.json <plugin-id>
go run ./cmd/gateway plugin transfer <artifact-id> --target-gateway http://target/admin/ --target-token <token>
go run ./cmd/gateway plugin export dist/<plugin-id>.mcgp --profile staging --config config/staging.json --out promotion.json
go run ./cmd/gateway plugin import promotion.json --dry-run
go run ./cmd/gateway plugin diff --baseline promotion.json --target dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin drift --baseline promotion.json --target dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin dr-drill promotion.json
go run ./cmd/gateway plugin apply promotion.json --dry-run
```

Promotion bundle 默认不包含 secret 明文、secret 密文和 runtime state。跨环境 apply 必须重新提供目标环境配置和 secret mapping。
