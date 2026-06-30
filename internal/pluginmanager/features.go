package pluginmanager

type SandboxEnvironmentSelfCheck func(SandboxPolicy) error

type RuntimeFeatureFactsOptions struct {
	FutureRuntimeGates FutureRuntimeGates
	SandboxPolicy      SandboxPolicy
	SandboxSelfCheck   SandboxEnvironmentSelfCheck
}

func RuntimeTypeFeatures() []RuntimeFeature {
	return RuntimeTypeFeaturesFor(RuntimeFeatureFactsOptions{})
}

func RuntimeTypeFeaturesFor(options RuntimeFeatureFactsOptions) []RuntimeFeature {
	options = normalizeRuntimeFeatureFactsOptions(options)
	sandboxFeature := sandboxRuntimeFeature(options)
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
			Implemented:       sandboxFeature.implemented,
			Maturity:          sandboxFeature.maturity,
			DataPlane:         sandboxFeature.dataPlane,
			RequiresRestart:   true,
			UnsupportedReason: sandboxFeature.runtimeReason,
			Entry:             RuntimeEntry,
		},
		{
			Type:              RuntimeWASM,
			Implemented:       true,
			Maturity:          FeatureMaturityPartial,
			DataPlane:         true,
			RequiresRestart:   false,
			UnsupportedReason: "wasm runtime only supports low-risk extension points; protocol-proxy, network, file, and high-risk extension points are not supported",
			Entry:             RuntimeWASMEntry,
		},
	}
}

func PluginServiceModeFeatures() []PluginServiceModeFeature {
	return PluginServiceModeFeaturesFor(RuntimeFeatureFactsOptions{})
}

func PluginServiceModeFeaturesFor(options RuntimeFeatureFactsOptions) []PluginServiceModeFeature {
	options = normalizeRuntimeFeatureFactsOptions(options)
	sandboxFeature := sandboxRuntimeFeature(options)
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
			Implemented:       sandboxFeature.implemented,
			Maturity:          sandboxFeature.maturity,
			DataPlane:         sandboxFeature.dataPlane,
			RequiresRestart:   true,
			UnsupportedReason: sandboxFeature.serviceModeReason,
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
		{Key: ExtensionIngressService, Type: "service", Implemented: false, Maturity: FeatureMaturityReserved, DataPlane: false, UnsupportedReason: "ingress.service/v1 is reserved; schema and governance checks exist but gateway-managed listener data-plane is not enabled"},
	}
}

func RuntimeTypeFeature(runtimeType string) RuntimeFeature {
	return RuntimeTypeFeatureFor(runtimeType, RuntimeFeatureFactsOptions{})
}

func RuntimeTypeFeatureFor(runtimeType string, options RuntimeFeatureFactsOptions) RuntimeFeature {
	for _, feature := range RuntimeTypeFeaturesFor(options) {
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
	return PluginServiceModeFeatureForOptions(mode, RuntimeFeatureFactsOptions{})
}

func PluginServiceModeFeatureForOptions(mode string, options RuntimeFeatureFactsOptions) PluginServiceModeFeature {
	for _, feature := range PluginServiceModeFeaturesFor(options) {
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

type sandboxProductionFeature struct {
	implemented       bool
	maturity          string
	dataPlane         bool
	runtimeReason     string
	serviceModeReason string
}

func normalizeRuntimeFeatureFactsOptions(options RuntimeFeatureFactsOptions) RuntimeFeatureFactsOptions {
	options.FutureRuntimeGates = normalizeFutureRuntimeGates(options.FutureRuntimeGates)
	options.SandboxPolicy = normalizeSandboxPolicy(options.SandboxPolicy)
	if options.SandboxSelfCheck == nil {
		options.SandboxSelfCheck = defaultSandboxEnvironmentSelfCheck
	}
	return options
}

func sandboxRuntimeFeature(options RuntimeFeatureFactsOptions) sandboxProductionFeature {
	if !options.FutureRuntimeGates.SandboxEnabled() {
		return sandboxProductionFeature{
			implemented:       false,
			maturity:          FeatureMaturityReserved,
			dataPlane:         false,
			runtimeReason:     "sandbox-process runtime is reserved; current gateway releases do not expose a sandbox data-plane without FutureRuntimeGates.SandboxProcess",
			serviceModeReason: "sandbox-process service mode is reserved; current data-plane modes are in-process and go-plugin-process until FutureRuntimeGates.SandboxProcess is enabled",
		}
	}
	if err := options.SandboxSelfCheck(options.SandboxPolicy); err != nil {
		reason := "sandbox-process environment self-check failed: " + err.Error()
		return sandboxProductionFeature{
			implemented:       true,
			maturity:          FeatureMaturityPartial,
			dataPlane:         false,
			runtimeReason:     reason,
			serviceModeReason: reason,
		}
	}
	reason := "sandbox-process data-plane is partial; first production slice only supports sandbox-process artifacts in sandbox-process service mode"
	return sandboxProductionFeature{
		implemented:       true,
		maturity:          FeatureMaturityPartial,
		dataPlane:         true,
		runtimeReason:     reason,
		serviceModeReason: reason,
	}
}
