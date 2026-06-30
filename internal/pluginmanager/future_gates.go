package pluginmanager

func normalizeFutureRuntimeGates(gates FutureRuntimeGates) FutureRuntimeGates {
	return gates
}

func (g FutureRuntimeGates) SandboxEnabled() bool {
	return g.SandboxProcess
}

func (g FutureRuntimeGates) WASMEnabled() bool {
	return g.WASM
}

func (g FutureRuntimeGates) IngressEnabled() bool {
	return g.Ingress
}

func (m *Manager) FutureRuntimeGates() FutureRuntimeGates {
	if m == nil {
		return FutureRuntimeGates{}
	}
	return m.futureGates
}

func (m *Manager) RuntimeFeatureFactsOptions() RuntimeFeatureFactsOptions {
	if m == nil {
		return RuntimeFeatureFactsOptions{}
	}
	return RuntimeFeatureFactsOptions{
		FutureRuntimeGates: m.futureGates,
		SandboxPolicy:      m.sandboxPolicy,
		SandboxSelfCheck:   m.sandboxSelfCheck,
	}
}
