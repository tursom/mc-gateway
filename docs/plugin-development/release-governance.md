# 发布治理和供应链

发布治理把插件从“能构建”推进到“可解释、可审计、可回滚”。本页覆盖 preflight、self-test、benchmark、review、SBOM、签名、advisory、vulnerability、repository 和 promotion。

## 发布前本地证据

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

输出应随 release 保存，至少包含：

- artifact sha256 和 package sha256
- plugin id/version
- gateway `schema_version` 和 `api_version`
- runtime type 和 extension points
- config hash 或环境配置说明
- conformance 和 benchmark 结果
- SBOM、签名或外部 CI provenance

## Preflight

用途：

- 检查当前 action、profile、config 和 artifact 是否满足启用/回滚/promotion 门禁。
- 调用插件可选 `PreflightChecker`。
- 执行 gateway governance 检查，例如 review、conformance fixture、required feature、advisory、supply-chain、conflict。

命令：

```sh
go run ./cmd/gateway plugin preflight dist/<plugin-id>.mcgp \
  --profile prod \
  --action enable \
  --config config/prod.json \
  --require-conformance-fixture
```

不要在 preflight 中修改生产状态。它必须可重复运行。

## Self-test

用途：

- 插件内部健康检查。
- 外部依赖轻量探测。
- 本地 fixture 或缓存状态验证。

```sh
go run ./cmd/gateway plugin self-test dist/<plugin-id>.mcgp --profile prod
```

插件可实现 `api.SelfTester`，但 self-test 失败不能写入永久业务状态。

## Benchmark

```sh
go run ./cmd/gateway plugin benchmark dist/<plugin-id>.mcgp \
  --profile prod \
  --benchmark-profile release \
  --p95-ms 5 \
  --p99-ms 20 \
  --error-rate 0 \
  --active-proxy-capacity 1000 \
  --baseline-diff 0.05
```

Benchmark 证据应和扩展点风险匹配。protocol-proxy、connection filter、handshake filter 和 route resolver 属于 hot path，不能只提供 manifest 检查。

## Review 和 Override

远程开发 gateway 或生产 gateway：

```sh
go run ./cmd/gateway plugin review status <plugin-id> --artifact <artifact-id> --profile prod
go run ./cmd/gateway plugin review approve <plugin-id> --artifact <artifact-id> --profile prod --notes "release 0.1.0"
go run ./cmd/gateway plugin review reject <plugin-id> --artifact <artifact-id> --profile prod --notes "missing conformance"
go run ./cmd/gateway plugin review override <plugin-id> --artifact <artifact-id> --profile prod --reason "temporary mitigation" --ttl 3600
```

Override 只应用于 warning，不应绕过 blocking advisory、denylist、critical vulnerability 或不兼容 runtime。

## SBOM 和供应链

生成 SBOM：

```sh
go run ./cmd/gateway plugin sbom generate dist/<plugin-id>.mcgp --out dist/sbom.json
```

远程供应链评估：

```sh
go run ./cmd/gateway plugin verify <plugin-id> --artifact <artifact-id> --metadata dist/supply-chain.json
go run ./cmd/gateway plugin sbom verify <plugin-id> --artifact <artifact-id> --metadata dist/sbom.json
```

供应链 metadata 应说明：

- source sha256
- artifact sha256
- builder identity
- builder image digest
- CI run identity
- signature
- attestation
- SBOM
- license policy 结果
- vulnerability scan 结果

外部 CI 产物必须提供可信 builder、source sha、artifact sha、签名、attestation 和 SBOM，否则 repository apply、promotion apply、enable 或 rollback 可能被治理阻断。

## 签名和信任根

验证签名：

```sh
go run ./cmd/gateway plugin sign verify dist/<plugin-id>.mcgp \
  --signature dist/<plugin-id>.mcgp.sig \
  --public-key keys/plugin-publisher.pub
```

使用 trust store：

