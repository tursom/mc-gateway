// cmd/gateway/plugin.go 把网关运行时接入 pluginmanager，负责钩子分发和插件生命周期加载。

package main

import (
	"context"
	"errors"
	"net"
	"plugin"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/plugin/api"
)

var (
	exitWaitGroup sync.WaitGroup

	pluginLock sync.RWMutex
	plugins    = make(map[string]api.Plugin)

	hooks = make(map[string]map[string]any)
)

type (
	Gateway struct {
		pluginId string
	}
)

func loadPlugins() {
	pluginLock.Lock()
	defer pluginLock.Unlock()

	for pluginKey, pluginConfig := range config.Plugin {
		if enable, ok := pluginConfig["enable"].(bool); ok && enable {
			pluginFile := pluginKey
			if file, ok := pluginConfig["file"].(string); ok {
				pluginFile = file
			}

			if _, ok := plugins[pluginKey]; ok {
				continue
			}

			gateway := &Gateway{
				pluginId: pluginKey,
			}

			log.Info().Str("plugin", pluginKey).Msg("Loading plugin")
			p, err := plugin.Open(pluginFile + ".so")
			if err != nil {
				log.Err(err).Str("plugin", pluginKey).Msg("Failed to open plugin")
				continue
			}

			pluginSymbol, err := p.Lookup("Plugin")
			if err != nil {
				log.Err(err).Str("plugin", pluginKey).Msg("Failed to lookup plugin")
				continue
			}
			pluginFactory, ok := pluginSymbol.(func() api.Plugin)
			if !ok {
				log.Err(err).Str("plugin", pluginKey).Msg("Invalid plugin factory signature")
				continue
			}
			pluginInstance := pluginFactory()
			cfgObj := pluginInstance.NewConfigObj()
			if err := loadPluginConfig(pluginConfig, cfgObj); err != nil {
				log.Err(err).Str("plugin", pluginKey).Msg("Failed to load plugin config")
				continue
			}

			if err := pluginInstance.ReloadConfig(cfgObj); err != nil {
				log.Err(err).Str("plugin", pluginKey).Msg("Failed to reload plugin config")
				continue
			}

			if err := pluginInstance.Init(gateway); err != nil {
				log.Err(err).Str("plugin", pluginKey).Msg("Failed to initialize plugin")
				continue
			}

			plugins[pluginKey] = pluginInstance
			hooks[pluginKey] = make(map[string]any)
			log.Info().Str("plugin", pluginKey).Msg("Plugin loaded successfully")
		} else {
			if plugin, ok := plugins[pluginKey]; ok {
				if err := plugin.Destroy(); err != nil {
					log.Err(err).Str("plugin", pluginKey).Msg("Failed to destroy plugin")
				}
				log.Info().Str("plugin", pluginKey).Msg("Plugin disabled")
			}
			delete(plugins, pluginKey)
			delete(hooks, pluginKey)
		}
	}
}

// HandleConn 实现 api.Gateway，用于让插件把连接交回网关主流程。
func (g *Gateway) HandleConn(conn net.Conn) {
	go handleRequest(conn)
}

// Hook 实现 api.Gateway，用于注册旧版内存钩子处理器。
func (g *Gateway) Hook(hook string, handler any) error {
	pluginLock.Lock()
	defer pluginLock.Unlock()

	hooks[g.pluginId][hook] = handler
	return nil
}

// ExitWaitGroup 实现 api.Gateway，用于把插件后台任务纳入进程退出等待。
func (g *Gateway) ExitWaitGroup() *sync.WaitGroup {
	return &exitWaitGroup
}

func (g *Gateway) EmitEvent(ctx context.Context, name string, fields map[string]string) error {
	_ = ctx
	_ = name
	_ = fields
	return nil
}

func (g *Gateway) ObserveMetric(ctx context.Context, name string, value float64, labels map[string]string) error {
	_ = ctx
	_ = name
	_ = value
	_ = labels
	return nil
}

func (g *Gateway) Logger() api.Logger {
	return noopPluginLogger{}
}

