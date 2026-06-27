// cmd/gateway/log_unix.go 注册 Unix 信号处理，让进程无需完整重启即可重新打开日志文件。

// pid_unix.go
//go:build unix || plan9

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
)

func handleLogRotate() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGHUP)

	for range signalChan {
		log.Info().Msg("Received SIGHUP, reopening log file")
		if err := reopenLogFile(); err != nil {
			log.Error().Err(err).Msg("Failed to reopen log file")
		}
	}
}
