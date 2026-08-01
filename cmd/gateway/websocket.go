// cmd/gateway/websocket.go 把 WebSocket 会话适配为 net.Conn，让浏览器客户端复用网关请求路径。

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 当前网关把 WebSocket 当作传输层入口，先允许所有来源；生产暴露时应在反向代理层收紧来源。
		return true
	},
}

type (
	// WebSocket 连接适配器，实现 net.Conn 接口
	webSocketConn struct {
		*websocket.Conn
		reader io.Reader
	}
)

// WebSocket 处理函数
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 升级 HTTP 连接为 WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Err(err).Msg("Failed to upgrade WebSocket connection")
		return
	}
	defer conn.Close()

	gatewayMetrics.WebSocketConnectionStarted()
	// WebSocket 连接包装为 net.Conn 后进入同一个 handleRequest，复用插件、路由和转发逻辑。
	handleRequest(&webSocketConn{Conn: conn})
}

func (w *webSocketConn) Read(b []byte) (n int, err error) {
	for {
		if w.reader != nil {
			// 当前消息帧没读完前持续从同一个 reader 读取，模拟流式 net.Conn。
			n, err = w.reader.Read(b)
			if errors.Is(err, io.EOF) {
				w.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}

		messageType, reader, err := w.NextReader()
		if err != nil {
			return 0, err
		}
		if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
			// 控制帧不进入 Minecraft 协议流。
			continue
		}
		w.reader = reader
	}
}

func (w *webSocketConn) Write(b []byte) (n int, err error) {
	// 每次 Write 输出一个二进制 WebSocket 消息，保持与 Minecraft packet 边界无关的字节流语义。
	writer, err := w.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}

	n, err = writer.Write(b)
	closeErr := writer.Close()
	if err != nil {
		return n, err
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n != len(b) {
		return n, io.ErrShortWrite
	}

	return n, nil
}

func (w *webSocketConn) SetDeadline(t time.Time) error {
	if err := w.SetReadDeadline(t); err != nil {
		return err
	}
	return w.SetWriteDeadline(t)
}

func newWebSocketHandler() http.Handler {
	mux := http.NewServeMux()
	// 路径来自运行态服务配置，允许管理端把 WebSocket 入口挂到子路径。
	mux.HandleFunc(normalizedWebSocketPath(), handleWebSocket)
	return mux
}

// 启动 WebSocket 服务器
func runWebSocket(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	port := normalizedWebSocketPort()
	path := normalizedWebSocketPath()

	log.Info().Int("port", port).Str("path", path).Msg("Starting WebSocket server")
	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: newWebSocketHandler()}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve WebSocket port %d: %w", port, err)
	}
	return nil
}
