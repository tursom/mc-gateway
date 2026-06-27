// internal/gatewayconfig/plugin.go 定义进程启动时可加载的静态插件配置项。

package gatewayconfig

import "github.com/mitchellh/mapstructure"

func DecodePluginConfig(cfg map[string]any, pluginCfg any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:  pluginCfg,
		TagName: "toml",
	})
	if err != nil {
		return err
	}

	return decoder.Decode(cfg)
}
