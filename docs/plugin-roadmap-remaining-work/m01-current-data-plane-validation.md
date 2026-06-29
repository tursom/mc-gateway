# M1：当前数据面验收闭环

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把当前真正可用的 `in-process go-plugin`、binary/source `.mcgp`、dialer mode 和 protocol-proxy 主路径做成可重复验收的上线基线。

## 闭环工作

1. 真实 Minecraft smoke 矩阵：
   - 多协议版本 handshake/login/payload。
   - malformed varint、异常 packet length、半包、客户端提前关闭、后端提前关闭。
   - 慢速插件端读取、大包 backpressure、handler timeout 和 panic recover。
   - 证据：`protocol/smoke` 提供 packet/TCP fixture；`internal/pluginmanager` 覆盖 protocol-proxy 多版本、异常包、early close、backpressure、timeout、panic recover 和默认 route 恢复。
2. binary/source `.mcgp` 端到端 fixture：
   - upload、inspect、load、enable、disable、delete、restart recovery。
   - upstream-rewrite source build 成功后生成 binary artifact。
   - source build 失败不影响 active artifact。
   - 证据：`internal/pluginmanager` 覆盖 manager lifecycle、restart recovery、source build 成功/失败保护；`cmd/gateway` 覆盖 upstream-rewrite、mc-auth-proxy 示例 source/binary package，以及 upstream-rewrite source `.mcgp` 再构建 binary artifact。
3. 示例插件固定验收：
   - `upstream-rewrite` 覆盖匹配 host、非匹配 `api.ErrPass` 和 source/binary package。
   - `mc-auth-proxy` 覆盖 reject、backend unavailable、accept 后端转发和低基数 auth event/metric。
   - 证据：示例插件本地测试覆盖运行行为，`plugin test --profile manifest` 和 `plugin build --type both` 作为固定验收命令。
4. dispatch 和 drain 断言：
   - enable/disable 不污染旧 dispatch table。
   - disable 后新连接恢复默认 route。
   - active proxy summary 能反映 draining、force close 和错误摘要。
   - 证据：`internal/pluginmanager` 覆盖 dispatch snapshot 隔离、disable 后 pass-through、draining/force-close active proxy summary 和 `last_proxy_error` 摘要。

## 验收

- `go test ./internal/pluginmanager ./cmd/gateway ./protocol/smoke` 通过。
- `GOWORK=off go test .` 在 `examples/plugins/upstream-rewrite` 通过。
- `GOWORK=off go test .` 在 `examples/plugins/mc-auth-proxy` 通过。
- `go run ./cmd/gateway plugin test examples/plugins/upstream-rewrite --profile manifest` 通过。
- `go run ./cmd/gateway plugin build examples/plugins/upstream-rewrite --type both` 通过。
- `go run ./cmd/gateway plugin test examples/plugins/mc-auth-proxy --profile manifest` 通过。
- `go run ./cmd/gateway plugin build examples/plugins/mc-auth-proxy --type both` 通过。
- 插件失败、panic、timeout、disable、restart recovery 都不破坏默认 route。

## 回滚边界

- 本阶段主要补测试和 fixture；发现主路径不稳定时应修复主路径，不降低验收标准。
