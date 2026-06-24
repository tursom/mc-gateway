# TCP 端口复用 Web 服务设计

## 背景

当前 gateway 的 Minecraft TCP 入口和 WebSocket/HTTP 入口分别监听端口：

- `runTcp` 使用 `net.Listen("tcp", :tcp.port)` 接收原始 Minecraft TCP 连接。
- `runWebSocket` 使用 `http.ListenAndServe(:websocket.port)` 接收 HTTP/WebSocket 连接。
- `handleRequest` 会读取连接的第一段数据，并通过 Minecraft 握手包里的 host 路由到上游。

因此当前不能把 `tcp.port` 和 `websocket.port` 配成同一个端口；两个监听器会竞争同一个 TCP 地址，后启动的服务会失败。

## 目标

在同一个 TCP 监听端口上同时提供：

- Minecraft 原始 TCP 转发服务。
- Web 服务，包括普通 HTTP 接口和 WebSocket upgrade。

入口连接根据首包内容自动分流：

- HTTP/WebSocket 首包进入 Web 服务。
- 其他首包进入现有 Minecraft TCP 转发流程。

## 非目标

- 不在本次设计中实现代码。
- 不改变 KCP、QUIC 的端口与协议行为。
- 不实现 HTTPS/TLS 复用。HTTPS 需要在 gateway 内终止 TLS，或由外部代理终止 TLS 后再转发明文 HTTP。
- 不改变现有 Minecraft host 路由规则。
- 不改变现有上游协议选择规则，例如 `quic://`、`kcp://`、`haproxy://`。

## 配置设计

不新增复用开关，复用行为由端口配置自动决定：

```toml
[tcp]
enable = true
port = 25565

[websocket]
enable = true
port = 25566
path = "/"
```

端口判定规则：

- `tcp.enable = true`、`websocket.enable = true` 且 `tcp.port == websocket.port` 时，自动进入端口复用模式。
- `tcp.enable = true`、`websocket.enable = true` 且 `tcp.port != websocket.port` 时，保持现有独立监听模式。
- 只有 `tcp.enable = true` 时，只启动 Minecraft TCP 服务。
- 只有 `websocket.enable = true` 时，只启动 Web 服务。
- 端口比较应使用默认值归一化后的结果，例如 `websocket.port = 0` 应先按当前默认值视为 `25566`。

自动复用模式下：

- Web 服务不再单独监听 `websocket.port`。
- Web 服务挂载到 TCP listener 的 HTTP 分流分支。
- 实际对外端口为 `tcp.port`。
- `websocket.port` 与 `tcp.port` 相同，作为触发自动复用的配置表达。

## 总体架构

复用模式下只有一个 TCP listener：

```text
net.Listen(:tcp.port)
        |
        v
Accept
        |
        v
读取首批字节
        |
        +-- HTTP/WebSocket
        |       -> HTTP channel listener
        |       -> http.Server.Serve
        |
        +-- Minecraft TCP
                -> 回放首批字节
                -> handleRequest
                -> mapToHost
                -> proxyConnections
```

关键点：首包只用于判断协议，不能被消费掉。分流后必须把已经读取的字节重新接回连接流。

## 协议识别规则

HTTP/WebSocket 都以 HTTP 请求开始，因此只需要识别 HTTP 方法前缀。

建议识别以下前缀：

- `GET `
- `POST `
- `HEAD `
- `PUT `
- `PATCH `
- `DELETE `
- `OPTIONS `
- `CONNECT `
- `TRACE `

WebSocket upgrade 请求通常是 `GET /path HTTP/1.1`，会自然进入 HTTP 分支，再由现有 WebSocket handler 处理。

未匹配 HTTP 方法前缀的连接全部进入 Minecraft TCP 分支。

## 首包读取与回放

新增一个包装连接，例如 `replayConn`：

```go
type replayConn struct {
    net.Conn
    reader io.Reader
}
```

创建时把已读取的首包和原始连接拼接：

```go
reader := io.MultiReader(bytes.NewReader(peeked), conn)
```

之后 `Read` 从 `reader` 读取，`Write`、`Close`、deadline、地址信息继续委托给底层 `net.Conn`。

这样 Minecraft 分支仍然可以使用现有 `handleRequest(replayConn)`，`mapToHost` 读到的内容和没有分流时一致。

## HTTP 接入方式

不建议手写 HTTP 解析。建议实现一个 channel listener：

```go
type chanListener struct {
    conns  chan net.Conn
    closed chan struct{}
    addr   net.Addr
}
```

