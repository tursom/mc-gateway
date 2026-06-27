// cmd/gateway/log.go 配置网关日志、日志文件、日志级别和日志轮转钩子。

package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	currentLogFile string
)

func loadLogger() error {
	if len(config.Log.Level) == 0 {
		config.Log.Level = "info"
	}

	level, err := zerolog.ParseLevel(config.Log.Level)
	if err != nil {
		return err
	}

	log.Logger = log.Logger.Level(level)

	if config.Log.File != "" && currentLogFile != config.Log.File {
		if err := os.MkdirAll(filepath.Dir(config.Log.File), 0755); err != nil {
			return err
		}

		logFile, err := os.OpenFile(config.Log.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return err
		}

		log.Logger = log.Logger.Output(io.MultiWriter(os.Stdout, logFile))

		currentLogFile = config.Log.File
	}

	return nil
}

func reopenLogFile() error {
	configLoadLock.Lock()
	defer configLoadLock.Unlock()

	if config.Log.File == "" {
		return nil
	}

	// 重新打开日志文件
	logFile, err := os.OpenFile(config.Log.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	log.Logger = log.Logger.Output(io.MultiWriter(os.Stdout, logFile))
	currentLogFile = config.Log.File

	return nil
}
