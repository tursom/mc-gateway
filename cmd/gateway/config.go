// cmd/gateway/config.go 加载静态网关配置，并与管理数据库提供的运行态状态组合使用。

package main

import (
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/internal/gatewayconfig"
)

var (
	config         gatewayconfig.Config
	configLoadLock sync.Mutex
)

func loadConfig() error {
	configLoadLock.Lock()
	defer configLoadLock.Unlock()

	if err := initializeGatewayRuntime(); err != nil {
		return err
	}

	if err := loadLogger(); err != nil {
		return err
	}

	loadPlugins()

	return nil
}

func loadPluginConfig(cfg map[string]any, pluginCfg any) error {
	log.Info().
		Any("config", cfg).
		Msg("Loading plugin config")

	return gatewayconfig.DecodePluginConfig(cfg, pluginCfg)
}
