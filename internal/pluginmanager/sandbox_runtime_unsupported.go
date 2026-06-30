//go:build !linux

package pluginmanager

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

func configureSandboxCommand(_ *exec.Cmd, _ string) {}

func applySandboxRLimits(pid int, _ SandboxPolicy) error {
	if pid <= 0 {
		return errors.New("sandbox pid is required")
	}
	return validateSandboxEnforcementSupported()
}

func validateSandboxEnforcementSupported() error {
	return fmt.Errorf("sandbox-process enforcement requires linux namespaces, got %s", runtime.GOOS)
}
