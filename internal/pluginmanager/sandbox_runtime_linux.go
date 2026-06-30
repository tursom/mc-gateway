//go:build linux

package pluginmanager

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const sandboxUnprivilegedID = 65534

func configureSandboxCommand(cmd *exec.Cmd, rootDir string, _ SandboxPolicy) {
	attr := &syscall.SysProcAttr{
		Chroot:      rootDir,
		Setsid:      true,
		Pdeathsig:   syscall.SIGKILL,
		Cloneflags:  unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWPID,
		AmbientCaps: []uintptr{},
	}
	if sandboxUserNamespaceConfigured() {
		containerUID, containerGID, hostUID, hostGID := sandboxNamespaceIDs()
		attr.Cloneflags |= unix.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}, {ContainerID: containerUID, HostID: hostUID, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}, {ContainerID: containerGID, HostID: hostGID, Size: 1}}
		attr.GidMappingsEnableSetgroups = false
		attr.Credential = &syscall.Credential{Uid: uint32(containerUID), Gid: uint32(containerGID), NoSetGroups: true}
	}
	cmd.SysProcAttr = attr
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
	if policy.FileQuotaBytes > 0 {
		limit := &unix.Rlimit{Cur: uint64(policy.FileQuotaBytes), Max: uint64(policy.FileQuotaBytes)}
		if err := unix.Prlimit(pid, unix.RLIMIT_FSIZE, limit, nil); err != nil {
			return fmt.Errorf("apply sandbox file size limit: %w", err)
		}
	}
	return nil
}

func validateSandboxEnforcementSupported(policy SandboxPolicy) error {
	for _, nsPath := range []string{
		"/proc/self/ns/mnt",
		"/proc/self/ns/net",
		"/proc/self/ns/pid",
		"/proc/self/ns/ipc",
		"/proc/self/ns/uts",
		"/proc/self/ns/user",
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
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		return fmt.Errorf("sandbox-process file size rlimit unavailable: %w", err)
	}
	report := sandboxEnforcementReportForPolicy(policy)
	if err := report.err(); err != nil {
		return err
	}
	return sandboxLinuxLaunchSelfCheck(policy)
}

func sandboxPlatformEnforcementFacts(policy SandboxPolicy) []SandboxEnforcementFact {
	externalProcessPolicy := policy.ExternalIsolation
	userNamespaceEnforced := sandboxUserNamespaceConfigured() || externalProcessPolicy
	userNamespaceMethod := "clone(CLONE_NEWUSER)+uid_gid_map"
	userNamespaceReason := ""
	if !sandboxUserNamespaceConfigured() && externalProcessPolicy {
		userNamespaceMethod = "external-container-user-namespace-or-non-root-boundary"
	} else if !sandboxUserNamespaceConfigured() {
		userNamespaceMethod = ""
		userNamespaceReason = "gateway is not running with privileges needed to create the sandbox user namespace; set sandbox policy external_isolation=true when an outer container/user boundary enforces it"
	}
	cgroupMethod, cgroupEnforced := sandboxCgroupV2Fact()
	seccompReason := ""
	forkExecReason := ""
	seccompMethod := "external-container-seccomp-profile"
	forkExecMethod := "external-container-fork-exec-policy"
	if !externalProcessPolicy {
		seccompReason = "seccomp filter cannot be installed safely by the current Go process adapter; set sandbox policy external_isolation=true when an outer container/profile enforces it"
		forkExecReason = "fork/exec allowlist cannot be enforced safely by the current Go process adapter; set sandbox policy external_isolation=true when an outer container/profile enforces it"
		seccompMethod = ""
		forkExecMethod = ""
	}
	return []SandboxEnforcementFact{
		{Category: "namespace", Key: "mount", Required: true, Enforced: sandboxNamespacePathExists("mnt"), Method: "clone(CLONE_NEWNS)+chroot"},
		{Category: "namespace", Key: "network", Required: true, Enforced: sandboxNamespacePathExists("net"), Method: "clone(CLONE_NEWNET)"},
		{Category: "namespace", Key: "pid", Required: true, Enforced: sandboxNamespacePathExists("pid"), Method: "clone(CLONE_NEWPID)"},
		{Category: "namespace", Key: "uts", Required: true, Enforced: sandboxNamespacePathExists("uts"), Method: "clone(CLONE_NEWUTS)"},
		{Category: "namespace", Key: "ipc", Required: true, Enforced: sandboxNamespacePathExists("ipc"), Method: "clone(CLONE_NEWIPC)"},
		{Category: "namespace", Key: "user", Required: true, Enforced: userNamespaceEnforced, Method: userNamespaceMethod, UnsupportedReason: userNamespaceReason},
		{Category: "process", Key: "no_new_privs", Required: true, Enforced: true, Method: "prctl(PR_SET_NO_NEW_PRIVS)-on-locked-launch-thread"},
		{Category: "process", Key: "capabilities_dropped", Required: true, Enforced: true, Method: "setuid-setgid-no-ambient-caps"},
		{Category: "process", Key: "seccomp", Required: true, Enforced: externalProcessPolicy, Method: seccompMethod, UnsupportedReason: seccompReason},
		{Category: "process", Key: "fork_exec_policy", Required: true, Enforced: externalProcessPolicy, Method: forkExecMethod, UnsupportedReason: forkExecReason},
		{Category: "resource", Key: "cgroup_v2", Required: false, Enforced: cgroupEnforced, Method: cgroupMethod},
		{Category: "cleanup", Key: "process_tree", Required: true, Enforced: true, Method: "pid-namespace-init-exit+process-group-signal"},
		{Category: "cleanup", Key: "orphan", Required: true, Enforced: true, Method: "PR_SET_PDEATHSIG(SIGKILL)+pid-namespace"},
	}
}

