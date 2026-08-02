> **Archived:** This document records the superseded pre-v2 plugin design. Current behavior is defined by `docs/plugin-development/extension-points.md`.

# 插件开发工具链设计

本文定义插件开发工具链的功能需求和实现边界。目标是让插件作者从新建、开发、测试、打包到发布前检查都使用同一套 `gateway plugin` CLI，而不是在每个示例插件里维护重复脚本。

本设计以 [plugin-system-design.md](plugin-system-design.md) 和 [plugin-implementation-plan.md](plugin-implementation-plan.md) 为上游约束。插件作者只维护一个 manifest source 文件，支持 `manifest.yaml`、`manifest.yml`、`manifest.toml`、`manifest.jsonc` 或 `manifest.json`；`.mcgp` 包内仍统一物化为 `manifest.json`。Go 代码中不再维护 `manifestJSON` 或等价重复元数据。

## 目标

- 提供 `gateway plugin init/build/test` 三个核心开发入口。
- 让示例插件和第三方插件使用同一套构建、打包、校验和测试流程。
- 支持 binary `.mcgp` 和 source `.mcgp`，并逐步替代示例插件内的 `build.sh`、`cmd/render-manifest` 等重复逻辑。
- 保持工具链 runtime-neutral：Go plugin 是第一批实现目标，后续 `go-plugin-process`、`sandbox-process`、WASM 和 ingress service 通过 runtime adapter 扩展。
- 保证 CLI 产物可被 Admin/API 的服务端校验重复验证；CLI 只是开发体验和预检工具，不是信任边界。
- 产物尽量稳定可复现：相同输入、相同 builder 和相同环境生成相同 zip 排序、权限和摘要。

## 非目标

- 不引入 `gateway plugin dev ...` 命名空间；开发命令直接扩展在 `gateway plugin` 下。
- 不恢复代码内 manifest 元数据。
- 不支持插件自定义构建脚本作为默认路径。
- 不把 source build 当成 runtime sandbox。
- 不在第一版支持远程插件市场、签名分发或自动升级。
- 不承诺 Go plugin 真正热卸载；本地调试仍遵守运行时限制。

## 设计决策

| 决策 | 结论 |
| --- | --- |
| CLI 命名 | 直接扩展 `gateway plugin init/build/test`，不新增 `dev` 子命名空间 |
| 元数据来源 | 插件目录只允许一个人工维护的 manifest source；包内可信元数据统一为 canonical `manifest.json` |
| 打包入口 | `gateway plugin build` 同时承担 build 和 package，不再要求插件目录自带 zip 脚本 |
| 示例插件 | `upstream-rewrite` 和 `mc-auth-proxy` 迁移到标准 CLI，删除重复 `build.sh` 和 `render-manifest` 逻辑 |
| runtime 扩展 | CLI 通过 runtime build/test adapter 分发逻辑，命令名不随 runtime 改变 |
| 校验边界 | CLI 校验不能替代 gateway 服务端上传、构建、准入和 enable 校验 |
| source manifest | 源码目录中的 `manifest.yaml/yml/toml/jsonc/json` 是作者输入；artifact 包内的 `manifest.json` 是构建时物化结果，不作为第二份人工维护数据 |

## 命令总览

第一版重点实现：

| 命令 | 用途 |
| --- | --- |
| `gateway plugin init <dir>` | 生成插件模板 |
| `gateway plugin build [dir]` | 构建并打包 binary/source `.mcgp` |
| `gateway plugin test [dir]` | 运行插件单元测试和 harness 测试 |
| `gateway plugin validate <path>` | 校验 manifest、源码目录或 `.mcgp` 包 |
| `gateway plugin inspect <artifact.mcgp>` | 查看包内 manifest 和摘要 |
| `gateway plugin compat <artifact.mcgp>` | 检查当前 gateway 对 artifact 的兼容性 |

