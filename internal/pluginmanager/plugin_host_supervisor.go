package pluginmanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	defaultPluginHostStartTimeout       = 5 * time.Second
	defaultPluginHostOrphanSocketMaxAge = time.Hour
	pluginHostMetadataSchemaVersion     = "mc-gateway.plugin-host-runtime/v1"
)

type PluginHostSupervisor struct {
	Executable   string
	ArgsPrefix   []string
	RuntimeDir   string
	Protocol     string
	StartTimeout time.Duration
}

type PluginHostSupervisorProcess struct {
	PluginID     string
	ArtifactID   string
	SocketPath   string
	MetadataPath string
	Protocol     string
	PID          int
	StartedAt    int64

	cmd  *exec.Cmd
	done chan struct{}

	mu          sync.Mutex
	stopping    bool
	exitedAt    int64
	lastCrashAt int64
	lastError   string
	crashCount  int
	crashLoop   bool
	exitErr     error
}

type contextBoundConn struct {
	net.Conn
	stop func()
	once sync.Once
}

func (c *contextBoundConn) Close() error {
	c.once.Do(c.stop)
	return c.Conn.Close()
}

func (c *contextBoundConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (c *contextBoundConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
}

func (s PluginHostSupervisor) Start(ctx context.Context, pluginID, artifactID string) (*PluginHostSupervisorProcess, PluginHostHandshake, error) {
	protocol := s.Protocol
	if protocol == "" {
		protocol = PluginHostProtocol
	}
	if err := ValidatePluginHostProtocol(protocol); err != nil {
		return nil, PluginHostHandshake{}, err
	}
	executable := s.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, PluginHostHandshake{}, err
		}
	}
	runtimeDir := s.RuntimeDir
	if runtimeDir == "" {
		runtimeDir = filepath.Join(os.TempDir(), "mc-gateway-plugin-host")
	}
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, PluginHostHandshake{}, err
	}
	if _, err := cleanupPluginHostRuntimeDir(runtimeDir, defaultPluginHostOrphanSocketMaxAge, time.Now()); err != nil {
		return nil, PluginHostHandshake{}, err
	}
	socketPath, err := pluginHostSocketPath(runtimeDir)
	if err != nil {
		return nil, PluginHostHandshake{}, err
	}
	args := append([]string{}, s.ArgsPrefix...)
	args = append(args, "plugin-host", "serve", "--control-socket", socketPath, "--protocol", protocol)
	cmd := exec.Command(executable, args...)
	if err := cmd.Start(); err != nil {
		return nil, PluginHostHandshake{}, err
	}
	metadataPath := pluginHostMetadataPath(socketPath)
	process := &PluginHostSupervisorProcess{
		PluginID:     pluginID,
		ArtifactID:   artifactID,
		SocketPath:   socketPath,
		MetadataPath: metadataPath,
		Protocol:     protocol,
		PID:          cmd.Process.Pid,
		StartedAt:    time.Now().Unix(),
		cmd:          cmd,
		done:         make(chan struct{}),
	}
	if err := writePluginHostMetadata(metadataPath, pluginHostMetadata{
		SchemaVersion: pluginHostMetadataSchemaVersion,
		PluginID:      pluginID,
		ArtifactID:    artifactID,
		PID:           cmd.Process.Pid,
		SocketPath:    socketPath,
		StartedAt:     process.StartedAt,
	}); err != nil {
		_ = process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
		_ = os.Remove(metadataPath)
		return nil, PluginHostHandshake{}, err
	}
	go process.wait()

	startCtx, cancel := context.WithTimeout(ctx, pluginHostStartTimeout(s.StartTimeout))
	defer cancel()
	if err := waitForPluginHostControl(startCtx, socketPath); err != nil {
		_ = process.Kill()
		<-process.done
		return nil, PluginHostHandshake{}, err
	}
	resp, err := SendPluginHostControlRequest(startCtx, socketPath, PluginHostControlRequest{
		Command:  PluginHostCommandHandshake,
		Protocol: protocol,
	})
	if err != nil {
		_ = process.Kill()
		<-process.done
		return nil, PluginHostHandshake{}, err
	}
	if !resp.OK || resp.Handshake == nil {
		_ = process.Kill()
		<-process.done
		if resp.Error != "" {
			return nil, PluginHostHandshake{}, errors.New(resp.Error)
		}
		return nil, PluginHostHandshake{}, errors.New("plugin-host handshake failed")
	}
	return process, *resp.Handshake, nil
}

func (p *PluginHostSupervisorProcess) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.exitedAt != 0 {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	p.mu.Unlock()

	resp, err := SendPluginHostControlRequest(ctx, p.SocketPath, PluginHostControlRequest{
		Command:  PluginHostCommandShutdown,
		Protocol: p.Protocol,
	})
	if err != nil || !resp.OK {
		_ = p.Kill()
		if err == nil {
			if resp.Error != "" {
				err = errors.New(resp.Error)
			} else {
				err = errors.New("plugin-host shutdown failed")
			}
		}
	}
	select {
	case <-p.done:
		return err
	case <-ctx.Done():
		_ = p.Kill()
		<-p.done
		return ctx.Err()
	}
}