func sandboxFilesystemEnforcementFact(policy SandboxPolicy) (method string, enforced bool, unsupportedReason string) {
	if sandboxUserNamespaceConfigured() {
		return "chroot-staged-root-readonly-permissions+different-runtime-uid", true, ""
	}
	if policy.ExternalIsolation {
		return "external-container-permission-boundary+chroot-staged-root", true, ""
	}
	return "", false, "readonly root and path allowlist require a root-created sandbox user namespace or sandbox policy external_isolation=true with an outer container/permission boundary"
}

func startSandboxCommand(cmd *exec.Cmd) (func(), error) {
	errCh := make(chan error, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	go func() {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			errCh <- fmt.Errorf("set sandbox no_new_privs: %w", err)
			return
		}
		if value, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil {
			errCh <- fmt.Errorf("verify sandbox no_new_privs: %w", err)
			return
		} else if value != 1 {
			errCh <- errors.New("verify sandbox no_new_privs: kernel did not enable no_new_privs")
			return
		}
		if err := cmd.Start(); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
		<-release
	}()
	if err := <-errCh; err != nil {
		return nil, err
	}
	return func() { releaseOnce.Do(func() { close(release) }) }, nil
}

func signalSandboxProcessTree(pid int, signal os.Signal) error {
	if pid <= 0 {
		return errors.New("sandbox pid is required")
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		sig = syscall.SIGTERM
	}
	if err := syscall.Kill(-pid, sig); err == nil || !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return syscall.Kill(pid, sig)
}

func killSandboxProcessTree(pid int) error {
	return signalSandboxProcessTree(pid, os.Kill)
}

func chownSandboxWritable(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	_, _, hostUID, hostGID := sandboxNamespaceIDs()
	return os.Chown(path, hostUID, hostGID)
}

func sandboxNamespaceIDs() (containerUID, containerGID, hostUID, hostGID int) {
	hostUID = os.Geteuid()
	hostGID = os.Getegid()
	if hostUID == 0 {
		return sandboxUnprivilegedID, sandboxUnprivilegedID, sandboxUnprivilegedID, sandboxUnprivilegedID
	}
	return hostUID, hostGID, hostUID, hostGID
}

func sandboxUserNamespaceConfigured() bool {
	return os.Geteuid() == 0
}

func sandboxNamespacePathExists(name string) bool {
	_, err := os.Stat("/proc/self/ns/" + name)
	return err == nil
}

func sandboxCgroupV2Fact() (string, bool) {
	stat, err := os.Stat("/sys/fs/cgroup")
	if err != nil || !stat.IsDir() {
		return "unavailable", false
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return "cgroup-v1-or-unavailable-not-used", false
	}
	return "cgroup-v2-available-not-used-rlimit-active", false
}

func sandboxLinuxLaunchSelfCheck(policy SandboxPolicy) error {
	cmd := exec.Command("/bin/true")
	configureSandboxCommand(cmd, "", policy)
	if cmd.SysProcAttr != nil {
		cmd.SysProcAttr.Chroot = ""
	}
	release, err := startSandboxCommand(cmd)
	if err != nil {
		return fmt.Errorf("sandbox-process linux namespace/no_new_privs launch self-check failed: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		release()
		return fmt.Errorf("sandbox-process linux namespace/no_new_privs launch self-check wait failed: %w", err)
	}
	release()
	return nil
}