现有 `gateway plugin source-build <source.mcgp> [out.mcgp]` 保留为兼容命令。后续可以由 `gateway plugin build --from-source <source.mcgp> --out <out.mcgp>` 覆盖同等能力，再把 `source-build` 标记为兼容别名。

所有面向 CI 的命令都应支持：

- `--json`：输出机器可读结果。
- `--quiet`：只输出错误或关键产物路径。
- `--out <path>`：指定产物或报告位置。
- 稳定退出码：参数错误、校验失败、构建失败和测试失败应可区分。

## 完整功能域

工具链最终需要覆盖从插件作者到生产运维的完整闭环。下表是功能需求清单，阶段表示推荐落地顺序，不代表命令只能在该阶段出现。

| 功能域 | 需要解决的问题 | 关键命令 |
| --- | --- | --- |
| 项目脚手架 | 快速生成可构建、可测试、manifest 正确的插件目录 | `init` |
| Manifest 编辑 | 发现字段错误、解释支持能力、避免人工维护环境字段 | `validate`、`manifest format`、`manifest explain`、`features` |
| 构建和打包 | 统一 binary/source `.mcgp` 产物，替代示例脚本 | `build`、`clean` |
| Source 构建复现 | 在本地或 CI 复现 gateway builder 行为 | `build --from-source` |
| 单元和契约测试 | 在真实上传前验证 SDK、extension point 和 fixture | `test`、`conformance` |
| 本地安装调试 | 把产物上传到开发 gateway，启用、禁用、回滚和查看状态 | `upload`、`enable`、`disable`、`rollback`、`status` |
| 配置和 secret 预检 | 在启用前验证 config schema、secret ref、reload 兼容性 | `config validate`、`secret check`、`preflight` |
| 发布门禁 | 生成能进入 review/CI 的证据 | `preflight`、`self-test`、`benchmark` |
| 观测诊断 | 收集插件日志、事件、指标、trace 和诊断包 | `logs`、`events`、`metrics`、`diagnose` |
| 后台任务 | 开发和运维手动触发任务、查看执行状态 | `task list`、`task run`、`task cancel` |
| 数据和文件 | 查看 plugin_data/runtime files 配额、导出可迁移数据、GC | `data inspect/export/gc`、`files inspect/export/gc` |
| Promotion | 跨环境导入导出、diff、drift 和灾备演练 | `export`、`import`、`diff`、`drift`、`dr-drill` |
| 仓库和供应链 | 导入仓库候选、验证 SBOM/license/signature/advisory/vulnerability | `repo`、`sbom`、`sign`、`verify`、`advisory`、`vulnerability` |
| SDK 和契约治理 | 发布前检查 SDK/API/manifest/错误码兼容性 | `contract check`、`schema export`、`conformance` |
| Runtime 扩展 | 让新 runtime 复用同一套 init/build/test/validate 命令 | runtime adapter、`runtime features` |

### 命令分层

为了避免第一版实现过大，命令按层交付：

| 层级 | 阶段 | 命令 | 说明 |
| --- | --- | --- | --- |
| 0 | 已有能力 | `inspect`、`validate`、`compat`、`source-validate`、`source-build` | 当前 CLI 基线，后续保持兼容 |
| 1 | 阶段 1-3 | `init`、`build`、`test`、`features`、`manifest format/explain` | 插件作者日常开发闭环 |
| 2 | 阶段 4 | `upload`、`enable`、`disable`、`rollback`、`status`、`config validate`、`secret check` | 本地开发 gateway 和 Admin API 操作闭环 |
| 3 | 阶段 5 | `preflight`、`self-test`、`benchmark`、`review status`、`advisory scan`、`vulnerability scan` | 发布治理和准入证据 |
| 4 | 阶段 6 | `logs`、`events`、`metrics`、`diagnose`、`task`、`data`、`files`、`gc` | 运行诊断、后台任务、数据和资源治理 |
| 5 | 阶段 7-8 | `repo`、`sbom`、`sign`、`verify`、`contract`、`conformance`、`export/import/diff/drift/dr-drill` | 生态、供应链、跨环境发布和未来 runtime |

