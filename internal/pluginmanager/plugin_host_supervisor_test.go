package pluginmanager

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestPluginHostSupervisorWaitRemovesSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host control sockets use Unix-domain sockets")
	}
	socketPath := filepath.Join(t.TempDir(), "plugin-host.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(unix) error = %v", err)
	}
	defer listener.Close()
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("Lstat(socket before wait) error = %v", err)
	}
	metadataPath := pluginHostMetadataPath(socketPath)
	if err := writePluginHostMetadata(metadataPath, pluginHostMetadata{
		SchemaVersion: pluginHostMetadataSchemaVersion,
		PluginID:      "plugin-a",
		ArtifactID:    "artifact-a",
		PID:           os.Getpid(),
		SocketPath:    socketPath,
		StartedAt:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("writePluginHostMetadata() error = %v", err)
	}

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(exit command) error = %v", err)
	}
	process := &PluginHostSupervisorProcess{
		SocketPath:   socketPath,
		MetadataPath: metadataPath,
		cmd:          cmd,
		done:         make(chan struct{}),
	}
	process.wait()
	_ = listener.Close()

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("Lstat(socket after wait) error = %v, want not exist", err)
	}
	if _, err := os.Lstat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("Lstat(metadata after wait) error = %v, want not exist", err)
	}
}

func TestPluginHostSupervisorUnexpectedCleanExitMarksCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sh to simulate a short-lived plugin-host")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(exit command) error = %v", err)
	}
	process := &PluginHostSupervisorProcess{
		PluginID:   "plugin-a",
		ArtifactID: "artifact-a",
		PID:        cmd.Process.Pid,
		StartedAt:  time.Now().Unix(),
		cmd:        cmd,
		done:       make(chan struct{}),
	}
	process.wait()
	summary := process.Summary()
	if !summary.CrashLoop ||
		summary.CrashCount != 1 ||
		summary.State != RuntimeFailed ||
		summary.LastError != "plugin-host exited unexpectedly" ||
		summary.ExitedAt == 0 ||
		summary.LastCrashAt == 0 {
		t.Fatalf("summary = %+v, want unexpected clean exit classified as crash", summary)
	}
}

func TestCleanupPluginHostRuntimeDirRemovesOnlyStaleInactiveSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host control sockets use Unix-domain sockets")
	}
	runtimeDir := t.TempDir()
	stalePath := filepath.Join(runtimeDir, "plugin-host-stale.sock")
	staleListener, err := net.Listen("unix", stalePath)
	if err != nil {
		t.Fatalf("Listen(stale unix) error = %v", err)
	}
	if unixListener, ok := staleListener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := staleListener.Close(); err != nil {
		t.Fatalf("Close(stale listener) error = %v", err)
	}

	activePath := filepath.Join(runtimeDir, "plugin-host-active.sock")
	activeListener, err := net.Listen("unix", activePath)
	if err != nil {
		t.Fatalf("Listen(active unix) error = %v", err)
	}
	defer activeListener.Close()

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatalf("Chtimes(stale socket) error = %v", err)
	}
	if err := os.Chtimes(activePath, old, old); err != nil {
		t.Fatalf("Chtimes(active socket) error = %v", err)
	}
	activeMetadataPath := filepath.Join(runtimeDir, "plugin-host-active.json")
	if err := writePluginHostMetadata(activeMetadataPath, pluginHostMetadata{
		SchemaVersion: pluginHostMetadataSchemaVersion,
		PluginID:      "active",
		ArtifactID:    "artifact-active",
		PID:           os.Getpid(),
		SocketPath:    activePath,
		StartedAt:     old.Unix(),
	}); err != nil {
		t.Fatalf("writePluginHostMetadata(active) error = %v", err)
	}
	if err := os.Chtimes(activeMetadataPath, old, old); err != nil {
		t.Fatalf("Chtimes(active metadata) error = %v", err)
	}

	removed, err := cleanupPluginHostRuntimeDir(runtimeDir, time.Hour, now)
	if err != nil {
		t.Fatalf("cleanupPluginHostRuntimeDir() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanupPluginHostRuntimeDir() removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("Lstat(stale socket) error = %v, want not exist", err)
	}
	if _, err := os.Lstat(activePath); err != nil {
		t.Fatalf("Lstat(active socket) error = %v, want still present", err)
	}
	if _, err := os.Lstat(activeMetadataPath); err != nil {
		t.Fatalf("Lstat(active metadata) error = %v, want still present", err)
	}
}

