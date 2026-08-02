# 运行时和未来能力

本文说明不同 runtime、service mode 和 future-gated 能力的开发边界。它们已经出现在 `plugin features` 中，但 maturity 不同，不能都当作同等完整的生产主路径。

## Runtime 类型

| Runtime | 当前状态 | 开发含义 |
| --- | --- | --- |
| `go-plugin` | implemented | 当前主路径，进程内加载 `plugin.so` |
| `builtin` | partial | 仅限 gateway-owned official plugin，不是第三方包路径 |
| `sandbox-process` | partial | 独立进程 sandbox 数据面已可用于受支持扩展点；需要 conformance 和 sandbox policy 通过 |
| `wasm` | partial/future-gated | wazero runtime 已建模，用于低风险 validation 类扩展；需要 future gate |

当前第三方插件开发仍应优先选择 `go-plugin`。需要强隔离、跨语言或可回收进程边界时，可以选择 `sandbox-process`，但必须覆盖 sandbox conformance、capability enforcement 和部署侧隔离策略。

## Service Mode

| Service mode | 当前状态 | 开发含义 |
| --- | --- | --- |
| `in-process` | implemented | gateway 进程内加载 Go plugin |
| `go-plugin-process` | partial | 子进程 plugin-host，支持 `upstream.connect/v2` takeover 与 drain |
| `sandbox-process` | partial | 独立 sandbox 进程承载模式；切换需要 apply/restart 语义 |

`go-plugin` in-process 直接接收客户端 `net.Conn`。plugin-host 和 sandbox-process
通过 takeover stream relay 表达同一契约；WASM 不支持 `upstream.connect/v2`。

## Go Plugin

特点：

- 低改造成本。
- 可用 Go typed API。
- 可直接读取、包装或替换客户端 `net.Conn`。
- 适合 `upstream.connect/v2` connection takeover。

限制：

- 不能真正热卸载。
- ABI 受 Go 版本、OS、架构、依赖和 SDK 版本影响。
- 不提供恶意代码隔离。
- native 插件能绕过 ExternalClient 和 FileStore，因此治理是观测边界，不是强隔离边界。

开发要求：

- manifest 写明 `runtime.type: go-plugin`、`runtime.entry: plugin.so`、`runtime.entry_symbol: Plugin`。
- binary 包必须包含 `plugin.so`。
- 构建和 gateway 运行环境要兼容。

## Go Plugin Process

`go-plugin-process` 通过子进程 `plugin-host` 承载 Go plugin。当前状态是 partial。

已建模能力：

- plugin-host handshake。
- control channel。
- supervisor start/stop。
- crash tracking、crash policy、backoff。
- stream proxy protocol。
- upstream connect。
- `upstream.connect/v2` 的 `Next`、`Core`、replacement stream 和 drain。

限制：

- fd-live migration 未完成。
- full isolation 不是目标。
- sandbox enforcement 不属于该 runtime。
- 非 Linux process-table orphan discovery 不完整。
- 需要 service mode 切换，可能要求 restart。

开发建议：

- handler API 与 in-process 相同，但 stream 会通过 Unix relay 跨进程承载。
- takeover 必须覆盖 drain、host crash、client close、endpoint close、half-close 和 backpressure。
- conformance 要包含 stream proxy fixture。

命令：

```sh
go run ./cmd/gateway plugin-host handshake
go run ./cmd/gateway plugin runtime features
go run ./cmd/gateway plugin runtime status
go run ./cmd/gateway plugin runtime mode --mode go-plugin-process
go run ./cmd/gateway plugin runtime apply
```

## Sandbox Process

Sandbox 是独立进程隔离 runtime，当前状态是 partial data-plane。默认产品事实会把 `sandbox-process` 标记为可用；部署方仍可用 `MC_GATEWAY_FUTURE_RUNTIME_SANDBOX_PROCESS=0` 显式关闭。若自定义 `MC_GATEWAY_SANDBOX_POLICY_JSON`，必须保证 policy 能通过环境自检，否则 Admin/CLI 会显示 blocked 而不是 active data-plane。