行为：

- 主 TCP accept 循环识别到 HTTP 后，把 `replayConn` 投递到 `chanListener.conns`。
- `http.Server.Serve(chanListener)` 负责标准 HTTP/WebSocket 处理。
- 关闭 gateway 时关闭 `chanListener`，让 HTTP server 退出。

优点：

- 继续使用 Go 标准库 HTTP server。
- WebSocket upgrade 流程不需要重写。
- 可以复用现有 `handleWebSocket`。

## 运行流程

自动复用模式启动流程：

1. 加载配置。
2. 创建 Web handler 和 `http.Server`，但不调用 `ListenAndServe`。
3. 创建 TCP listener。
4. 启动 `http.Server.Serve(chanListener)`。
5. TCP accept 循环接收所有连接。
6. 每个连接设置 socket option。
7. 设置短读超时读取首批字节。
8. 根据首包分流到 Web 或 Minecraft。

非复用模式保持现有流程：

- TCP 服务继续由 `runTcp` 独立监听。
- WebSocket 服务继续由 `runWebSocket` 独立监听。

## 超时与错误处理

首包读取需要短超时，避免空连接或慢连接长期占用 goroutine。

建议策略：

- 首包读取超时：关闭连接并记录 debug 或 warn 日志。
- 首包为空：关闭连接。
- HTTP 分支投递失败：关闭连接。
- Minecraft 分支继续使用现有错误处理。

读超时只用于首包判断。分流完成后应清除 read deadline，避免影响长连接转发。

## 与现有热路径的关系

Minecraft TCP 分支应尽量保持现有转发路径：

- `handleRequest`
- `mapToHost`
- `proxyConnections`
- `copyForward`

复用逻辑只出现在 listener 和首包分流层，不进入双向转发热路径。这样可以避免破坏已有 TCP 转发优化。

## 测试计划

单元测试：

- HTTP 方法前缀识别。
- 非 HTTP 首包进入 Minecraft 分支。
- `replayConn` 能先读出已窥探字节，再读底层连接后续字节。
- 首包读取超时会关闭连接。
- `chanListener` 的 `Accept`、`Close` 行为。

集成测试：

- 复用模式下，同一端口可以访问 HTTP 接口。
- 复用模式下，同一端口可以完成 WebSocket upgrade。
- 复用模式下，Minecraft 握手包仍能被 `protocol.GetMcHost` 正确解析。
- 非复用模式下，现有 `tcp.port` 和 `websocket.port` 行为不变。
- `tcp.port == websocket.port` 时自动复用，不应出现两个 listener 竞争同一端口。

性能验证：

- 对 Minecraft TCP 转发路径跑现有 benchmark，确认复用层没有影响已建立连接后的转发性能。
- 至少覆盖 loopback TCP 场景，因为该项目的主要热路径是 TCP-to-TCP 转发。

## 风险与边界

- HTTP 识别只能覆盖明文 HTTP。TLS 握手首包不会匹配 HTTP 方法，会被送入 Minecraft 分支并失败。
- 某些非 HTTP 协议如果首包刚好以 `GET ` 等方法前缀开头，会被误判为 Web 请求；对 Minecraft Java 握手来说风险很低。
- 首包读取缓冲区不能太小，否则可能影响识别；但 HTTP 方法识别只需要很少字节。
- 复用模式改变 listener 所有权，需要注意进程退出时 TCP listener、HTTP server、channel listener 的关闭顺序。

## 实施步骤

1. 增加端口默认值归一化和启动模式判定逻辑。
2. 新增 HTTP 方法识别函数。
3. 新增 `replayConn`。
4. 新增 `chanListener`。
5. 拆分 WebSocket handler 注册逻辑，让自动复用模式和独立监听模式都能使用同一套 handler。
6. 新增自动复用模式的 TCP accept 分流入口。
7. 增加配置校验，避免启动两个 listener 监听同一 TCP 地址。
8. 补充单元测试、集成测试和 TCP benchmark 验证。

## 验收标准

- 当 `tcp.port == websocket.port` 且两个服务都启用时，只暴露该端口也能同时提供 Minecraft TCP 和 WebSocket/HTTP 服务。
- Minecraft 客户端连接、host 路由、上游转发行为与复用前一致。
- WebSocket 客户端通过同一端口能完成 upgrade 并进入现有 gateway 流程。
- 端口不同或只启用单个服务时，现有配置和行为不变。
- 测试覆盖首包识别、连接回放、HTTP 分流、Minecraft 分流和配置校验。
