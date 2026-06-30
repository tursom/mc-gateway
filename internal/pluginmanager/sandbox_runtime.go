package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	sandboxProcessProtocol           = SandboxProcessProtocolV1
	sandboxControlChannelUnix        = "sandbox-control-rpc"
	sandboxControlCommandHealth      = "health"
	sandboxControlCommandDiagnostics = "diagnostics"
	sandboxControlCommandSecret      = "secret.resolve"
	sandboxControlCommandStop        = "stop"
)

type SandboxProcessAdapter struct {
	Supervisor SandboxSupervisor
	Policy     SandboxPolicy
	Secrets    SandboxSecretResolver
}

type SandboxSupervisor struct {
	Executable string
	ArgsPrefix []string
	Policy     SandboxPolicy
}

type SandboxProcess struct {
	PluginID   string
	ArtifactID string
	PID        int
	StartedAt  int64
	Policy     SandboxPolicy
	RootDir    string
	SocketPath string

	cmd      *exec.Cmd
	listener net.Listener
	done     chan struct{}

	lastError   string
	crashLoop   bool
	crashCount  int
	exitedAt    int64
	lastCrashAt int64
}

type SandboxSecretResolver interface {
	ResolveSandboxSecret(ctx context.Context, req SandboxSecretRequest) (SandboxSecretResponse, error)
}