已建模能力：

- control RPC。
- filesystem/network/env enforcement。
- CPU/memory enforcement。
- secret RPC。
- crash loop policy。
- diagnostic summary。
- route/rule/config request-response dispatch。
- `upstream.connect/v2` takeover stream relay。
- conformance fixture release gate。

开发边界：

- 不应直接读写宿主文件系统。
- secret 通过 handle/RPC 访问，不进入 env 或普通 config。
- 网络访问必须声明并被策略允许。
- 不直接返回进程内 `net.Conn`。
- takeover stream 必须使用 SDK 的 endpoint、replacement endpoint 和 action 契约。

Manifest 能力示例：

```yaml
runtime:
  type: sandbox-process
capabilities:
  runtime:
    required_capabilities:
      - filesystem:read
      - network:egress
```

当前只能把它作为 partial runtime 使用：受支持 extension point 可以进入数据面，未覆盖的 capability、缺失 fixture 或环境自检失败仍会阻断 prod enable。

## WASM

WASM runtime 当前是 partial/future-gated，适合低风险 validation 类扩展。

把 WASM 宣告为生产可启用前，需要先补齐真实 extension dispatch、ABI、lifecycle、governance、conformance 和 Admin/CLI fact source 闭环；实施拆解见 [WASM 运行时生产可用化实施计划](../plugin-wasm-runtime-production-plan.md)。

已建模能力：

- wazero host ABI。
- module cache。
- fuel/time/memory limits。
- 默认无文件和网络访问。
- high-risk extension reject。

开发边界：

- 优先用于 `config.validate/v1`、`rule.evaluate/v1` 等轻量逻辑。
- 明确不支持 `upstream.connect/v2`。
- 不直接访问 secret、文件、网络，除非 host ABI 显式提供。
- trap、timeout、memory limit 必须被 conformance 覆盖。

Manifest：

```yaml
runtime:
  type: wasm
  entry: plugin.wasm
extension_points:
  - type: rule
    key: rule.evaluate/v1
```

## Ingress Service

`ingress.service/v1` 用于未来 gateway-managed listener。当前是 partial，并受 future gate 控制。

已建模能力：

- schema validation。
- preflight schema gate。
- plugin port conflict。
- reserved listener conflict。
- gateway listener lifecycle。
- TLS secret refs redaction。
- disable drain。

开发边界：

- 插件不能自行任意监听端口。
- listener 由 gateway/supervisor 创建、启停和 drain。
- TLS key/cert 只能通过 secret ref。
- ingress 不等同于 upstream connect。

## Plugin Host Protocol

`plugin-host` 使用 `mc-gateway-plugin-host/v1` 协议。开发或调试时可以检查 handshake：

```sh
go run ./cmd/gateway plugin-host handshake --protocol mc-gateway-plugin-host/v1
```

`plugin-host serve` 需要 control socket，通常由 supervisor 启动，不建议插件作者手工运行生产实例：

```sh
go run ./cmd/gateway plugin-host serve --control-socket /tmp/mc-gateway-plugin.sock
```

## Instrumentation

Instrumentation 是构建期增强和 release 证据，不是 runtime plugin。

可用前提：

- generated diff hash
- gateway binary sha256
- CI artifact sha256
- conformance
- benchmark
- smoke

不要把 instrumentation 做成热加载插件。它属于官方或组织 CI 管线，产物是 gateway binary 或带证据的 release artifact。

## 未来能力写作规则

在代码、manifest、UI 和文档中表达未来能力时：

- 使用 `partial`、`reserved`、`future-gated` 或 unsupported reason。
- 不要把 future-gated runtime 写成默认可启用。
- 不要把 sandbox/WASM 写成 Go plugin 的 drop-in 替代。
- 不要把 ingress service 写成插件自行监听端口。
- 每个 future capability 都要有 conformance fixture、governance gate 和 rollback/drain 语义后才能提升成熟度。