第一版不必一次实现所有命令，但设计上要避免把能力做进一次性脚本。每个命令都应能输出 JSON 报告，方便 CI 和 Admin API 复用。

## 开发工作流

工具链需要支持这些端到端流程。

### 新插件开发

```sh
gateway plugin init ./my-plugin --id my-plugin --template takeover --module example.com/my-plugin
cd ./my-plugin
gateway plugin validate .
gateway plugin test .
gateway plugin build . --type both
gateway plugin compat dist/my-plugin.mcgp
```

完成标准：

- 不需要手写 zip 命令。
- 不需要手写 `render-manifest`。
- 不需要在 Go 代码中声明 manifest 元数据。
- 默认模板生成 `manifest.yaml`；如需其它格式可使用 `gateway plugin init --manifest-format yaml|toml|jsonc|json`。

### 本地调试

```sh
gateway plugin build . --type binary
gateway plugin upload dist/my-plugin.mcgp --gateway http://127.0.0.1:8080
gateway plugin enable my-plugin --config testdata/config.json --profile dev
gateway plugin status my-plugin
gateway plugin logs my-plugin --tail 100
gateway plugin disable my-plugin
```

本地调试命令通过 Admin API 工作，不绕过服务端校验。需要认证时使用现有 Admin session/token 机制；CLI 不保存 secret 明文。

### CI 发布检查

```sh
gateway plugin validate .
gateway plugin test . --profile unit,manifest,harness,protocol-smoke
gateway plugin build . --type both --json --out dist/build-report.json
gateway plugin compat dist/my-plugin.mcgp --json --out dist/compat-report.json
gateway plugin preflight dist/my-plugin.mcgp --config config/prod.json --profile prod --json
gateway plugin benchmark dist/my-plugin.mcgp --profile ci-contract --json
```

CI 报告必须能作为 review 证据保存，并包含 artifact sha256、source sha256、SDK/API 版本、runtime、extension points、config hash、测试 profile 和失败原因。

### Source 包复现

```sh
gateway plugin build . --type source
gateway plugin build --from-source dist/my-plugin-source.mcgp --out dist/my-plugin-rebuilt.mcgp
gateway plugin compat dist/my-plugin-rebuilt.mcgp
```

该流程用于验证源码包能被受控 builder 重建，且构建失败不会影响 active artifact。

### 跨环境发布

```sh
gateway plugin export my-plugin --profile staging --out promotion.json
gateway plugin diff promotion.json --target prod
gateway plugin import promotion.json --target prod --dry-run
gateway plugin drift --baseline promotion.json --target prod
```

promotion bundle 默认不包含 secret 明文、secret 密文和 runtime state。缺失 secret mapping、runtime 不兼容、advisory 命中或策略阻断时必须失败。

## `gateway plugin init`

`init` 负责生成一个可直接构建和测试的插件目录。

### 输入

推荐参数：

| 参数 | 说明 |
| --- | --- |
| `--id <id>` | 插件 ID，必须满足 manifest 命名规则 |
| `--name <name>` | 展示名，默认由 ID 派生 |
| `--template <name>` | 模板名 |
| `--runtime <type>` | runtime 类型，默认 `go-plugin` |
| `--module <module>` | Go module path，Go runtime 模板必填或由目录推导 |
| `--extension <key>` | 目标 extension point |

第一批模板：

| 模板 | runtime | extension point | 说明 |
| --- | --- | --- | --- |
| `takeover` | `go-plugin` | `upstream.connect/v2` | 最小客户端连接接管模板 |
| `empty-go` | `go-plugin` | 无默认 handler | 用于自定义实验 |

预留模板：

| 模板 | runtime | 说明 |
| --- | --- | --- |
| `wasm-rule` | `wasm` | 未来 rule/config validate 类轻量插件 |
| `sandbox-process` | `sandbox-process` | 未来隔离进程插件 |
| `ingress-service` | `sandbox-process` 或专用 runtime | 未来入口服务插件 |