func (p *PluginHostSupervisorProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *PluginHostSupervisorProcess) Summary() PluginHostRuntimeSummary {
	if p == nil {
		return PluginHostRuntimeSummary{State: RuntimeNotLoaded, DrainMode: PluginMigrationDrainOnly}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := RuntimeEnabled
	if p.stopping && p.exitedAt == 0 {
		state = RuntimeDraining
	}
	if p.exitedAt != 0 {
		state = RuntimeDisabled
	}
	if p.crashLoop {
		state = RuntimeFailed
	}
	return PluginHostRuntimeSummary{
		PluginID:    p.PluginID,
		ArtifactID:  p.ArtifactID,
		PID:         p.PID,
		State:       state,
		DrainMode:   PluginMigrationDrainOnly,
		CrashLoop:   p.crashLoop,
		CrashCount:  p.crashCount,
		LastError:   p.lastError,
		StartedAt:   p.StartedAt,
		ExitedAt:    p.exitedAt,
		LastCrashAt: p.lastCrashAt,
	}
}

func (p *PluginHostSupervisorProcess) wait() {
	err := p.cmd.Wait()
	if p.SocketPath != "" {
		_ = os.Remove(p.SocketPath)
	}
	if p.MetadataPath != "" {
		_ = os.Remove(p.MetadataPath)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exitErr = err
	p.exitedAt = time.Now().Unix()
	if !p.stopping {
		p.crashCount++
		p.crashLoop = true
		if err != nil {
			p.lastError = err.Error()
		} else {
			p.lastError = "plugin-host exited unexpectedly"
		}
		p.lastCrashAt = p.exitedAt
	}
	close(p.done)
}

func pluginHostStartTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultPluginHostStartTimeout
	}
	return timeout
}

func pluginHostSocketPath(runtimeDir string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "plugin-host-"+hex.EncodeToString(nonce[:])+".sock"), nil
}

func pluginHostMetadataPath(socketPath string) string {
	if strings.HasSuffix(socketPath, ".sock") {
		return strings.TrimSuffix(socketPath, ".sock") + ".json"
	}
	return socketPath + ".json"
}

type pluginHostMetadata struct {
	SchemaVersion string `json:"schema_version"`
	PluginID      string `json:"plugin_id"`
	ArtifactID    string `json:"artifact_id"`
	PID           int    `json:"pid"`
	SocketPath    string `json:"socket_path"`
	StartedAt     int64  `json:"started_at"`
}

func writePluginHostMetadata(path string, metadata pluginHostMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func cleanupPluginHostRuntimeDir(runtimeDir string, maxAge time.Duration, now time.Time) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "plugin-host-") {
			continue
		}
		path := filepath.Join(runtimeDir, name)
		if strings.HasSuffix(name, ".json") {
			n, err := cleanupPluginHostMetadata(path, maxAge, now)
			removed += n
			if err != nil {
				return removed, err
			}
			continue
		}
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		if info.Mode()&os.ModeSocket == 0 || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if pluginHostSocketActive(path) {
			if pluginHostMetadataExists(path) {
				continue
			}
			cleaned, err := cleanupUntrackedPluginHostSocket(path)
			if err != nil {
				return removed, err
			}
			if cleaned {
				removed++
			}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	n, err := cleanupPluginHostProcessTable(runtimeDir, maxAge, now)
	removed += n
	if err != nil {
		return removed, err
	}
	return removed, nil
}

