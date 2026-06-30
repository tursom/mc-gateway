//go:build linux

package pluginmanager

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureSandboxCommand(cmd *exec.Cmd, rootDir string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     rootDir,
		Setsid:     true,
		Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS,
	}
}

func applySandboxRLimits(pid int, policy SandboxPolicy) error {
	if pid <= 0 {
		return errors.New("sandbox pid is required")
	}
	if policy.MemoryBytes > 0 {
		limit := &unix.Rlimit{Cur: uint64(policy.MemoryBytes), Max: uint64(policy.MemoryBytes)}
		if err := unix.Prlimit(pid, unix.RLIMIT_AS, limit, nil); err != nil {
			return fmt.Errorf("apply sandbox memory limit: %w", err)
		}
	}
	if policy.CPUSeconds > 0 {
		limit := &unix.Rlimit{Cur: uint64(policy.CPUSeconds), Max: uint64(policy.CPUSeconds)}
		if err := unix.Prlimit(pid, unix.RLIMIT_CPU, limit, nil); err != nil {
			return fmt.Errorf("apply sandbox cpu limit: %w", err)
		}
	}
	return nil
}

func validateSandboxEnforcementSupported() error {
	return nil
}