### 输出目录

Go plugin 模板应至少生成：

- `manifest.yaml`（默认；也支持 `manifest.yml`、`manifest.toml`、`manifest.jsonc`、`manifest.json`）
- `go.mod`
- `main.go`
- `main_test.go`
- `README.md`
- `testdata/config.json`
- `testdata/fixtures/`，按模板放置 harness 输入

生成的 manifest source 只包含作者应该维护的字段。`go_version`、`go_os`、`go_arch` 等环境相关字段可以为空或使用文档化占位；`build` 时再物化到 artifact manifest。

## `gateway plugin build`

`build` 是统一构建和打包入口。

### 常用模式

| 命令 | 结果 |
| --- | --- |
| `gateway plugin build .` | 默认生成 binary `.mcgp` |
| `gateway plugin build . --type binary` | 生成 binary `.mcgp` |
| `gateway plugin build . --type source` | 生成 source `.mcgp` |
| `gateway plugin build . --type both` | 同时生成 binary 和 source `.mcgp` |
| `gateway plugin build --from-source source.mcgp --out built.mcgp` | 使用 gateway builder 从 source 包生成 binary 包 |

当源码目录内存在多个 `manifest.*` 文件，`build` 必须通过 `--manifest <path>` 显式选择源文件；同一规则也适用于 `test`、`validate`、`preflight`、`self-test`、`benchmark` 和 `manifest format`。

推荐默认输出：

- `dist/<plugin-id>.mcgp`
- `dist/<plugin-id>-source.mcgp`
- `dist/<plugin-id>-built.mcgp`
- `dist/build-report.json`

### Manifest 物化规则

源码目录中只能存在一个 manifest source 文件。`build` 读取 `manifest.yaml/yml/toml/jsonc/json` 后在内存中生成 artifact manifest，并写入 `.mcgp` 包内的 canonical `manifest.json`：

- `artifact_type` 按 `--type` 写为 `binary` 或 `source`。
- binary 包写入 `runtime.entry=plugin.so`。
- Go plugin binary 包写入实际 `go_version`、`go_os`、`go_arch`。
- source 包写入 `build.type=go`、`build.entry`、`build.output`、`build.tags` 和 vendor 策略。
- 构建 provenance、module summary、artifact sha256 等写入 build report 或服务端 build record，不要求回写源码目录的 manifest source。

这保证源码仓库里没有第二份需要维护的 manifest，也避免 manifest source 与 Go 代码常量不一致。

如果目录中同时存在多个 `manifest.*` 文件，CLI 必须失败并要求传入 `--manifest <path>` 显式选择，避免不同格式的 manifest 分叉。`gateway plugin manifest format --canonical-json --type binary|source` 可查看最终写入对应 `.mcgp` 的规范 JSON；不传 `--type` 时使用 manifest source 中的 `artifact_type`，缺省按 binary 处理。`--write` 对 YAML/TOML/JSONC 必须保留注释，无法保留时不能覆盖源文件。

### Go Plugin Adapter

第一版 `go-plugin` build adapter 负责：

1. 读取并校验唯一 manifest source，或通过 `--manifest` 指定的 manifest source。
2. 运行 `go test ./...`，除非传入 `--skip-tests`。
3. 用固定命令构建 `plugin.so`：`go build -buildmode=plugin -trimpath -buildvcs=false`。
4. 用 `go tool nm` 校验 `Plugin` 符号。
5. 生成稳定 zip：固定 entry 排序、权限、时间戳策略和路径分隔符。
6. 生成 source `.mcgp` 时只包含允许的源码、`go.mod`、可选 `go.sum/vendor`、README、LICENSE、SBOM 和测试 fixture。
7. 输出 artifact sha256、source sha256、Go/API/SDK 版本和 ABI fingerprint。

第一版不执行包内脚本。未来如果需要复杂构建，应通过受控 builder profile 或外部 CI，而不是让插件包携带任意 shell 脚本。

### Runtime Adapter 预留

