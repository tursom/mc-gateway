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
