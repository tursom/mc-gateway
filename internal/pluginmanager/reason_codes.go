package pluginmanager

import (
	"errors"
	"strings"
)

const (
	ReasonSandboxFutureGateClosed           = "sandbox_future_gate_closed"
	ReasonSandboxEnvironmentSelfCheckFailed = "sandbox_environment_self_check_failed"
	ReasonSandboxServiceModeInactive        = "sandbox_service_mode_inactive"
	ReasonSandboxAdapterUnavailable         = "sandbox_adapter_factory_unavailable"
	ReasonSandboxCrashLoop                  = "sandbox_crash_loop"
	ReasonSandboxProcessExited              = "sandbox_process_exited"
	ReasonSandboxCapabilityBlock            = "capability_enforcement_unavailable"
	ReasonSandboxExternalCapabilityMissing  = "sandbox_external_dependency_capability_missing"
	ReasonSandboxABIMismatch                = "abi_mismatch"
	ReasonSandboxSecretDenied               = "sandbox_secret_denied"
)

func reasonCodeFromError(err error) string {
	if err == nil {
		return ""
	}
	var controlErr sandboxControlError
	if errors.As(err, &controlErr) && controlErr.code != "" {
		return controlErr.code
	}
	var invokeErr sandboxInvocationError
	if errors.As(err, &invokeErr) && invokeErr.code != "" {
		return invokeErr.code
	}
	return reasonCodeFromMessage(err.Error())
}

func reasonCodeFromMessage(message string) string {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case lower == "":
		return ""
	case strings.Contains(lower, "future runtime gate"):
		return ReasonSandboxFutureGateClosed
	case strings.Contains(lower, "environment self-check failed") || strings.Contains(lower, "self-check"):
		return ReasonSandboxEnvironmentSelfCheckFailed
	case strings.Contains(lower, "disabled by plugin service mode"):
		return ReasonSandboxServiceModeInactive
	case strings.Contains(lower, "managed runtime adapter factory") || strings.Contains(lower, "adapter is not available"):
		return ReasonSandboxAdapterUnavailable
	case strings.Contains(lower, "crash loop"):
		return ReasonSandboxCrashLoop
	case strings.Contains(lower, "process exited"):
		return ReasonSandboxProcessExited
	case strings.Contains(lower, "external dependencies require runtime capability"):
		return ReasonSandboxExternalCapabilityMissing
	case strings.Contains(lower, "cannot enforce required capabilities") ||
		strings.Contains(lower, "required enforcement unavailable") ||
		strings.Contains(lower, "capability enforcement"):
		return ReasonSandboxCapabilityBlock
	case strings.Contains(lower, "abi_version") || strings.Contains(lower, "abi mismatch") || strings.Contains(lower, "abi_mismatch"):
		return ReasonSandboxABIMismatch
	case strings.Contains(lower, "secret request denied") || strings.Contains(lower, "secret handle"):
		return ReasonSandboxSecretDenied
	default:
		return ""
	}
}