func cleanupPluginHostMetadata(path string, maxAge time.Duration, now time.Time) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if now.Sub(info.ModTime()) < maxAge {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var metadata pluginHostMetadata
	if err := json.Unmarshal(data, &metadata); err == nil && metadata.SchemaVersion == pluginHostMetadataSchemaVersion {
		if metadata.SocketPath != "" && pluginHostSocketActive(metadata.SocketPath) {
			return 0, nil
		}
		if metadata.PID > 0 {
			if process, err := os.FindProcess(metadata.PID); err == nil {
				_ = process.Kill()
			}
		}
		if metadata.SocketPath != "" {
			_ = os.Remove(metadata.SocketPath)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return 1, nil
}

func pluginHostMetadataExists(socketPath string) bool {
	_, err := os.Stat(pluginHostMetadataPath(socketPath))
	return err == nil
}

func cleanupUntrackedPluginHostSocket(socketPath string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := SendPluginHostControlRequest(ctx, socketPath, PluginHostControlRequest{
		Command:  PluginHostCommandHandshake,
		Protocol: PluginHostProtocol,
	})
	if err != nil || !resp.OK || resp.Handshake == nil || resp.Handshake.PID <= 0 {
		return false, nil
	}
	if resp.Handshake.PID == os.Getpid() || resp.Handshake.PPID == os.Getpid() {
		return false, nil
	}
	if process, err := os.FindProcess(resp.Handshake.PID); err == nil {
		_ = process.Kill()
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func cleanupPluginHostProcessTable(runtimeDir string, maxAge time.Duration, now time.Time) (int, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, nil
	}
	runtimeDir, err = filepath.Abs(runtimeDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		socketPath := pluginHostControlSocketFromCmdline(data)
		if !pluginHostSocketWithinRuntimeDir(runtimeDir, socketPath) || pluginHostMetadataExists(socketPath) {
			continue
		}
		stale := false
		if info, err := os.Stat(socketPath); err == nil {
			stale = now.Sub(info.ModTime()) >= maxAge
		} else if errors.Is(err, os.ErrNotExist) {
			stale = true
		}
		if !stale {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
		if socketPath != "" {
			_ = os.Remove(socketPath)
		}
		removed++
	}
	return removed, nil
}

func PluginHostProcessTableUnsupportedReason() string {
	if runtime.GOOS == "linux" {
		return ""
	}
	return "process-table orphan discovery requires Linux /proc; this platform falls back to metadata-backed sweep, stale control socket handshake, and supervisor-owned Stop/Kill cleanup"
}

func PluginHostProcessTableDiscoveryAvailable() bool {
	return runtime.GOOS == "linux"
}

func pluginHostControlSocketFromCmdline(data []byte) string {
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	hasPluginHostServe := false
	for i := 0; i < len(parts); i++ {
		if i+1 < len(parts) && parts[i] == "plugin-host" && parts[i+1] == "serve" {
			hasPluginHostServe = true
		}
	}
	if !hasPluginHostServe {
		return ""
	}
	for i := 0; i < len(parts); i++ {
		if parts[i] == "--control-socket" && i+1 < len(parts) {
			return parts[i+1]
		}
		if strings.HasPrefix(parts[i], "--control-socket=") {
			return strings.TrimPrefix(parts[i], "--control-socket=")
		}
	}
	return ""
}

func pluginHostSocketWithinRuntimeDir(runtimeDir, socketPath string) bool {
	if socketPath == "" || !strings.HasPrefix(filepath.Base(socketPath), "plugin-host-") || !strings.HasSuffix(socketPath, ".sock") {
		return false
	}
	socketPath, err := filepath.Abs(socketPath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(runtimeDir, socketPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return false
	}
	return true
}

func pluginHostSocketActive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitForPluginHostControl(ctx context.Context, socketPath string) error {
	var lastErr error
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("plugin-host control socket not ready: %w", lastErr)
			}
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func SendPluginHostControlRequest(ctx context.Context, socketPath string, req PluginHostControlRequest) (PluginHostControlResponse, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return PluginHostControlResponse{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return PluginHostControlResponse{}, err
	}
	var resp PluginHostControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return PluginHostControlResponse{}, err
	}
	return resp, nil
}

func DialPluginHostUpstream(ctx context.Context, socketPath string, req PluginHostUpstreamConnectRequest) (net.Conn, error) {
	return dialPluginHostStream(ctx, socketPath, PluginHostCommandUpstream, req)
}

func DialPluginHostStreamProxy(ctx context.Context, socketPath string, req PluginHostUpstreamConnectRequest) (net.Conn, error) {
	return dialPluginHostStream(ctx, socketPath, PluginHostCommandStreamProxy, req)
}

func dialPluginHostStream(ctx context.Context, socketPath, command string, req PluginHostUpstreamConnectRequest) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	stopContextClose := bindConnToContext(ctx, conn)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(PluginHostControlRequest{
		Command:  command,
		Protocol: PluginHostProtocol,
		Payload:  payload,
	}); err != nil {
		stopContextClose()
		_ = conn.Close()
		return nil, err
	}
	var resp PluginHostControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		stopContextClose()
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		stopContextClose()
		_ = conn.Close()
		switch resp.Code {
		case "pass":
			return nil, api.ErrPass
		case "blocked":
			return nil, api.ErrBlocked
		default:
			if resp.Error != "" {
				return nil, errors.New(resp.Error)
			}
			return nil, fmt.Errorf("plugin-host upstream failed with code %q", resp.Code)
		}
	}
	connected := false
	switch command {
	case PluginHostCommandStreamProxy:
		connected = resp.Stream != nil && resp.Stream.Connected
	default:
		connected = resp.Upstream != nil && resp.Upstream.Connected
	}
	if !connected {
		stopContextClose()
		_ = conn.Close()
		return nil, api.ErrPass
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		stopContextClose()
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &contextBoundConn{Conn: conn, stop: stopContextClose}, nil
}

func bindConnToContext(ctx context.Context, conn net.Conn) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return stop
}
