package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

const (
	envFutureRuntimeSandboxProcess = "MC_GATEWAY_FUTURE_RUNTIME_SANDBOX_PROCESS"
	envSandboxPolicyJSON           = "MC_GATEWAY_SANDBOX_POLICY_JSON"
)

func pluginRuntimeFeatureFactsOptionsFromEnv(getenv func(string) string) (pluginmanager.RuntimeFeatureFactsOptions, error) {
	var gates pluginmanager.FutureRuntimeGates
	sandboxEnabled, err := parsePluginFeatureBoolEnv(getenv(envFutureRuntimeSandboxProcess), true, envFutureRuntimeSandboxProcess)
	if err != nil {
		return pluginmanager.RuntimeFeatureFactsOptions{}, err
	}
	gates.SandboxProcess = sandboxEnabled

	var sandboxPolicy pluginmanager.SandboxPolicy
	if raw := strings.TrimSpace(getenv(envSandboxPolicyJSON)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &sandboxPolicy); err != nil {
			return pluginmanager.RuntimeFeatureFactsOptions{}, fmt.Errorf("%s must be a sandbox policy JSON object: %w", envSandboxPolicyJSON, err)
		}
	} else if sandboxEnabled {
		sandboxPolicy.ExternalIsolation = true
	}
	return pluginmanager.RuntimeFeatureFactsOptions{
		FutureRuntimeGates: gates,
		SandboxPolicy:      sandboxPolicy,
	}, nil
}

func parsePluginFeatureBoolEnv(value string, fallback bool, name string) (bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback, nil
	}
	switch value {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean", name)
	}
}
