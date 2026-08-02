# Manifest 和包格式

Manifest 是插件开发、管理、治理和运行时调度的共同契约。源码目录中只维护一个 manifest source，`.mcgp` 包内只信任 canonical `manifest.json`。

## 文件和包

支持的 manifest source 文件名：

- `manifest.yaml`
- `manifest.yml`
- `manifest.toml`
- `manifest.jsonc`
- `manifest.json`

包格式：

| 包类型 | `artifact_type` | 必要内容 | 说明 |
| --- | --- | --- | --- |
| binary `.mcgp` | `binary` | `manifest.json`、`plugin.so` | 可上传、校验、加载和启用 |
| source `.mcgp` | `source` | `manifest.json`、源码、`go.mod`、可选 `go.sum/vendor`、README、LICENSE、fixture | 由 gateway builder 产出 binary artifact |

源码目录中存在多个 `manifest.*` 时，`build/test/validate/manifest format/preflight/self-test/benchmark` 都应传 `--manifest <path>`。否则命令会失败，避免元数据分叉。

## 最小 Manifest

```yaml
schema_version: mc-gateway.plugin/v1
id: connection-inspector
name: Connection Inspector
version: 0.1.0
description: Inspect the untouched client stream before core processing.
artifact_type: binary
runtime:
  type: go-plugin
  entry: plugin.so
  entry_symbol: Plugin
api_version: plugin-api/v1
sdk_module: github.com/tursom/mc-gateway/plugin/api
sdk_module_version: v0.1.0
go_version: go1.24.4
go_os: linux
go_arch: amd64
extension_points:
  - type: hook
    key: upstream.connect/v2
capabilities:
  extension_points:
    - upstream.connect/v2
runtime_limits:
  handler_timeout_ms: 3000
config_schema:
  type: object
  properties:
    enabled:
      type: boolean
```

## 字段分组

| 分组 | 字段 | 开发责任 |
| --- | --- | --- |
| 身份 | `id`、`name`、`version`、`description` | 插件作者维护，`id` 必须稳定 |
| schema | `schema_version`、`api_version` | 必须匹配当前 gateway 支持值 |
| runtime | `runtime.type`、`runtime.entry`、`runtime.entry_symbol`、`runtime.build_entry` | `go-plugin` 默认 `plugin.so` 和 `Plugin` |
| build | `build.type`、`build.entry`、`build.output`、`build.tags`、`build.vendor_required` | source 包和 builder 使用 |
| SDK | `sdk_module`、`sdk_module_version`、`go_version`、`go_os`、`go_arch` | binary 包由 build 物化当前环境值 |
| 扩展点 | `extension_points` | 声明要注册的 hook/provider/service |
| 能力 | `capabilities` | 治理、冲突分析、Admin 展示和 conformance 使用 |
| 限制 | `runtime_limits` | handler 超时和未来资源限制 |
| 配置 | `config_schema` | Admin 表单、dry-run 和配置校验 |
| 敏感信息 | `secrets` | secret 名称、必填性和轮换策略 |
| 观测 | `events`、`custom_metrics` | 事件和指标白名单 |
| 外部依赖 | `external_dependencies` | `ExternalClient` 可访问的依赖 |
| 后台任务 | `background_tasks` | 定时或手动任务声明 |
| 存储 | `data_stores`、`file_stores` | 配额、保留期和导出策略 |
| 供应链 | `supply_chain` | 依赖、license、provenance、外部 CI 证据 |

## Capabilities

`capabilities` 是能力摘要，不是运行时权限强隔离。native Go 插件仍能绕过文件和网络声明，gateway 通过该字段做治理、可见性、冲突分析和发布门禁。

常用结构：

```yaml
capabilities:
  extension_points:
    - upstream.connect/v2
  middleware:
    fail_policy: fail_open
  route:
    cache_ttl_ms: 60000
  status:
    hosts:
      - play.example
  event_subscriber:
    mode: at_least_once
    max_retry: 3
  minecraft:
    protocol_versions:
      min: 47
      max: 767
      tested: [47, 760, 763, 767]
      unsupported_policy: kick
    auth_modes:
      - fixture
    forwarding:
      supported:
        - none
        - velocity-modern
      default: none
      requires_secret: false
  runtime:
    required_features:
      - feature-key
```

开发规则：

- `extension_points` 是静态声明；`capabilities.extension_points` 应同步列出它们。
- `upstream.connect/v2` 不再使用 `capabilities.upstream_connect` 或 mode 字段。
- middleware 插件要声明 `fail_policy`，避免错误时行为不明确。
- 事件和指标标签应保持低基数，不能把玩家名、UUID、token、session response、secret 或 packet payload 放进标签。
- 未来 runtime 功能必须通过 `required_features` 或 future gate 体现，不要假装生产可用。

## Config Schema

`config_schema` 使用 JSON schema 风格的对象。它用于：

- Admin UI 表单和输入提示。
- `plugin config validate` dry-run。
- 启用、回滚和 promotion apply 前的配置验证。
- 生成脱敏 diff。

示例：

```yaml
config_schema:
  type: object
  properties:
    match_host:
      type: string
    upstream:
      type: string
  required:
    - upstream
```

插件代码中仍需在 `ReloadConfig` 做业务校验。schema 用于配置形状，不能替代业务约束、外部依赖探测或 secret 存在性检查。

## Secret

声明示例：

```yaml
secrets:
  - name: velocity_forwarding_secret
    description: Velocity forwarding secret
    required: true
    type: token
    rotation:
      strategy: dual-read
      grace_period: 10m
      reload: hot
```

规则：

- secret 不写入普通 config、日志、事件、指标或诊断包。
- 需要 secret 的插件必须在 `secrets` 中声明。
- 配置只保存 secret 引用或非敏感开关。
- dry-run 和回滚必须能发现必需 secret 缺失。

## 事件、指标、外部依赖和存储声明

```yaml
events:
  - name: auth.success
    fields: [result, mode]
custom_metrics:
  - name: auth.attempts
    type: counter
    labels: [result, mode]
external_dependencies:
  - name: backend
    endpoint: tcp://
    purpose: auth
    required: true
    timeout: 3s
    retry: 0
    fail_policy: fail_closed
    data_classes: [operational]
background_tasks:
  - id: profile-cache-gc
    name: Profile cache GC
    mode: manual
    manual: true
    timeout: 1s
data_stores:
  - name: profile-cache
    schema_version: 1
    data_class: profile_cache
    quota_bytes: 1048576
    retention: 24h
    exportable: false
file_stores:
  - namespace: cache
    data_class: profile_cache
    quota_bytes: 1048576
    retention: 24h
```

这些声明必须和代码行为一致：代码只上报声明过的事件/指标，只用声明过的外部依赖名称，只写入声明过的数据分类和命名空间。

## 校验命令

```sh
go run ./cmd/gateway plugin manifest format . --canonical-json --type binary
go run ./cmd/gateway plugin manifest explain runtime.type
go run ./cmd/gateway plugin schema export --section manifest
go run ./cmd/gateway plugin validate .
go run ./cmd/gateway plugin contract . --config testdata/config.json
```

如果目标是 `.mcgp`：

```sh
go run ./cmd/gateway plugin inspect dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin validate dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin compat dist/<plugin-id>.mcgp
```
