# M1：当前数据面验收未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把当前真正可用的 `in-process go-plugin`、binary/source `.mcgp`、dialer mode 和 protocol-proxy 主路径做成可重复验收的上线基线。

## 未完成工作

1. 补真实 Minecraft smoke 矩阵：
   - 多协议版本 handshake/login/payload。
   - malformed varint、异常 packet length、半包、客户端提前关闭、后端提前关闭。
   - 慢速插件端读取、大包 backpressure、handler timeout 和 panic recover。
2. 固化 binary/source `.mcgp` 端到端 fixture：
   - upload、inspect、load、enable、disable、delete、restart recovery。
   - upstream-rewrite source build 成功后生成 binary artifact。
   - source build 失败不影响 active artifact。
3. 把示例插件接入固定验收：
   - `upstream-rewrite` 覆盖匹配 host、非匹配 `api.ErrPass` 和 source/binary package。
   - `mc-auth-proxy` 覆盖 reject、backend unavailable、accept 后端转发和低基数 auth event/metric。
4. 增加 dispatch 和 drain 断言：
   - enable/disable 不污染旧 dispatch table。
   - disable 后新连接恢复默认 route。
   - active proxy summary 能反映 draining、force close 和错误摘要。

## 验收

- `go test ./internal/pluginmanager ./cmd/gateway ./protocol/smoke` 通过。
- `GOWORK=off go test .` 在 `examples/plugins/mc-auth-proxy` 通过。
- 示例插件的 `plugin test` 和 `plugin build --type both` 命令可重复运行。
- 插件失败、panic、timeout、disable、restart recovery 都不破坏默认 route。

## 回滚边界

- 本阶段主要补测试和 fixture；发现主路径不稳定时应修复主路径，不降低验收标准。