```sh
go run ./cmd/gateway plugin sign verify dist/<plugin-id>.mcgp \
  --signature dist/<plugin-id>.mcgp.sig \
  --trust-store trust-store.json \
  --key-id publisher-2026
```

维护 trust store：

```sh
go run ./cmd/gateway plugin sign key-rotation \
  --trust-store trust-store.json \
  --key-id publisher-2026 \
  --public-key keys/plugin-publisher.pub

go run ./cmd/gateway plugin sign revoke \
  --trust-store trust-store.json \
  --key-id publisher-2026 \
  --reason "key compromised"
```

## Advisory 和 Vulnerability

```sh
go run ./cmd/gateway plugin advisory scan <plugin-id> --artifact <artifact-id>
go run ./cmd/gateway plugin advisory import --feed-source local --metadata advisory.json
go run ./cmd/gateway plugin advisory sync --feed-url https://example.invalid/advisories.json
go run ./cmd/gateway plugin advisory rescan <plugin-id> --artifact <artifact-id>

go run ./cmd/gateway plugin vulnerability scan <plugin-id> --artifact <artifact-id>
go run ./cmd/gateway plugin vulnerability import --feed-source local --metadata vulnerabilities.json
go run ./cmd/gateway plugin vulnerability sync --feed-url https://example.invalid/vulns.json
go run ./cmd/gateway plugin vulnerability rescan <plugin-id> --artifact <artifact-id>
```

插件作者应在 release notes 中说明：

- 已知 advisory 是否命中。
- dependency vulnerability 是否命中。
- 是否存在 mitigation。
- fixed version 或 rollback 建议。

## Repository

本地仓库索引开发：

```sh
go run ./cmd/gateway plugin repo search --index repository.json
go run ./cmd/gateway plugin repo show --index repository.json <plugin-id>
```

远程导入和 apply：

```sh
go run ./cmd/gateway plugin repo import <plugin-id> \
  --repository-type file \
  --index repository.json \
  --version 0.1.0 \
  --trust-policy internal

go run ./cmd/gateway plugin repo apply --import-id <import-id> --dry-run
go run ./cmd/gateway plugin repo updates --repository-type file --index repository.json
```

Repository import 不应自动启用生产流量。apply 默认也应保持 desired disabled 或 dry-run，除非明确通过治理门禁。

## Promotion

导出 bundle：

```sh
go run ./cmd/gateway plugin export dist/<plugin-id>.mcgp \
  --profile staging \
  --config config/staging.json \
  --out promotion.json
```

比较、漂移和演练：

```sh
go run ./cmd/gateway plugin import promotion.json --dry-run
go run ./cmd/gateway plugin diff --baseline promotion.json --target dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin drift --baseline promotion.json --target dist/<plugin-id>.mcgp
go run ./cmd/gateway plugin dr-drill promotion.json
```

目标环境 apply：

```sh
go run ./cmd/gateway plugin apply promotion.json --dry-run --config config/prod.json
```

跨 gateway artifact transfer：

```sh
go run ./cmd/gateway plugin transfer <artifact-id> \
  --target-gateway http://target-host:25565/admin/ \
  --target-token <target-token>
```

Promotion 规则：

- bundle 不包含 secret 明文、secret 密文和 runtime state。
- apply 必须重新验证 target config hash、artifact 存在性、runtime 支持和 governance。
- apply 不应绕过 advisory、supply-chain 或 conformance 门禁。
- fact source 中的 `automatic_artifact_distribution` 和 `automatic_cluster_apply` 表示 promotion/repository apply 会进入统一目标态和分发模型；插件作者仍要把跨环境配置、secret mapping 和回滚证据显式交给目标环境校验。
- DR drill 不应改变目标 desired state。

## Instrumentation

Instrumentation 是构建期或 release 证据能力，不是 runtime plugin。它需要绑定：

- generated diff hash
- gateway binary sha256
- CI artifact sha256
- conformance
- benchmark
- smoke

不要把 instrumentation 写成普通热加载插件，也不要让它绕过插件治理门禁。