CLI 内部应抽象 build adapter：

```go
type PluginBuildAdapter interface {
    RuntimeType() string
    ValidateSource(ctx context.Context, req BuildCLIRequest) error
    Build(ctx context.Context, req BuildCLIRequest) (BuildCLIResult, error)
    PackageSource(ctx context.Context, req BuildCLIRequest) (BuildCLIResult, error)
}
```

预留 runtime 行为：

| runtime | build 产物 | source 包 | 测试方式 |
| --- | --- | --- | --- |
| `go-plugin` | `plugin.so` | Go module source | Go test + extension harness |
| `go-plugin-process` | `plugin.so` 或 host bundle | Go module source | 子进程 host harness |
| `sandbox-process` | executable 或 bundle | 受控源码/二进制 bundle | control RPC harness |
| `wasm` | `plugin.wasm` | WASM source/bundle | WASM host ABI harness |
| `builtin` | 无外部 artifact | 不适用 | gateway 内部测试 |

命令层不应写死 Go plugin 细节。新增 runtime 时只新增 adapter、manifest 校验和 harness，不新增一套用户命令。

## `gateway plugin test`

`test` 负责把插件作者的本地测试和 gateway extension contract 连接起来。

### 测试 profile

| Profile | 说明 |
| --- | --- |
| `unit` | 运行插件目录原生测试，例如 `go test ./...` |
| `manifest` | 校验 manifest schema、命名、runtime、extension point 和 config schema |
| `harness` | 运行 extension point fixture |
| `protocol-smoke` | 运行 Minecraft handshake/login smoke fixture |
| `conformance` | 运行当前 gateway 公开契约兼容测试 |

常用命令：

| 命令 | 结果 |
| --- | --- |
| `gateway plugin test .` | 运行模板默认 profile |
| `gateway plugin test . --profile unit,harness` | 运行指定 profile |
| `gateway plugin test . --config testdata/config.json` | 使用指定配置测试 |
| `gateway plugin test . --fixture testdata/fixtures/login-reject.json` | 使用指定 fixture |
| `gateway plugin test dist/plugin.mcgp --profile compat` | 对已打包 artifact 做兼容测试 |

### Harness 范围

第一版 harness 覆盖：

- `route.resolve/v1` provider：匹配 host、返回 override/pass/reject 决策并验证错误传播。
- `upstream.connect/v2` takeover：验证 Next/Core、替换流字节完整性、handshake/login packet、拒绝、panic、取消和半关闭。
- config：`ReloadConfig()` 成功、失败、默认值和 schema 校验。
- lifecycle：`Init()`、`Destroy()` 幂等、handler timeout、panic recover。

未来 runtime harness：

- `go-plugin-process`：通过 plugin-host 启动插件，验证 drain-only、crash loop 和 control channel。
- `sandbox-process`：验证 capability enforcement、secret handle、filesystem/network policy。
- `wasm`：验证 host ABI、memory/time limit、无授权文件和网络访问。
- `ingress.service/v1`：验证 listener 由 gateway 创建、端口冲突和 disable drain。

## `gateway plugin validate`

`validate` 应支持三类输入：

- manifest source 文件：`manifest.yaml`、`manifest.yml`、`manifest.toml`、`manifest.jsonc` 或 `manifest.json`
- 插件源码目录
- `.mcgp` artifact

校验内容：

- manifest schema 和必填字段。
- runtime type、runtime entry、build entry。
- extension point key、type 和 mode。
- config schema JSON。
- secret、event、metric、background task、external dependency、data store 和 file store 命名。
- binary/source 包结构、zip slip、大小限制和允许文件。
- 当前 gateway feature support。

对于源码目录，`validate` 不能执行插件代码；最多做静态文件、manifest 和包结构检查。需要运行代码的检查放在 `test` 或 `build`。

## 本地 Admin 操作命令

阶段 4 后，CLI 应能操作开发或测试环境的 Admin API，形成不依赖页面的调试闭环。

