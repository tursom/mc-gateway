package pluginmanager

func RuntimeTypeFeatures() []RuntimeFeature {
	return []RuntimeFeature{
		{
			Type:            RuntimeGoPlugin,
			Implemented:     true,
			Maturity:        FeatureMaturityImplemented,
			DataPlane:       true,
			RequiresRestart: false,
			Entry:           RuntimeEntry,
		},
		{
			Type:              RuntimeBuiltin,
			Implemented:       false,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         false,
			RequiresRestart:   false,
			UnsupportedReason: "builtin runtime is limited to gateway-owned official plugins",
		},
		{
			Type:              RuntimeSandbox,
			Implemented:       true,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         true,
			RequiresRestart:   true,
			UnsupportedReason: "sandbox-process enforcement is implemented but disabled unless future runtime gates enable sandbox_process",
		},
		{
			Type:              RuntimeWASM,
			Implemented:       true,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         true,
			RequiresRestart:   true,
			UnsupportedReason: "wazero runtime is implemented for low-risk validation extension points but disabled unless future runtime gates enable wasm",
			Entry:             RuntimeWASMEntry,
		},
	}
}

func PluginServiceModeFeatures() []PluginServiceModeFeature {
	return []PluginServiceModeFeature{
		{
			Mode:            PluginServiceModeInProcess,
			Implemented:     true,
			Maturity:        FeatureMaturityImplemented,
			DataPlane:       true,
			RequiresRestart: false,
		},
		{
			Mode:              PluginServiceModeGoPluginProcess,
			Implemented:       true,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         true,
			RequiresRestart:   true,
			UnsupportedReason: GoPluginProcessPartialUnsupportedReason,
		},
		{
			Mode:              PluginServiceModeSandboxProcess,
			Implemented:       true,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         true,
			RequiresRestart:   true,
			UnsupportedReason: "sandbox-process service mode is implemented but disabled unless future runtime gates enable sandbox_process",
		},
	}
}

func ExtensionPointFeatures() []ExtensionPointFeature {
	return []ExtensionPointFeature{
		{Key: ExtensionUpstreamConnect, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionRouteResolve, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionRouteResolver, Type: "provider", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionRuleEvaluate, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionConfigValidate, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: false},
		{Key: ExtensionStatusPing, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionConnectionFilter, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionHandshakeFilter, Type: "hook", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: true},
		{Key: ExtensionEventSubscriber, Type: "subscriber", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: false},
		{Key: ExtensionProvider, Type: "provider", Implemented: true, Maturity: FeatureMaturityImplemented, DataPlane: false},
		{Key: ExtensionAuthProvider, Type: "provider", Implemented: true, Maturity: FeatureMaturityPartial, DataPlane: false, UnsupportedReason: "auth.provider/v1 provider registration and status are implemented; Minecraft login data-plane integration is not implemented"},
		{Key: ExtensionAdminAuthProvider, Type: "provider", Implemented: false, Maturity: FeatureMaturityReserved, DataPlane: false, UnsupportedReason: "admin.auth.provider/v1 is reserved; local admin break-glass remains the implemented authentication path"},
		{Key: ExtensionIngressService, Type: "service", Implemented: true, Maturity: FeatureMaturityPartial, DataPlane: true, UnsupportedReason: "gateway-managed listener lifecycle is implemented but disabled unless future runtime gates enable ingress"},
	}
}

func RuntimeTypeFeature(runtimeType string) RuntimeFeature {
	for _, feature := range RuntimeTypeFeatures() {
		if feature.Type == runtimeType {
			return feature
		}
	}
	return RuntimeFeature{
		Type:              runtimeType,
		Implemented:       false,
		Maturity:          FeatureMaturityStub,
		DataPlane:         false,
		RequiresRestart:   true,
		UnsupportedReason: "runtime type is not recognized by this gateway",
	}
}

func PluginServiceModeFeatureFor(mode string) PluginServiceModeFeature {
	for _, feature := range PluginServiceModeFeatures() {
		if feature.Mode == mode {
			return feature
		}
	}
	return PluginServiceModeFeature{
		Mode:              mode,
		Implemented:       false,
		Maturity:          FeatureMaturityStub,
		DataPlane:         false,
		RequiresRestart:   true,
		UnsupportedReason: "plugin service mode is not recognized by this gateway",
	}
}
