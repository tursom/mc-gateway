# mc-gateway

一个简易的 Minecraft 网关，通过 host 将客户端的流量转发到对应的后端 Minecraft 服务器。

## 配置

目前 mc-gateway 只支持读取当前目录的 `config.toml` 作为配置。在 `config.toml` 被修改时，可以自动加载并更新部分配置，以达到不停机修改配置的效果。支持热加载的配置有：

- hosts
- log
- KCP 的 data_shards 和 parity_Shards
- QUIC 的 application_protocols
- pid_file

### 顶层配置

| 配置     | 类型   | 备注     |
| -------- | ------ | -------- |
| pid_file | string | pid 文件 |

> pid_file 在非 windows 平台默认会写入 /var/run/mc-gateway.pid，
> 在 windows 平台默认不会写入任何文件

### hosts

hosts 使用期望的 host 做 key，转发的目的地址为 value。参考`config.example.toml`。默认的 fallback host 配置 key 为 `default`。

#### upstream

host 支持多种协议的上游服务器，包括 tcp、kcp、quic、websocket 等。只需要在原始地址前加对应的协议名称即可，如 `quic://127.0.0.1:8080`。支持的列表如下：

| 协议名称 | 前缀       | 备注                          |
| -------- | ---------- | ----------------------------- |
| tcp      | 无         | 原始的tcp连接                 |
| kcp      | kcp://     | 使用 kcp 协议连接到服务器     |
| quic     | quic://    | 使用 quic 协议连接到服务器    |
| haproxy  | haproxy:// | 使用 HAProxy 协议连接到服务器 |

> HAProxy 协议头会保存客户端的真实 ip，大部分支持 HAProxy 的 mod（或插件）都支持从协议头获取真实 ip，
> 这样服务端就能够获取到真实的客户端 ip了，以此兼容现有的 ban ip 或者统计等插件。

### log

| 配置  | 类型   | 备注     |
| ----- | ------ | -------- |
| level | Level  | 日志等级 |
| file  | string | 日志文件 |

> 日志适配 logrotate，可以使用 logrotate 进行日志分片、压缩等日常运维操作，参考配置：

```logrotate
/var/log/mc-gateway.log {
    copytruncate
    daily
    missingok
    rotate 14
    compress
    compresscmd /usr/bin/zstd
    compressext .zst
    compressoptions -T0 --long
    uncompresscmd /usr/bin/unzstd
    notifempty
    delaycompress
    dateext
    postrotate
        # 向程序发送 SIGHUP 信号
        # mc-gateway 默认会将当前进程的 pid 写入 /var/run/mc-gateway.pid
        if [ -f /var/run/mc-gateway.pid ]; then
            kill -SIGHUP $(cat /var/run/mc-gateway.pid)
        fi
    endscript
}
```

### tcp

| 配置   | 类型 | 备注     |
| ------ | ---- | -------- |
| enable | bool | 是否启用 |
| port   | int  | 端口     |

> 默认端口为 25565，与 Minecraft 服务端保持一致

### kcp

| 配置          | 类型 | 备注     |
| ------------- | ---- | -------- |
| enable        | bool | 是否启用 |
| port          | int  | 端口     |
| data_shards   | int  | 数据分片 |
| parity_Shards | int  | 校验分片 |

### quic

| 配置                  | 类型     | 备注         |
| --------------------- | -------- | ------------ |
| enable                | bool     | 是否启用     |
| port                  | int      | 端口         |
| application_protocols | []string | 应用协议列表 |

> application_protocols 只要客户端与服务端有一个能够对应上就可以成功连接
> 默认值为 ["minecraft", "quic", "raw", "h3"]

### websocket

| 配置   | 类型 | 备注     |
| ------ | ---- | -------- |
| enable | bool | 是否启用 |
| port   | int  | 端口     |
| path   | str  | 接口路径 |

> path 默认为 "/"，会对所有路径的请求进行处理
>
> port 默认为 25566