| 命令 | 职责 |
| --- | --- |
| `gateway plugin upload <artifact.mcgp>` | 上传 artifact/source package，返回 artifact ID、sha256 和校验摘要 |
| `gateway plugin status [plugin-id]` | 展示 desired/runtime state、active/desired/loaded artifact、recent error 和 restart required |
| `gateway plugin enable <plugin-id>` | 设置 desired enabled，支持 `--artifact`、`--config`、`--profile`、`--priority` |
| `gateway plugin disable <plugin-id>` | 设置 desired disabled，connection takeover 连接按策略 drain 或 force close |
| `gateway plugin delete <plugin-id>` | 删除 desired state 或 artifact，支持保留/删除数据选项 |
| `gateway plugin rollback <plugin-id>` | 回滚 artifact 或 config snapshot，并重新执行当前基础门禁 |
| `gateway plugin config validate <plugin-id>` | 校验 config JSON、schema、secret ref 和 `ReloadConfig()` dry-run |
| `gateway plugin secret check <plugin-id>` | 检查 manifest 必需 secret、secret ref、版本和 reload/rotation 状态 |

这些命令必须通过 Admin API 执行，并复用服务端权限、审计和错误码。CLI 不直接写 SQLite，不直接操作 artifact store，也不能绕过上传时的 zip/manifest 校验。

## 发布治理命令

阶段 5 后，CLI 需要生成和读取生产准入证据。

| 命令 | 职责 |
| --- | --- |
| `gateway plugin preflight` | 运行 config、secret、feature、runtime limits、scope/rollout、conflict 和 Minecraft capability 检查 |
| `gateway plugin self-test` | 运行插件实现的 quick/protocol-smoke/integration profile，保存脱敏证据 |
| `gateway plugin benchmark` | 记录或执行 benchmark profile，输出 P95/P99、error rate、capacity 和 baseline diff |
| `gateway plugin review status` | 查看当前 artifact/config/scope/risk/policy hash 是否已有有效 review |
| `gateway plugin advisory scan/rescan` | 查看安全公告，或按 artifact sha256、plugin/version、SBOM dependency 重新扫描本地 artifact |
| `gateway plugin vulnerability scan/rescan` | 查看本地漏洞库，或按 SBOM dependency 重新扫描本地 artifact |

发布治理命令的 JSON 报告必须包含稳定 `code`、`severity`、`message`、`evidence_id` 和相关 hash，不能要求 CI 解析人类可读文本。

## 观测和运维命令

阶段 6 后，CLI 应覆盖插件出问题时的定位、证据导出和资源清理。

| 命令 | 职责 |
| --- | --- |
| `gateway plugin logs <plugin-id>` | 查看插件日志摘要，支持 tail、时间范围、trace ID 和脱敏 |
| `gateway plugin events <plugin-id>` | 查看插件业务事件、drop/dead-letter 摘要和 replay/drop 操作 |
| `gateway plugin metrics <plugin-id>` | 查看 handler calls、duration、panic、connection sessions 和 custom metrics |
| `gateway plugin diagnose <plugin-id>` | 生成诊断包，包含 manifest、state、recent logs/events/metrics/build summary，不含 secret 明文 |
| `gateway plugin task list/run/cancel <plugin-id>` | 查看、手动触发或取消 background task |
| `gateway plugin external list/health-check <plugin-id>` | 查看外部依赖状态，或触发单个声明依赖的受控健康检查 |
| `gateway plugin data inspect/export/gc <plugin-id>` | 查看 plugin_data schema/data class/quota，导出可迁移数据，执行 dry-run 或清理 |
| `gateway plugin files inspect/export/gc <plugin-id>` | 查看 runtime files/resources/cache/tmp/log/diagnostic 用量和 GC candidate |
| `gateway plugin gc --dry-run` | 汇总 artifact、build log、diagnostic、plugin_data 和 runtime files 的可清理对象 |

所有清理命令默认 dry-run；实际删除必须显式传入确认参数，并写审计。数据导出只允许 manifest 声明 `exportable=true` 且调用者有权限的数据。

