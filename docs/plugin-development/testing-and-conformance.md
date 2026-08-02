# 测试和 Conformance

插件测试要覆盖三个层次：插件自身单元测试、gateway 契约测试、发布治理测试。绿色构建只能说明当前命令覆盖的范围，不等于插件可生产上线。

## 推荐测试链

```sh
go test ./...
go run ./cmd/gateway plugin test . --profile unit,manifest
go run ./cmd/gateway plugin contract . --config testdata/config.json
go run ./cmd/gateway plugin conformance . --config testdata/config.json
go run ./cmd/gateway plugin build . --type both
go run ./cmd/gateway plugin validate dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin compat dist/<plugin-id>.mcgp
```

涉及 Minecraft 协议、connection takeover、drain、timeout 或 packet 处理时，必须增加专门 fixture，而不能只依赖 manifest 检查。

## `plugin test` Profile

| Profile | 作用 | 输入 |
| --- | --- | --- |
| `unit` | 运行插件目录原生测试，如 `go test ./...` | 源码目录 |
| `manifest` | 校验 manifest source、目录结构和包形状 | 目录或包 |
| `harness` | 校验配置/fixture 文件可读并执行目录校验 | 源码目录 |
| `protocol-smoke` | 校验协议 smoke fixture 文件可读并执行目录校验 | 源码目录 |
| `conformance` | 对包或目录运行兼容/契约检查 | 目录或包 |

命令：

```sh
go run ./cmd/gateway plugin test . --profile unit,manifest,harness,protocol-smoke
go run ./cmd/gateway plugin test . --profile conformance --fixture conformance.json
go run ./cmd/gateway plugin test dist/<plugin-id>.mcgp --profile manifest
```

## Contract

`contract` 输出稳定 JSON，用于 CI 和 review：

```sh
go run ./cmd/gateway plugin contract . \
  --config testdata/config.json \
  --profile prod
```

检查范围：

- manifest 是否可读。
- 包形状是否符合 binary/source 类型。
- extension point 是否被当前 gateway 支持。
- `capabilities.extension_points` 是否和 `extension_points` 对齐。
- `required_features` 是否可用。
- legacy `capabilities.upstream_connect` 是否被明确拒绝。
- `ingress.service/v1` schema 是否有效。
- `config_schema` 是否是有效 JSON。
- config fixture 是否是有效 JSON。

## Conformance

`conformance` 在 contract 基础上执行内置和自定义 fixture：

```sh
go run ./cmd/gateway plugin conformance . \
  --config testdata/config.json \
  --fixture conformance.json
```

默认 fixture 文件位置：

- 源码目录：`conformance.json`
- manifest source：同目录 `conformance.json`
- `.mcgp` 包：包内 `conformance.json`

严格门禁：

```sh
go run ./cmd/gateway plugin preflight dist/<plugin-id>.mcgp \
  --profile prod \
  --config config/prod.json \
  --require-conformance-fixture
```

## `conformance.json`

示例：

```json
{
  "fixtures": [
    {"name": "contract", "status": "pass"}
  ],
  "route_decisions": ["override", "fallback", "reject", "pass"],
  "status_hosts": ["blue.example", "red.example"],
  "takeover_scenarios": [
    "byte_integrity",
    "panic_recovered",
    "timeout_deadline",
    "endpoint_close",
    "client_close",
    "backpressure_large_packet",
    "drain_disable_new_connections",
    "force_close_draining"
  ],
  "rule_evaluation_outcomes": ["allow", "deny", "error_fail_closed", "timeout_fail_closed"],
  "connection_filter_scenarios": ["allow", "reject", "error_fail_open", "error_fail_closed"],
  "handshake_filter_scenarios": ["allow", "reject", "rewrite_host", "error_fail_open", "error_fail_closed"],
  "governance_gate_scenarios": ["review_required", "warning_override", "advisory_block"],
  "event_delivery": ["best_effort", "at_least_once", "subscriber_failure_non_blocking"],
  "provider_registry": ["singleton", "priority", "fallback", "dependency", "scope", "disable"],
  "default_route_must_survive_bad_rule_config": true,
  "local_admin_break_glass": true
}
```

## 按扩展点的最低测试要求

| 扩展点 | 最低测试 |
| --- | --- |
| `upstream.connect/v2` | Next/Core、完整处理、拒绝、replacement byte integrity、panic/error/crash fail-closed、half-close、backpressure、cancel、drain、force-close |
| `route.resolve/v1` | override、fallback、reject、pass、cache TTL、explanation |
| `status.ping/v1` | host match、maintenance、版本和人数字段、未匹配 pass |
| `rule.evaluate/v1` | allow、deny、错误 fail-closed、timeout、坏配置不破坏默认 route |
| `connection.filter/v1` | allow、reject、error fail-open、error fail-closed |
| `handshake.filter/v1` | allow、reject、rewrite host、error fail-open、error fail-closed |
| `event.subscriber/v1` | best-effort、at-least-once、retry、dead-letter 或 subscriber failure non-blocking |
| provider | singleton、priority、fallback、dependency、scope、disable |
| `ingress.service/v1` | schema、端口冲突、reserved listener、TLS secret refs、future gate |

## 失败路径

每个插件至少证明：

- manifest 缺字段或不兼容时失败。
- invalid config 被 dry-run 拒绝。
- required secret 缺失被拒绝。
- handler panic 被 recover。
- handler timeout 被记录并按 fail policy 处理。
- 禁用后新连接不进入插件。
- 回滚前会重新执行当前门禁。

connection takeover 插件还要证明：

- malformed packet 不会卡死连接。
- 后端不可用能返回协议级失败或按策略关闭。
- 大包和 backpressure 不导致 goroutine 泄漏。
- drain 期间老连接可控，新连接被拒绝或走新 dispatch。

## CI 建议

```sh
go run ./cmd/gateway plugin features > dist/features.json
go run ./cmd/gateway plugin schema export --section manifest > dist/manifest-schema.json
go run ./cmd/gateway plugin test . --profile unit,manifest
go run ./cmd/gateway plugin contract . --config testdata/config.json > dist/contract.json
go run ./cmd/gateway plugin conformance . --config testdata/config.json > dist/conformance.json
go run ./cmd/gateway plugin build . --type both
go run ./cmd/gateway plugin validate dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin compat dist/<plugin-id>.mcgp
```

把 `features.json` 和 `contract/conformance` 报告随 release 保存，可帮助后续解释“该插件是按哪个 gateway 能力事实源构建的”。