type SandboxControlRequest struct {
	Command  string          `json:"command"`
	Protocol string          `json:"protocol,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

type SandboxControlResponse struct {
	OK          bool                      `json:"ok"`
	Code        string                    `json:"code,omitempty"`
	Error       string                    `json:"error,omitempty"`
	Health      *RuntimeHealth            `json:"health,omitempty"`
	Diagnostics *SandboxDiagnosticSummary `json:"diagnostics,omitempty"`
	Secret      *SandboxSecretResponse    `json:"secret,omitempty"`
}

type sandboxHostedPlugin struct {
	process *SandboxProcess
}

func normalizeSandboxPolicy(policy SandboxPolicy) SandboxPolicy {
	if policy.CPUSeconds <= 0 {
		policy.CPUSeconds = 2
	}
	if policy.MemoryBytes <= 0 {
		policy.MemoryBytes = 64 * 1024 * 1024
	}
	if policy.Env == nil {
		policy.Env = map[string]string{}
	}
	policy.FilesystemRoots = append([]string(nil), policy.FilesystemRoots...)
	sort.Strings(policy.FilesystemRoots)
	policy.SecretHandles = append([]string(nil), policy.SecretHandles...)
	sort.Strings(policy.SecretHandles)
	return policy
}

func sandboxPolicyEmpty(policy SandboxPolicy) bool {
	return len(policy.FilesystemRoots) == 0 &&
		!policy.NetworkEnabled &&
		len(policy.Env) == 0 &&
		policy.CPUSeconds == 0 &&
		policy.MemoryBytes == 0 &&
		len(policy.SecretHandles) == 0
}

func validateSandboxPolicyEnforceable(policy SandboxPolicy) error {
	if policy.NetworkEnabled {
		return errors.New("sandbox-process network access cannot be enabled until network policy enforcement is available")
	}
	for key := range policy.Env {
		lower := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(lower, "secret") || strings.Contains(lower, "token") {
			return fmt.Errorf("sandbox-process env key %q looks like secret material; use secret RPC handles instead", key)
		}
	}
	if policy.CPUSeconds <= 0 {
		return errors.New("sandbox-process cpu limit must be positive")
	}
	if policy.MemoryBytes <= 0 {
		return errors.New("sandbox-process memory limit must be positive")
	}
	return nil
}

func defaultSandboxEnvironmentSelfCheck(policy SandboxPolicy) error {
	policy = normalizeSandboxPolicy(policy)
	if err := validateSandboxPolicyEnforceable(policy); err != nil {
		return err
	}
	return validateSandboxEnforcementSupported()
}

func (a SandboxProcessAdapter) Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error) {
	prepared, err := a.Prepare(ctx, artifact, pluginRecord)
	if err != nil {
		return nil, err
	}
	instance, err := a.Start(ctx, prepared, artifact, pluginRecord, gateway)
	if err != nil {
		return nil, err
	}
	return instance.Plugin, nil
}

func (a SandboxProcessAdapter) ValidateArtifact(_ context.Context, artifact ArtifactRecord) error {
	if artifact.RuntimeType != RuntimeSandbox {
		return fmt.Errorf("sandbox-process adapter does not support runtime %q", artifact.RuntimeType)
	}
	if artifact.ArtifactType != ArtifactTypeBinary {
		return fmt.Errorf("sandbox-process adapter requires binary artifact, got %q", artifact.ArtifactType)
	}
	policy := a.Supervisor.Policy
	if sandboxPolicyEmpty(policy) {
		policy = a.Policy
	}
	policy = normalizeSandboxPolicy(policy)
	if err := validateSandboxPolicyEnforceable(policy); err != nil {
		return err
	}
	return validateSandboxEnforcementSupported()
}

func (a SandboxProcessAdapter) Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error) {
	if err := a.ValidateArtifact(ctx, artifact); err != nil {
		return RuntimePrepared{}, err
	}
	return RuntimePrepared{
		PluginID:   pluginRecord.ID,
		ArtifactID: artifact.ID,
		Runtime:    artifact.RuntimeType,
		Mode:       PluginServiceModeSandboxProcess,
		PreparedAt: time.Now().Unix(),
	}, nil
}

func (a SandboxProcessAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, _ *Gateway) (RuntimeInstance, error) {
	supervisor := a.Supervisor
	if sandboxPolicyEmpty(supervisor.Policy) {
		supervisor.Policy = a.Policy
	}
	supervisor.Policy = normalizeSandboxPolicy(supervisor.Policy)
	if err := validateSandboxPolicyEnforceable(supervisor.Policy); err != nil {
		return RuntimeInstance{}, err
	}
	process, err := supervisor.Start(ctx, pluginRecord.ID, artifact.ID, artifact.FilePath, a.Secrets)
	if err != nil {
		return RuntimeInstance{}, err
	}
	return RuntimeInstance{
		RuntimePrepared: prepared,
		Plugin:          sandboxHostedPlugin{process: process},
		StartedAt:       process.StartedAt,
	}, nil
}

func (a SandboxProcessAdapter) HealthCheck(_ context.Context, instance RuntimeInstance) RuntimeHealth {
	now := time.Now().Unix()
	process, _ := instance.Plugin.(sandboxHostedPlugin)
	summary := SandboxDiagnosticSummary{PluginID: instance.PluginID, ArtifactID: instance.ArtifactID, State: RuntimeEnabled}
	if process.process != nil {
		summary = process.process.Diagnostics()
	}
	return RuntimeHealth{
		OK:        !summary.CrashLoop && summary.State != RuntimeFailed,
		Status:    summary.State,
		Error:     summary.LastError,
		Details:   sandboxDiagnosticsDetails(summary),
		CheckedAt: now,
	}
}

func (a SandboxProcessAdapter) ReloadConfig(context.Context, RuntimeInstance, string) error {
	return nil
}

func (a SandboxProcessAdapter) Drain(context.Context, RuntimeInstance) error {
	return nil
}

func (a SandboxProcessAdapter) Stop(ctx context.Context, instance RuntimeInstance) error {
	process, _ := instance.Plugin.(sandboxHostedPlugin)
	if process.process == nil {
		return nil
	}
	return process.process.Stop(ctx)
}

func (a SandboxProcessAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	process, _ := instance.Plugin.(sandboxHostedPlugin)
	summary := SandboxDiagnosticSummary{PluginID: instance.PluginID, ArtifactID: instance.ArtifactID, State: RuntimeNotLoaded}
	if process.process != nil {
		summary = process.process.Diagnostics()
	}
	return RuntimeAdapterDiagnostics{
		PluginID:   instance.PluginID,
		ArtifactID: instance.ArtifactID,
		Runtime:    instance.Runtime,
		Mode:       instance.Mode,
		State:      summary.State,
		Details:    sandboxDiagnosticsDetails(summary),
		CreatedAt:  time.Now().Unix(),
	}
}

func (s SandboxSupervisor) Start(ctx context.Context, pluginID, artifactID, executable string, resolver SandboxSecretResolver) (*SandboxProcess, error) {
	if err := validateSandboxEnforcementSupported(); err != nil {
		return nil, err
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		executable = s.Executable
	}
	if executable == "" {
		return nil, errors.New("sandbox executable is required")
	}
	policy := normalizeSandboxPolicy(s.Policy)
	if err := validateSandboxPolicyEnforceable(policy); err != nil {
		return nil, err
	}
	rootDir, err := prepareSandboxRoot(executable, policy)
	if err != nil {
		return nil, err
	}
	socketPath, err := sandboxControlSocketPath(rootDir)
	if err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	args := append([]string{}, s.ArgsPrefix...)
	cmd := exec.CommandContext(ctx, "/plugin", args...)
	cmd.Env = sandboxEnv(policy)
	configureSandboxCommand(cmd, rootDir)
	cmd.Dir = "/"
	if policy.CPUSeconds > 0 {
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
	}
	if err := cmd.Start(); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	if err := applySandboxRLimits(cmd.Process.Pid, policy); err != nil {
		_ = listener.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	process := &SandboxProcess{
		PluginID:   pluginID,
		ArtifactID: artifactID,
		PID:        cmd.Process.Pid,
		StartedAt:  time.Now().Unix(),
		Policy:     policy,
		RootDir:    rootDir,
		SocketPath: socketPath,
		cmd:        cmd,
		done:       make(chan struct{}),
	}
	if err := process.startControlRPC(ctx, listener, resolver); err != nil {
		_ = process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	go process.wait()
	return process, nil
}

func prepareSandboxRoot(executable string, policy SandboxPolicy) (string, error) {
	rootDir, err := os.MkdirTemp("", "mc-gateway-sandbox-*")
	if err != nil {
		return "", err
	}
	if err := copyFile(executable, filepath.Join(rootDir, "plugin")); err != nil {
		_ = os.RemoveAll(rootDir)
		return "", err
	}
	if err := os.Chmod(filepath.Join(rootDir, "plugin"), 0755); err != nil {
		_ = os.RemoveAll(rootDir)
		return "", err
	}
	for _, dir := range []string{"tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(rootDir, dir), 0700); err != nil {
			_ = os.RemoveAll(rootDir)
			return "", err
		}
	}
	for _, root := range policy.FilesystemRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		clean := strings.TrimPrefix(filepath.Clean(root), string(filepath.Separator))
		if clean == "." || strings.HasPrefix(clean, "..") {
			_ = os.RemoveAll(rootDir)
			return "", fmt.Errorf("sandbox filesystem root %q escapes sandbox root", root)
		}
		if err := os.MkdirAll(filepath.Join(rootDir, clean), 0700); err != nil {
			_ = os.RemoveAll(rootDir)
			return "", err
		}
	}
	return rootDir, nil
}

func sandboxControlSocketPath(rootDir string) (string, error) {
	if strings.TrimSpace(rootDir) == "" {
		return "", errors.New("sandbox root is required")
	}
	runDir := filepath.Join(rootDir, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(runDir, "control.sock"), nil
}

func sandboxEnv(policy SandboxPolicy) []string {
	keys := make([]string, 0, len(policy.Env))
	for key := range policy.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)+1)
	out = append(out, "MC_GATEWAY_SANDBOX=1")
	out = append(out, "MC_GATEWAY_SANDBOX_CONTROL=unix:///run/control.sock")
	for _, key := range keys {
		if strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "token") {
			continue
		}
		out = append(out, key+"="+policy.Env[key])
	}
	return out
}

func (p *SandboxProcess) startControlRPC(ctx context.Context, listener net.Listener, resolver SandboxSecretResolver) error {
	if p == nil || strings.TrimSpace(p.SocketPath) == "" {
		return errors.New("sandbox control socket path is required")
	}
	if listener == nil {
		var err error
		listener, err = net.Listen("unix", p.SocketPath)
		if err != nil {
			return err
		}
	}
	p.listener = listener
	go func() {
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-p.done:
				case <-ctx.Done():
				default:
				}
				return
			}
			go p.handleControlConn(ctx, conn, resolver)
		}
	}()
	return nil
}

func (p *SandboxProcess) handleControlConn(ctx context.Context, conn net.Conn, resolver SandboxSecretResolver) {
	defer conn.Close()
	var req SandboxControlRequest
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(SandboxControlResponse{OK: false, Code: "invalid_json", Error: err.Error()})
		return
	}
	resp := p.HandleControlRequest(ctx, req, resolver)
	_ = encoder.Encode(resp)
}

func (p *SandboxProcess) HandleControlRequest(ctx context.Context, req SandboxControlRequest, resolver SandboxSecretResolver) SandboxControlResponse {
	protocol := strings.TrimSpace(req.Protocol)
	if protocol != "" && protocol != sandboxProcessProtocol {
		return SandboxControlResponse{OK: false, Code: "invalid_protocol", Error: fmt.Sprintf("unsupported sandbox protocol %q", protocol)}
	}
	switch strings.TrimSpace(req.Command) {
	case sandboxControlCommandHealth:
		diag := p.Diagnostics()
		health := RuntimeHealth{
			OK:        !diag.CrashLoop && diag.State != RuntimeFailed,
			Status:    diag.State,
			Error:     diag.LastError,
			Details:   sandboxDiagnosticsDetails(diag),
			CheckedAt: time.Now().Unix(),
		}
		return SandboxControlResponse{OK: true, Health: &health}
	case sandboxControlCommandDiagnostics:
		diag := p.Diagnostics()
		return SandboxControlResponse{OK: true, Diagnostics: &diag}
	case sandboxControlCommandSecret:
		if resolver == nil {
			return SandboxControlResponse{OK: false, Code: "secret_resolver_missing", Error: "sandbox secret resolver is not configured"}
		}
		var payload SandboxSecretRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return SandboxControlResponse{OK: false, Code: "invalid_request", Error: err.Error()}
		}
		if strings.TrimSpace(payload.PluginID) == "" {
			payload.PluginID = p.PluginID
		}
		secret, err := resolver.ResolveSandboxSecret(ctx, payload)
		if err != nil {
			return SandboxControlResponse{OK: false, Code: "secret_resolve_failed", Error: err.Error()}
		}
		return SandboxControlResponse{OK: secret.OK, Secret: &secret, Error: secret.Error}
	case sandboxControlCommandStop:
		return SandboxControlResponse{OK: true}
	default:
		return SandboxControlResponse{OK: false, Code: "unknown_command", Error: "unsupported sandbox control command"}
	}
}

func (p *SandboxProcess) Stop(ctx context.Context) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
		<-p.done
		return ctx.Err()
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		return nil
	}
}

func (p *SandboxProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	return p.cmd.Process.Kill()
}

func (p *SandboxProcess) wait() {
	err := p.cmd.Wait()
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.exitedAt = time.Now().Unix()
	if err != nil {
		p.lastError = err.Error()
		p.crashLoop = true
		p.crashCount = 1
		p.lastCrashAt = p.exitedAt
	}
	if p.SocketPath != "" {
		_ = os.Remove(p.SocketPath)
	}
	if p.RootDir != "" {
		_ = os.RemoveAll(p.RootDir)
	}
	close(p.done)
}

func (p *SandboxProcess) Diagnostics() SandboxDiagnosticSummary {
	if p == nil {
		return SandboxDiagnosticSummary{State: RuntimeNotLoaded}
	}
	state := RuntimeEnabled
	if p.exitedAt != 0 {
		state = RuntimeDisabled
	}
	if p.crashLoop {
		state = RuntimeFailed
	}
	policy := normalizeSandboxPolicy(p.Policy)
	envKeys := make([]string, 0, len(policy.Env))
	for key := range policy.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	return SandboxDiagnosticSummary{
		PluginID:           p.PluginID,
		ArtifactID:         p.ArtifactID,
		PID:                p.PID,
		State:              state,
		ControlRPC:         true,
		FilesystemEnforced: true,
		NetworkEnforced:    !policy.NetworkEnabled,
		EnvEnforced:        true,
		CPUMemoryEnforced:  true,
		SecretRPC:          true,
		CrashLoop:          p.crashLoop,
		CrashCount:         p.crashCount,
		LastError:          p.lastError,
		SecretHandles:      append([]string(nil), policy.SecretHandles...),
		EnvKeys:            envKeys,
		ControlSocket:      "unix:///run/control.sock",
		EnforcementAttributes: map[string]string{
			"control_channel": sandboxControlChannelUnix,
			"protocol":        sandboxProcessProtocol,
			"filesystem":      "chroot-staged-root-with-explicit-roots",
			"network":         "newnet-without-host-network-by-default",
			"environment":     "explicit-allowlist-no-secret-env",
			"cpu_memory":      "process-rlimit-policy",
			"secret_delivery": "handle-rpc-version-only",
		},
	}
}

func sandboxDiagnosticsDetails(summary SandboxDiagnosticSummary) map[string]any {
	return map[string]any{
		"protocol":               sandboxProcessProtocol,
		"control_channel":        sandboxControlChannelUnix,
		"pid":                    summary.PID,
		"control_rpc":            summary.ControlRPC,
		"filesystem_enforced":    summary.FilesystemEnforced,
		"network_enforced":       summary.NetworkEnforced,
		"env_enforced":           summary.EnvEnforced,
		"cpu_memory_enforced":    summary.CPUMemoryEnforced,
		"secret_rpc":             summary.SecretRPC,
		"crash_loop":             summary.CrashLoop,
		"crash_count":            summary.CrashCount,
		"last_error":             summary.LastError,
		"secret_handles":         append([]string(nil), summary.SecretHandles...),
		"env_keys":               append([]string(nil), summary.EnvKeys...),
		"control_socket":         summary.ControlSocket,
		"enforcement_attributes": summary.EnforcementAttributes,
	}
}

func (p sandboxHostedPlugin) Init(api.Gateway) error { return nil }

func (p sandboxHostedPlugin) Destroy() error { return nil }

func (p sandboxHostedPlugin) NewConfigObj() any { return struct{}{} }

func (p sandboxHostedPlugin) ReloadConfig(any) error { return nil }

func (m *Manager) ResolveSandboxSecret(ctx context.Context, req SandboxSecretRequest) (SandboxSecretResponse, error) {
	if strings.TrimSpace(req.PluginID) == "" || strings.TrimSpace(req.Handle) == "" {
		return SandboxSecretResponse{OK: false, Error: "plugin_id and handle are required"}, nil
	}
	secrets, err := m.repo.ListSecrets(ctx, req.PluginID)
	if err != nil {
		return SandboxSecretResponse{}, err
	}
	for _, secret := range secrets {
		if secret.Name == req.Handle && secret.CurrentVersion > 0 {
			return SandboxSecretResponse{OK: true, Version: secret.CurrentVersion}, nil
		}
	}
	return SandboxSecretResponse{OK: false, Error: "secret handle is not authorized or configured"}, nil
}
