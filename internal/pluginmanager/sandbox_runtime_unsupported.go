//go:build !linux

package pluginmanager

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func configureSandboxCommand(_ *exec.Cmd, _ string, _ SandboxPolicy) {}

func startSandboxCommand(cmd *exec.Cmd) (func(), error) {
	return func() {}, cmd.Start()
}

func applySandboxRLimits(pid int, _ SandboxPolicy) error {
	if pid <= 0 {
		return errors.New("sandbox pid is required")
	}
	return validateSandboxEnforcementSupported(SandboxPolicy{})
}

func validateSandboxEnforcementSupported(_ SandboxPolicy) error {
	return fmt.Errorf("sandbox-process enforcement requires linux namespaces, got %s", runtime.GOOS)
}

func sandboxPlatformEnforcementFacts(_ SandboxPolicy) []SandboxEnforcementFact {
	reason := fmt.Sprintf("sandbox-process enforcement requires linux namespaces, got %s", runtime.GOOS)
	return []SandboxEnforcementFact{
		{Category: "namespace", Key: "mount", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "namespace", Key: "network", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "namespace", Key: "pid", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "namespace", Key: "uts", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "namespace", Key: "ipc", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "namespace", Key: "user", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "process", Key: "no_new_privs", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "process", Key: "capabilities_dropped", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "process", Key: "seccomp", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "process", Key: "fork_exec_policy", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "cleanup", Key: "process_tree", Required: true, Enforced: false, UnsupportedReason: reason},
		{Category: "cleanup", Key: "orphan", Required: true, Enforced: false, UnsupportedReason: reason},
	}
}

func sandboxFilesystemEnforcementFact(_ SandboxPolicy) (method string, enforced bool, unsupportedReason string) {
	return "", false, fmt.Sprintf("sandbox-process filesystem enforcement requires linux namespaces, got %s", runtime.GOOS)
}

func signalSandboxProcessTree(pid int, signal os.Signal) error {
	if pid <= 0 {
		return errors.New("sandbox pid is required")
	}
	return fmt.Errorf("sandbox-process tree signal %v requires linux process groups, got %s", signal, runtime.GOOS)
}

func killSandboxProcessTree(pid int) error {
	return signalSandboxProcessTree(pid, os.Kill)
}

func chownSandboxWritable(string) error {
	return nil
}
