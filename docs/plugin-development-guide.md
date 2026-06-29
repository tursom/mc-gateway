# 插件系统开发指南

本文是插件开发文档入口，面向插件作者和需要维护插件生态的工程人员。管理员上线、启停、回滚和排障流程见 [插件系统使用指南](plugin-usage-guide.md)。

当前文档以 `go run ./cmd/gateway plugin features`、`plugin/api` 和 `internal/pluginmanager` 的当前实现为事实源。设计背景见 [插件系统设计](plugin-system-design.md)，工具链目标见 [插件开发工具链设计](plugin-development-toolchain-design.md)。

## 文档结构

| 文档 | 覆盖范围 |
| --- | --- |
| [开发快速开始](plugin-development/getting-started.md) | 开发前提、目录结构、模板、示例插件和推荐开发流程 |
| [Manifest 和包格式](plugin-development/manifest-and-package.md) | `manifest.*`、`.mcgp`、binary/source 包、capabilities、schema、secret、资源声明 |
| [SDK 生命周期和入口](plugin-development/sdk-and-lifecycle.md) | `Plugin()` 符号、`api.Plugin` 生命周期、配置加载、超时、错误语义和 goroutine 管理 |
| [扩展点开发](plugin-development/extension-points.md) | 当前所有 extension point 的契约、状态、适用场景和实现示例 |
| [宿主能力](plugin-development/host-capabilities.md) | 事件、指标、日志、DataStore、FileStore、ExternalClient、后台任务和敏感信息约束 |
| [工具链命令](plugin-development/toolchain.md) | `init/build/test/validate/inspect/compat/schema/manifest/source-build` 等开发命令 |
| [测试和 Conformance](plugin-development/testing-and-conformance.md) | 单元测试、manifest 检查、contract、conformance fixture、protocol smoke、失败路径 |
| [运维和诊断](plugin-development/operations-and-diagnostics.md) | logs、events、metrics、diagnose、task、external、data/files、GC、retention 和多实例状态 |
| [发布治理和供应链](plugin-development/release-governance.md) | preflight、self-test、benchmark、review、SBOM、签名、advisory、vulnerability、promotion |
| [运行时和未来能力](plugin-development/runtimes-and-future.md) | `go-plugin`、`go-plugin-process`、sandbox、WASM、ingress、instrumentation 的开发边界 |

## 当前开发功能点覆盖矩阵

| 功能点 | 当前事实源 | 开发文档 |
| --- | --- | --- |
| `.mcgp` binary/source 包 | `artifact_types`、`build` | [Manifest 和包格式](plugin-development/manifest-and-package.md)、[工具链命令](plugin-development/toolchain.md) |
| Go plugin 模板和本地开发 | `cli.implemented_commands` 中的 `init/build/test` | [开发快速开始](plugin-development/getting-started.md) |
| Manifest source 单一维护 | `manifest` schema、manifest parser | [Manifest 和包格式](plugin-development/manifest-and-package.md) |
| SDK 生命周期 | `plugin/api.Plugin` | [SDK 生命周期和入口](plugin-development/sdk-and-lifecycle.md) |
| 配置加载和 dry-run | `NewConfigObj`、`ReloadConfig`、`config validate` | [SDK 生命周期和入口](plugin-development/sdk-and-lifecycle.md)、[发布治理和供应链](plugin-development/release-governance.md) |
| `upstream.connect/v1` dialer/protocol-proxy | `extension_points`、`HookUpstreamConnect` | [扩展点开发](plugin-development/extension-points.md) |
| route/status/rule/middleware/provider/event 扩展点 | `ExtensionPointFeatures()`、`plugin/api/hook.go` | [扩展点开发](plugin-development/extension-points.md) |
| `auth.provider/v1` 和 `admin.auth.provider/v1` 边界 | `plugin features` maturity | [扩展点开发](plugin-development/extension-points.md) |
| 事件、指标、日志、数据、文件、外部依赖、后台任务 | `api.Gateway`、manifest specs | [宿主能力](plugin-development/host-capabilities.md) |
| source build 和 builder | `build.builder_types`、`source-build` | [工具链命令](plugin-development/toolchain.md) |
| contract 和 conformance | `conformance` fact block | [测试和 Conformance](plugin-development/testing-and-conformance.md) |
| 运行观测、诊断、GC 和任务运维 | `operations` fact block | [运维和诊断](plugin-development/operations-and-diagnostics.md) |
| preflight/self-test/benchmark | `cli.implemented_commands`、governance | [发布治理和供应链](plugin-development/release-governance.md) |
| SBOM、签名、advisory、vulnerability | `supply_chain` fact block | [发布治理和供应链](plugin-development/release-governance.md) |
| repository、promotion、cross-gateway transfer | `repository`、`promotion` fact blocks | [发布治理和供应链](plugin-development/release-governance.md) |
| promotion 自动分发和集群 apply 边界 | `automatic_artifact_distribution`、`automatic_cluster_apply`、`target_desired_apply` | [发布治理和供应链](plugin-development/release-governance.md) |
| artifact distribution、多节点 apply、外部 exporter | `artifact_distribution`、`cross_node_apply`、`external_exporters` | [运维和诊断](plugin-development/operations-and-diagnostics.md) |
| `go-plugin-process` 和 plugin-host | `plugin_host`、runtime adapters | [运行时和未来能力](plugin-development/runtimes-and-future.md) |
| runtime 类型、service mode 和 adapter | `runtime_types`、`service_modes`、`runtime_adapters` | [运行时和未来能力](plugin-development/runtimes-and-future.md) |
| sandbox、WASM、ingress、instrumentation | `sandbox`、`wasm`、`ingress`、`instrumentation` fact blocks | [运行时和未来能力](plugin-development/runtimes-and-future.md) |

## 推荐阅读路径

新插件作者：

1. 阅读 [开发快速开始](plugin-development/getting-started.md)。
2. 根据需要选择 [扩展点开发](plugin-development/extension-points.md) 中的扩展点。
3. 按 [Manifest 和包格式](plugin-development/manifest-and-package.md) 补齐 metadata、capabilities、config schema 和资源声明。
4. 用 [工具链命令](plugin-development/toolchain.md) 和 [测试和 Conformance](plugin-development/testing-and-conformance.md) 形成可重复证据。
5. 提交发布前按 [发布治理和供应链](plugin-development/release-governance.md) 生成治理、供应链和 promotion 证据。

维护现有插件：

1. 先跑 `gateway plugin features` 确认当前 runtime、extension point 和命令事实源。
2. 用 `gateway plugin contract <target>` 找 manifest、配置和扩展点契约问题。
3. 用 `gateway plugin conformance <target>` 覆盖对应 fixture。
4. 变更高风险数据面逻辑时，补 `protocol-smoke`、drain、timeout、panic 和 rollback 场景。

未来 runtime 或隔离能力开发：

1. 先阅读 [运行时和未来能力](plugin-development/runtimes-and-future.md)。
2. 不要把 `go-plugin` 的进程内 `net.Conn` 契约直接映射到 sandbox/WASM。
3. 使用当前 `plugin features` 中的 maturity、future gate 和 unsupported reason 表达能力状态。
