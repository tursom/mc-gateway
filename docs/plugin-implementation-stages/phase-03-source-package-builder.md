# 阶段 3：Source Package Builder

## 目标

支持 source `.mcgp`。管理员可以上传源码包，由受控 builder 构建出最终 `plugin.so` artifact，再进入阶段 1/2 已经可用的加载和启用流程。

本阶段解决开发者分发源码包、记录构建环境和产物 provenance 的问题。构建失败不得影响当前 active 插件。

## 可用性检查点

阶段结束时必须能做到：

- 上传 binary `.mcgp` 的路径不受影响。
- 上传 source `.mcgp` 后创建 build job。
- build 成功后生成新的 binary artifact，可 load/enable。
- build 失败只记录错误和日志摘要，不改变当前 active artifact。
- 管理员能看到 source sha256、builder、Go version、module list 和 artifact sha256。

## 范围

### Source 包格式

source `.mcgp` 必须包含：

- `manifest.json`
- `go.mod`
- build entry
- 源码文件

可选：

- `go.sum`
- `vendor/`
- README、LICENSE、SBOM

不执行包内任意 shell 脚本。构建命令由 gateway/builder 固定生成。

### Builder

支持两种 builder：

- `local-process`：开发模式。
- `container`：生产推荐。

生产默认推荐 container builder 或外部 CI。gateway 主进程不得直接执行 `go build`。

官方发布的 container builder image 必须绑定 gateway release、plugin API version、Go version、GOOS 和 GOARCH，并在 release 文档中只把 digest-pinned 引用作为生产推荐，例如 `ghcr.io/tursom/mc-gateway-plugin-builder:release-v0-1-0-plugin-api-v1-go1.24.4-linux-amd64@sha256:<digest>`。缺少 digest 或缺少 release/API/Go/平台绑定的 image 只能进入 warning/override，不得直接作为无提示生产准入。

官方 builder image 由 `.github/workflows/plugin-builder-image.yml` 发布。release 说明必须引用该 workflow 输出的 digest-pinned 文本 artifact；单独的 tag（即使包含 release/API/Go/平台）不能作为生产配置样例。

external CI binary artifact 的 `provenance.json` 必须包含签名、attestation、SBOM、source sha、artifact sha、CI run identity、builder identity 和 release provenance；缺失或 hash 不匹配时阻断 enable、rollback、repository apply 和 promotion apply。

固定构建维度：

- Go version。
- GOOS/GOARCH/GOAMD64/GOARM64。
- CGO。
- build tags。
- SDK module version。
- GOPROXY/GONOSUMDB/GOPRIVATE 策略。
- vendor required。

### Provenance

记录：

- source package sha256。
- artifact sha256。
- builder type/image/version。
- Go version。
- `go list -m -json all` 摘要。
- `go version -m` 摘要。
- ABI fingerprint。
- build log 摘要。
- build start/end/duration。

### GC

实现：

- source package 保留策略。
- build log 保留策略。
- artifact GC candidate。
- active/desired/snapshot referenced artifact 不可被 GC。

## 明确不做

- 不把源码构建等同于运行时沙箱。
- 不支持自定义构建脚本。
- 不强制签名。
- 不实现远程插件仓库。

## 实现任务

1. 扩展 `.mcgp` 校验支持 `artifact_type=source`。
2. 增加 `plugin_builds` 状态机。
3. 实现 build operation 和取消/重试。
4. 实现 local-process builder。
5. 实现 container builder 接口或预留适配。
6. 构建后执行 manifest ABI 校验。
7. 构建成功写入 `plugin_artifacts`。
8. 构建失败保存脱敏日志摘要。
9. 增加 build API/CLI。
10. 更新示例插件，支持 source package。

## 验收

- source upstream-rewrite 能构建并启用。
- source mc-auth-proxy 能构建或至少通过编译 fixture。
- builder Go version 不匹配时阻断启用或构建。
- 构建日志不包含 secret、环境 token 或完整私有路径。
- 构建失败不影响 active artifact。

## 回滚策略

- 构建产物只有 enable 后才影响流量。
- 构建失败或产物校验失败时保留旧 artifact。
- 如 builder 配置异常，可关闭 source package 构建，继续支持 binary `.mcgp`。
