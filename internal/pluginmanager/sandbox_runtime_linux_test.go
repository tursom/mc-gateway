//go:build linux

package pluginmanager

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSandboxLinuxCommandAttributes(t *testing.T) {
	cmd := exec.Command("/bin/true")
	configureSandboxCommand(cmd, "/sandbox-root", normalizeSandboxPolicy(SandboxPolicy{}))
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	attr := cmd.SysProcAttr
	for name, flag := range map[string]uintptr{
		"mount":   unix.CLONE_NEWNS,
		"network": unix.CLONE_NEWNET,
		"pid":     unix.CLONE_NEWPID,
		"uts":     unix.CLONE_NEWUTS,
		"ipc":     unix.CLONE_NEWIPC,
	} {
		if attr.Cloneflags&flag == 0 {
			t.Fatalf("Cloneflags missing %s namespace: %#x", name, attr.Cloneflags)
		}
	}
	if sandboxUserNamespaceConfigured() && attr.Cloneflags&unix.CLONE_NEWUSER == 0 {
		t.Fatalf("Cloneflags missing user namespace while configured: %#x", attr.Cloneflags)
	}
	if attr.Chroot != "/sandbox-root" || !attr.Setsid || attr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %+v, want chroot, setsid, pdeathsig", attr)
	}
	if sandboxUserNamespaceConfigured() {
		if attr.Credential == nil || attr.Credential.Uid != sandboxUnprivilegedID || len(attr.UidMappings) == 0 || len(attr.GidMappings) == 0 {
			t.Fatalf("SysProcAttr = %+v, want user namespace mappings and dropped credentials", attr)
		}
	}
}

func TestSandboxLinuxEnforcementFactsExternalIsolation(t *testing.T) {
	report := sandboxEnforcementReportForPolicy(normalizeSandboxPolicy(SandboxPolicy{}))
	err := report.err()
	if err == nil || !strings.Contains(err.Error(), "process.seccomp") || !strings.Contains(err.Error(), "process.fork_exec_policy") {
		t.Fatalf("report.err() = %v, want missing seccomp and fork/exec policy", err)
	}

	report = sandboxEnforcementReportForPolicy(normalizeSandboxPolicy(SandboxPolicy{ExternalIsolation: true}))
	if err := report.err(); err != nil {
		t.Fatalf("external isolation report.err() = %v", err)
	}
	if !sandboxFactsAllRequiredEnforced(report.Facts, "process", "no_new_privs", "capabilities_dropped", "seccomp", "fork_exec_policy") {
		t.Fatalf("facts = %+v, want process restrictions satisfied by adapter plus external policy", report.Facts)
	}
}
