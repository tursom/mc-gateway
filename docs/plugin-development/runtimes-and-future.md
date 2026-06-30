# 运行时和未来能力

本文说明不同 runtime、service mode 和 future-gated 能力的开发边界。它们已经出现在 `plugin features` 中，但 maturity 不同，不能都当作生产可用主路径。

## Runtime 类型

| Runtime | 当前状态 | 开发含义 |
| --- | --- | --- |
| `go-plugin` | implemented | 当前主路径，进程内加载 `plugin.so` |
| `builtin` | partial | 仅限 gateway-owned official plugin，不是第三方包路径 |
| `sandbox-process` | partial/future-gated | enforcement 已建模，但需要 future gate；不能默认启用 |
| `wasm` | partial/future-gated | wazero runtime 已建模，用于低风险 validation 类扩展；需要 future gate |

当前第三方插件开发应以 `go-plugin` 为主。其它 runtime 文档只能表达开发边界和 future gate，不应承诺无条件生产可用。

## Service Mode

| Service mode | 当前状态 | 开发含义 |
| --- | --- | --- |
| `in-process` | implemented | gateway 进程内加载 Go plugin |
| `go-plugin-process` | partial | 子进程 plugin-host，支持部分 upstream/protocol-proxy drain-only 能力 |
| `sandbox-process` | partial/future-gated | 未来隔离进程和 WASM 承载模式 |

`go-plugin` in-process 能直接返回 `net.Conn`。跨进程、sandbox 和 WASM 不能复用这个内存内 `net.Conn` 语义，需要 stream relay 或新 ABI。

## Go Plugin

特点：

- 低改造成本。
- 可用 Go typed API。
- 可直接返回 `net.Conn`。
- 适合 `upstream.connect/v1` dialer 和 protocol-proxy。

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
- protocol-proxy drain-only。

限制：

- fd-live migration 未完成。
- full isolation 不是目标。
- sandbox enforcement 不属于该 runtime。
- 非 Linux process-table orphan discovery 不完整。
- 需要 service mode 切换，可能要求 restart。

开发建议：

- 不要假设 in-process `net.Conn` handler 可以无改动迁移到 plugin-host。
- protocol-proxy 必须覆盖 drain-only、host crash、client close、endpoint close、backpressure。
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

Sandbox 是未来隔离 runtime，当前由 future gate 控制。

已建模能力：

- control RPC。
- filesystem/network/env enforcement。
- CPU/memory enforcement。
- secret RPC。
- crash loop policy。
- diagnostic summary。

开发边界：

- 不应直接读写宿主文件系统。
- secret 通过 handle/RPC 访问，不进入 env 或普通 config。
- 网络访问必须声明并被策略允许。
- 不直接返回进程内 `net.Conn`。
- 高风险 stream/protocol-proxy 需要专门 stream relay 语义。

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

当前不能把它写成默认生产 runtime。

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
- 不用于 `upstream.connect/v1` protocol-proxy。
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
