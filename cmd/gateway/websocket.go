package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 允许所有来源的连接（生产环境中应该更严格）
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

	handleRequest(&webSocketConn{Conn: conn})
}

func (w *webSocketConn) Read(b []byte) (n int, err error) {
	for {
		if w.reader != nil {
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
			continue
		}
		w.reader = reader
	}
}

func (w *webSocketConn) Write(b []byte) (n int, err error) {
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

// 启动 WebSocket 服务器
func runWebSocket(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	path := config.WebSocket.Path
	if path == "" {
		path = "/" // 默认路径，全部处理
	}
	http.HandleFunc(path, handleWebSocket)

	port := config.WebSocket.Port
	if port == 0 {
		port = 25566 // 默认端口
	}

	log.Info().Int("port", port).Str("path", path).Msg("Starting WebSocket server")
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		log.Fatal().Err(err).Msg("Failed to start WebSocket server")
	}
}