## 仓库、供应链和签名命令

阶段 8 的分发能力不能绕过本地 review 和 enable 流程。

| 命令 | 职责 |
| --- | --- |
| `gateway plugin repo list/search/show` | 查看 official/internal/file/url repository 中的候选版本 |
| `gateway plugin repo import` | 下载或导入候选 artifact 到本地 store，只生成 local artifact，不自动启用 |
| `gateway plugin sbom generate/verify` | 生成或验证 SBOM，供 advisory/license 策略使用 |
| `gateway plugin sign verify/key-rotation/revoke` | 验证 artifact 签名，维护本地 trust store 并吊销不可信 key |
| `gateway plugin verify` | 验证 signature、sha256、SBOM、license 和 provenance |
| `gateway plugin advisory import/sync/scan/rescan` | 导入单条安全公告、本地 JSON feed 或外部 feed URL，重新扫描本地 artifact |
| `gateway plugin vulnerability import/sync/scan/rescan` | 导入单条漏洞记录、本地 JSON 漏洞库或外部 feed URL，按 SBOM dependency 重新扫描本地 artifact |

仓库删除、远端更新或签名失败都不能自动改变本地 active artifact。repository import 之后仍要走 validate、compat、preflight、review 和 enable。

`gateway plugin verify` / `gateway plugin sbom verify` 提交的 `metadata-json` 支持本地 license policy：`license_policy.allowed`、`license_policy.denied`、`license_policy.review_required`、`license_policy.allow_unknown` 和 `license_policy.apply_to_transitive`。服务端会从 artifact/manifest/metadata 的 license 字段计算 `denylist_matches`、`allowlist_missing`、`review_required_matches` 或 `unknown`，再进入 supply-chain gate。

`gateway plugin advisory import --metadata-json` 可以提交单条 advisory，也可以提交 `{"source":"local-json","advisories":[...]}` 形式的本地 feed。`gateway plugin advisory sync --feed-url ...` 由 Admin 端拉取 `http`、`https` 或 `file` feed。feed 导入后会触发 rescan；`quarantine` 或 `revoke` 命中 active artifact 时会移出 dispatch 并进入 drain。

`gateway plugin vulnerability import --metadata-json` 可以提交单条 vulnerability，也可以提交 `{"source":"local-json","vulnerabilities":[...]}` 形式的本地漏洞库。`gateway plugin vulnerability sync --feed-url ...` 使用同一条服务端拉取和导入路径；部署方也可以通过 `pluginmanager.Options` 配置只在显式启用时运行的 feed scheduler。导入后会触发本地 rescan；命中 manifest SBOM dependency 的 `denylist`、`quarantine` 或 `revoke` 会进入治理门禁，其中 quarantine/revoke 命中 active artifact 时会移出 dispatch 并进入 drain。完整外部 CVE 自动扫描链仍是后续能力。

## 契约和 SDK 命令

插件系统公开 API 后，CLI 还要服务 gateway release 过程。

| 命令 | 职责 |
| --- | --- |
| `gateway plugin features` | 输出当前 gateway 支持的 runtime、extension point、manifest field、feature key 和版本 |
| `gateway plugin schema export` | 导出 manifest JSON schema、config UI hint schema 和 extension fixture schema |
| `gateway plugin contract check` | 对比上一 release 的 SDK/API/manifest/error code/CLI JSON 输出兼容性 |
| `gateway plugin conformance` | 构建示例插件，运行 source/binary fixture 和 Admin/CLI golden test |

`features` 输出必须和 Admin API 使用同一契约。`contract check` 和 `conformance` 失败应被视为 gateway release 风险，不是普通文档错误。

## Runtime 扩展命令

新增 runtime 不应增加一套平行 CLI。`init/build/test/validate/compat/preflight` 必须根据 `manifest.runtime.type` 选择 adapter。