func TestCleanupPluginHostRuntimeDirKillsStaleMetadataProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host control sockets use Unix-domain sockets")
	}
	runtimeDir, err := os.MkdirTemp("", "phs-")
	if err != nil {
		t.Fatalf("MkdirTemp(runtime dir) error = %v", err)
	}
	defer os.RemoveAll(runtimeDir)
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sleep) error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	socketPath := filepath.Join(runtimeDir, "plugin-host-orphan.sock")
	metadataPath := filepath.Join(runtimeDir, "plugin-host-orphan.json")
	if err := writePluginHostMetadata(metadataPath, pluginHostMetadata{
		SchemaVersion: pluginHostMetadataSchemaVersion,
		PluginID:      "orphan",
		ArtifactID:    "artifact-orphan",
		PID:           cmd.Process.Pid,
		SocketPath:    socketPath,
		StartedAt:     old.Unix(),
	}); err != nil {
		t.Fatalf("writePluginHostMetadata(orphan) error = %v", err)
	}
	if err := os.Chtimes(metadataPath, old, old); err != nil {
		t.Fatalf("Chtimes(orphan metadata) error = %v", err)
	}

	removed, err := cleanupPluginHostRuntimeDir(runtimeDir, time.Hour, now)
	if err != nil {
		t.Fatalf("cleanupPluginHostRuntimeDir(orphan) error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanupPluginHostRuntimeDir(orphan) removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("Lstat(orphan metadata) error = %v, want not exist", err)
	}
	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatal("stale metadata process was not killed")
	}
}

func TestCleanupPluginHostRuntimeDirKillsUntrackedActiveSocketProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin-host control sockets use Unix-domain sockets")
	}
	runtimeDir, err := os.MkdirTemp("", "phs-")
	if err != nil {
		t.Fatalf("MkdirTemp(runtime dir) error = %v", err)
	}
	defer os.RemoveAll(runtimeDir)
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sleep) error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	socketPath := filepath.Join(runtimeDir, "plugin-host-untracked.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(untracked unix) error = %v", err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
				var req PluginHostControlRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				if req.Command != PluginHostCommandHandshake {
					return
				}
				handshake := NewPluginHostHandshake(PluginHostProtocol, cmd.Process.Pid, 1, time.Now().Unix())
				_ = json.NewEncoder(conn).Encode(PluginHostControlResponse{
					OK:        true,
					Handshake: &handshake,
				})
			}(conn)
		}
	}()
	defer func() {
		_ = listener.Close()
		<-acceptDone
	}()

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(socketPath, old, old); err != nil {
		t.Fatalf("Chtimes(untracked socket) error = %v", err)
	}
	removed, err := cleanupPluginHostRuntimeDir(runtimeDir, time.Hour, now)
	if err != nil {
		t.Fatalf("cleanupPluginHostRuntimeDir(untracked active) error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanupPluginHostRuntimeDir(untracked active) removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("Lstat(untracked socket) error = %v, want not exist", err)
	}
	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatal("untracked active socket process was not killed")
	}
}

func TestCleanupPluginHostRuntimeDirKillsProcessTableOrphan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-table plugin-host discovery uses /proc")
	}
	runtimeDir, err := os.MkdirTemp("", "phs-")
	if err != nil {
		t.Fatalf("MkdirTemp(runtime dir) error = %v", err)
	}
	defer os.RemoveAll(runtimeDir)
	socketPath := filepath.Join(runtimeDir, "plugin-host-proc-orphan.sock")
	cmd := exec.Command(os.Args[0],
		"-test.run=TestPluginHostSupervisorHelperProcess",
		"--",
		"plugin-host",
		"serve",
		"--control-socket",
		socketPath,
	)
	cmd.Env = append(os.Environ(), "MC_GATEWAY_PLUGIN_HOST_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(helper process) error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "cmdline")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper process cmdline did not become visible")
		}
		time.Sleep(10 * time.Millisecond)
	}
	removed, err := cleanupPluginHostRuntimeDir(runtimeDir, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("cleanupPluginHostRuntimeDir(process table) error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanupPluginHostRuntimeDir(process table) removed = %d, want 1", removed)
	}
	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatal("process-table orphan was not killed")
	}
}

func TestPluginHostSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("MC_GATEWAY_PLUGIN_HOST_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}