func (g *Gateway) DataStore() api.DataStore {
	return noopPluginDataStore{}
}

func (g *Gateway) FileStore() api.FileStore {
	return noopPluginFileStore{}
}

func (g *Gateway) ExternalClient(name string) api.ExternalClient {
	_ = name
	return noopExternalClient{}
}

func (g *Gateway) RegisterBackgroundTask(task api.BackgroundTask) error {
	_ = task
	return nil
}

// TestOp 实现 api.Gateway，保留给测试或调试插件能力探测。
func (g *Gateway) TestOp() {
	panic("unimplemented")
}

type noopPluginLogger struct{}

func (noopPluginLogger) Debug(context.Context, string, map[string]string) {}
func (noopPluginLogger) Info(context.Context, string, map[string]string)  {}
func (noopPluginLogger) Warn(context.Context, string, map[string]string)  {}
func (noopPluginLogger) Error(context.Context, string, map[string]string) {}

type noopPluginDataStore struct{}

func (noopPluginDataStore) Put(context.Context, api.DataRecord) error { return nil }
func (noopPluginDataStore) Get(context.Context, string) (api.DataRecord, error) {
	return api.DataRecord{}, errors.New("plugin data store is unavailable")
}
func (noopPluginDataStore) Delete(context.Context, string) error { return nil }

type noopPluginFileStore struct{}

func (noopPluginFileStore) ResourcePath(string) (string, error) {
	return "", errors.New("plugin file store is unavailable")
}
func (noopPluginFileStore) Write(context.Context, string, string, []byte, string, time.Duration) error {
	return nil
}
func (noopPluginFileStore) Read(context.Context, string, string, int64) ([]byte, error) {
	return nil, errors.New("plugin file store is unavailable")
}
func (noopPluginFileStore) Delete(context.Context, string, string) error { return nil }

type noopExternalClient struct{}

func (noopExternalClient) DoHTTP(context.Context, api.ExternalRequest) (api.ExternalResponse, error) {
	return api.ExternalResponse{}, errors.New("external client is unavailable")
}
func (noopExternalClient) DialTCP(context.Context, string, time.Duration) (net.Conn, error) {
	return nil, errors.New("external client is unavailable")
}
func (noopExternalClient) HealthCheck(context.Context) error { return nil }

func Handler1[T1, R any](t1 T1) func(func(T1) R) R {
	return func(acceptor func(T1) R) R {
		return acceptor(t1)
	}
}

func Handler2[T1, T2, R any](t1 T1, t2 T2) func(func(T1, T2) R) R {
	return func(acceptor func(T1, T2) R) R {
		return acceptor(t1, t2)
	}
}

func Handler1R2[T1, R1, R2 any](t1 T1) func(func(T1) (R1, R2)) (R1, R2) {
	return func(acceptor func(T1) (R1, R2)) (R1, R2) {
		return acceptor(t1)
	}
}

func invokeFirstHookHandler[Acceptor, Handler any](
	hook api.HookType[Acceptor, Handler],
	acceptor func(Acceptor) bool,
	handelr func(Handler) error,
) (bool, error) {
	pluginLock.RLock()
	defer pluginLock.RUnlock()

	for _, handlers := range hooks {
		if handler, ok := handlers[hook.Key()].(api.HookHandler[Acceptor, Handler]); ok && acceptor(handler.Acceptor()) {
			if err := handelr(handler.Handler()); err != nil {
				return true, err
			}
			return true, nil
		}
	}

	return false, nil
}

func invokeAllHookHandler[Acceptor, Handler any](
	hook api.HookType[Acceptor, Handler],
	acceptor func(Acceptor) bool,
	handelr func(Handler) error,
) error {
	pluginLock.RLock()
	defer pluginLock.RUnlock()

	for _, handlers := range hooks {
		if handler, ok := handlers[hook.Key()].(api.HookHandler[Acceptor, Handler]); ok && acceptor(handler.Acceptor()) {
			if err := handelr(handler.Handler()); err != nil {
				return err
			}
		}
	}

	return nil
}
