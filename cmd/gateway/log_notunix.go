// cmd/gateway/log_notunix.go 在没有 Unix 信号的平台上提供空的日志轮转信号钩子。

// pid_unix.go
//go:build !unix && !plan9

package main

import (
	"github.com/rs/zerolog/log"
)

func handleLogRotate() {
	// 非 Unix 平台不执行日志轮转信号处理。
	// 该平台不支持通过信号触发日志轮转。
	// 保留空实现是为了让跨平台调用点保持一致。
	log.Info().Msg("Log rotation is not supported on this platform")
}
