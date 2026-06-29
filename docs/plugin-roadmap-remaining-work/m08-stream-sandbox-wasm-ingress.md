# M8：Stream、Sandbox、WASM 和 Ingress 未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

在 `go-plugin-process` drain-only 和 conformance 稳定后，实现真正跨进程 stream、隔离 runtime、低风险 WASM extension point 和 gateway-managed ingress listener。

## 未完成工作

1. 定义稳定 stream 协议：
   - `stream.proxy/v1` 或 `upstream.connect/v2` 明确 half-close、deadline、backpressure、cancel 和 byte accounting。
   - 协议需要 conformance fixture，不能只依赖内部 UDS relay 实现。
2. 实现 sandbox-process：
   - supervisor 和 control RPC。
   - filesystem、network、env、CPU、memory enforcement。
   - secret RPC/handle，默认不把 secret 注入进程环境。
   - crash loop policy 和诊断摘要。
3. 实现真实 WASM runtime：
   - 选择 wazero 或等价 runtime。
   - host ABI、module load/cache、fuel/time/memory limit。
   - 默认无文件、无网络。
   - 首批只允许 rule、route、config validate 等低风险 extension point。
4. 实现 ingress listener lifecycle：
   - gateway 创建 listener，插件不能任意监听端口。
   - TLS/secret refs 装载和脱敏。
   - health、disable、drain。
   - 内置 TCP/Admin、KCP、QUIC、WebSocket listener reservation 需要支持运行期刷新或明确需要重启。
5. 补 future runtime feature gate：
   - sandbox/WASM/ingress 必须可关闭。
   - required capability 无法强制时阻断启用。
   - reserved/schema-only 能力不能被 enable 成数据面。

## 验收

- required capability 无法强制时阻断启用。
- WASM timeout、trap、memory exceeded 只影响当前调用。
- sandbox 插件不能访问未授权 secret、network、files。
- `ingress.service/v1` 在 listener lifecycle 实现前只能通过 schema/contract/preflight 校验识别，不能启用。
- ingress disable 后停止接收新连接并 drain。

## 回滚边界

- sandbox、WASM、ingress 都必须 feature flag 或 service mode 可关闭。
- 任何 future runtime 故障不得影响 `in-process go-plugin`。