| runtime | 额外 CLI 需求 |
| --- | --- |
| `go-plugin-process` | `test` 能启动 plugin-host harness；`preflight` 检查 migration mode、safe point、drain-only/fd-live 声明 |
| `sandbox-process` | `validate/preflight` 检查 capability、secret handle、filesystem/network/env/cpu/memory policy；`test` 验证 control RPC 和 crash loop |
| `wasm` | `build` 生成 `plugin.wasm`；`test` 使用 WASM host ABI；`preflight` 检查 memory/time/no file/no network |
| `ingress.service/v1` | `preflight` 检查 listener ownership、port conflict、TLS/secret refs 和 disable drain |
| build-time instrumentation | 不进入 runtime plugin enable/disable；CLI 只提供 manifest/provenance/conformance/benchmark/smoke 证据 |

如果目标 gateway 不支持某 runtime，`compat` 和 `preflight` 必须返回明确的 blocking code，而不是降级为 Go plugin 尝试加载。

## 发布前检查

发布前推荐流程：

1. `gateway plugin validate .`
2. `gateway plugin test . --profile unit,manifest,harness`
3. `gateway plugin build . --type both`
4. `gateway plugin validate dist/<plugin-id>.mcgp`
5. `gateway plugin compat dist/<plugin-id>.mcgp`
6. 可选：`gateway plugin build --from-source dist/<plugin-id>-source.mcgp --out dist/<plugin-id>-rebuilt.mcgp`
7. 可选：`gateway plugin test dist/<plugin-id>.mcgp --profile conformance`

CI 产物应至少保存：

- binary `.mcgp`
- source `.mcgp`
- build report JSON
- test report JSON
- artifact sha256 和 source sha256

## 示例插件迁移

`examples/plugins/upstream-rewrite` 和 `examples/plugins/mc-auth-proxy` 迁移目标：

- README 使用 `gateway plugin build . --type both`。
- README 使用 `gateway plugin test .`。
- 删除或降级 `build.sh` 为兼容包装；最终不再作为主路径。
- 删除 `cmd/render-manifest`，由 CLI 根据源码 manifest source 生成 artifact manifest。
- 示例插件的测试 fixture 进入 `testdata/fixtures/`。
- 示例插件进入 conformance suite；构建失败视为插件 API 回归。

迁移时必须保留现有 `.mcgp` 格式：binary 包仍包含 `manifest.json` 和 `plugin.so`；source 包仍包含 `manifest.json`、`go.mod`、build entry 和源码。

## 实现顺序

建议按以下顺序实现：

1. 增加 `gateway plugin init`，生成 `takeover` Go 模板。
2. 增加 `gateway plugin build` 的 Go plugin binary/source 打包能力，复用现有 artifact 校验逻辑。
3. 用 `gateway plugin build` 替换示例插件 `build.sh` 和 `cmd/render-manifest` 主路径。
4. 增加 `gateway plugin test` 的 unit、manifest 和 upstream harness profile。
5. 将 `source-build` 能力收敛为 `build --from-source`，保留兼容别名。
6. 增加 runtime build/test adapter 接口，为 `go-plugin-process`、`sandbox-process` 和 WASM 实现预留扩展点。
7. 增加 JSON report、conformance profile 和 CI golden 输出。

每一步结束时，现有 `inspect/validate/compat/source-validate/source-build` 不能回归。

## 验收标准

- 新建 `takeover` 模板后，不手写额外脚本即可 build/test/validate。
- 新建 `connection takeover` 模板后，能跑通 Minecraft handshake/login smoke fixture。
- `upstream-rewrite` 和 `mc-auth-proxy` 示例插件使用标准 CLI 生成 binary/source `.mcgp`。
- 生成的 `.mcgp` 能通过现有上传和服务端校验。
- manifest source 与 Go 代码不重复维护插件元数据。
- Go plugin adapter 之外的 runtime 可以通过 adapter 注册进入同一套 `init/build/test` 命令。
- CLI 失败输出能定位到字段、文件或 fixture，而不是只返回通用错误。
