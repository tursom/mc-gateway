package main

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/mitchellh/mapstructure"
	"github.com/rs/zerolog/log"
)

var (
	configFile = "config.toml"

	config         Config
	configLoadLock sync.Mutex
)

type (
	Config struct {
		Tcp       ProtocolConfig            `toml:"tcp"`
		Quic      QuicConfig                `toml:"quic"`
		Kcp       KcpConfig                 `toml:"kcp"`
		WebSocket WebSocketConfig           `toml:"websocket"`
		Hosts     map[string]string         `toml:"hosts"`
		Log       LogConfig                 `toml:"log"`
		PidFile   string                    `toml:"pid_file"`
		Plugin    map[string]map[string]any `toml:"plugin"`
	}

	ProtocolConfig struct {
		Enable bool `toml:"enable"`
		Port   int  `toml:"port"`
	}

	KcpConfig struct {
		Enable       bool `toml:"enable"`
		Port         int  `toml:"port"`
		DataShards   int  `toml:"data_shards"`
		ParityShards int  `toml:"parity_Shards"`
	}

	QuicConfig struct {
		Enable               bool     `toml:"enable"`
		Port                 int      `toml:"port"`
		ApplicationProtocols []string `toml:"application_protocols"`
	}

	WebSocketConfig struct {
		Enable bool   `toml:"enable"`
		Port   int    `toml:"port"`
		Path   string `toml:"path"`
	}

	// LogConfig 定义了日志的配置，包括日志级别和日志文件路径
	// 日志级别可以是 "trade", "debug", "info", "warn", "error", "fatal", "disabled"
	// 默认日志级别为 "info"
	// 日志文件路径指定了日志输出的位置
	// 如果日志文件路径为空，则日志将只输出到标准输出
	LogConfig struct {
		Level string `toml:"level"`
		File  string `toml:"file"`
	}

	// serviceConfig 定义了一个服务的配置，包括是否启用和运行函数
	// 运行函数接收一个 WaitGroup，用于在服务运行时进行同步
	// 这样可以确保所有服务在主函数退出前都能正确关闭
	// 运行函数通常会在 goroutine 中执行，以便并发处理多个服务
	serviceConfig struct {
		enable *bool
		run    func(wg *sync.WaitGroup)
	}
)

var services = []serviceConfig{
	{
		enable: &config.Tcp.Enable,
		run:    runTcp,
	},
	{
		enable: &config.Kcp.Enable,
		run:    runKcp,
	},
	{
		enable: &config.Quic.Enable,
		run:    runQuic,
	},
	{
		enable: &config.WebSocket.Enable,
		run:    runWebSocket,
	},
}

func loadConfig() error {
	configLoadLock.Lock()
	defer configLoadLock.Unlock()

	file, err := os.Open(configFile)
	if err != nil {
		return err
	}
	defer file.Close()

	byteValue, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	if err := toml.Unmarshal(byteValue, &config); err != nil {
		return err
	}

	writePIDFile()

	if err := loadLogger(); err != nil {
		return err
	}

	loadPlugins()

	return nil
}

func watchConfig() *fsnotify.Watcher {
	go func() {
		for {
			time.Sleep(time.Minute)
			if err := loadConfig(); err != nil {
				log.Error().Err(err).Msg("Failed to reload config")
			}
		}
	}()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create config watcher")
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					log.Error().Msg("watcher.Events channel closed")
					return
				}
				if !strings.HasSuffix(event.Name, configFile) {
					continue
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					log.Error().Msg("watcher.Errors channel closed")
					return
				}
				log.Error().Err(err).Msg("watcher error")
				continue
			}

			log.Info().Msg("reload config")
			if err := loadConfig(); err != nil {
				log.Error().Err(err).Msg("Failed to reload config")
			}
		}
	}()
	if err = watcher.Add("."); err != nil {
		log.Fatal().Err(err).Msg("Failed to watch config file")
	}

	return watcher
}

func loadPluginConfig(cfg map[string]any, pluginCfg any) error {
	log.Info().
		Any("config", cfg).
		Msg("Loading plugin config")

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:  pluginCfg,
		TagName: "toml",
	})
	if err != nil {
		return err
	}

	if err := decoder.Decode(cfg); err != nil {
		return err
	}

	return nil
}
