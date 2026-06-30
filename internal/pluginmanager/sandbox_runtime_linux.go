//go:build linux

package pluginmanager

import (
	"errors"
	"fmt"
	"os"
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
	for _, nsPath := range []string{
		"/proc/self/ns/mnt",
		"/proc/self/ns/net",
		"/proc/self/ns/ipc",
		"/proc/self/ns/uts",
	} {
		if _, err := os.Stat(nsPath); err != nil {
			return fmt.Errorf("sandbox-process linux namespace support missing %s: %w", nsPath, err)
		}
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &limit); err != nil {
		return fmt.Errorf("sandbox-process memory rlimit unavailable: %w", err)
	}
	if err := unix.Getrlimit(unix.RLIMIT_CPU, &limit); err != nil {
		return fmt.Errorf("sandbox-process cpu rlimit unavailable: %w", err)
	}
	if stat, err := os.Stat("/sys/fs/cgroup"); err != nil {
		return fmt.Errorf("sandbox-process cgroup filesystem unavailable: %w", err)
	} else if !stat.IsDir() {
		return errors.New("sandbox-process cgroup filesystem is not a directory")
	}
	return nil
}
