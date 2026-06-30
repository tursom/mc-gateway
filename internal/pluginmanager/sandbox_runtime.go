package pluginmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	sandboxProcessProtocol               = SandboxProcessProtocolV1
	sandboxControlChannelUnix            = "sandbox-control-rpc"
	sandboxControlMaxFrameBytes          = 64 * 1024
	sandboxControlMaxPayloadBytes        = 32 * 1024
	defaultSandboxControlStartupTimeout  = 5 * time.Second
	sandboxControlCommandHandshake       = "handshake"
	sandboxControlCommandInit            = "init"
	sandboxControlCommandRegister        = "register"
	sandboxControlCommandReloadConfig    = "reload_config"
	sandboxControlCommandInvoke          = "invoke"
	sandboxControlCommandStreamOpen      = "stream_open"
	sandboxControlCommandStreamClose     = "stream_close"
	sandboxControlCommandMetrics         = "metrics"
	sandboxControlCommandDrain           = "drain"
	sandboxControlCommandStop            = "stop"
	sandboxControlCommandHealth          = "health"
	sandboxControlCommandDiagnostics     = "diagnostics"
	sandboxControlCommandSecret          = "secret.resolve"
	sandboxControlErrorProtocolMismatch  = "protocol_mismatch"
	sandboxControlErrorABIMismatch       = "abi_mismatch"
	sandboxControlErrorUnknownCommand    = "unknown_command"
	sandboxControlErrorTimeout           = "timeout"
	sandboxControlErrorBadJSON           = "bad_json"
	sandboxControlErrorOversizedPayload  = "oversized_payload"
	sandboxControlErrorSchemaInvalid     = "schema_invalid"
	sandboxControlErrorSequenceInvalid   = "sequence_invalid"
	sandboxControlErrorInitFailed        = "init_failed"
	sandboxControlErrorRegisterFailed    = "register_failed"
	sandboxControlErrorNotImplemented    = "not_implemented"
	sandboxControlErrorSecretUnavailable = "secret_unavailable"
)

type SandboxProcessAdapter struct {
	Supervisor SandboxSupervisor
	Policy     SandboxPolicy
	Secrets    SandboxSecretResolver

	startProcess sandboxProcessStarter
}

type SandboxSupervisor struct {
	Executable     string
	ArgsPrefix     []string
	Policy         SandboxPolicy
	StartupTimeout time.Duration
}

type SandboxProcess struct {
	PluginID          string
	ArtifactID        string
	RuntimeInstanceID string
	Generation        int64
	Protocol          string
	PID               int
	StartedAt         int64
	Policy            SandboxPolicy
	RootDir           string
	SocketPath        string
	ConfigJSON        string

	Registrations []SandboxHandlerRegistration

	cmd      *exec.Cmd
	listener net.Listener
	done     chan struct{}

	startupMu        sync.Mutex
	startupDone      chan struct{}
	startupClosed    bool
	startupErr       error
	handshakeOK      bool
	initOK           bool
	registerOK       bool
	startupErrorCode string

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
	RequestID         string          `json:"request_id,omitempty"`
	Command           string          `json:"command"`
	Protocol          string          `json:"protocol"`
	PluginID          string          `json:"plugin_id,omitempty"`
	ArtifactID        string          `json:"artifact_id,omitempty"`
	RuntimeInstanceID string          `json:"runtime_instance_id,omitempty"`
	Generation        int64           `json:"generation"`
	TraceID           string          `json:"trace_id,omitempty"`
	DeadlineUnixMS    int64           `json:"deadline,omitempty"`
	ErrorCode         string          `json:"error_code,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
}

type SandboxControlResponse struct {
	RequestID         string                    `json:"request_id,omitempty"`
	Command           string                    `json:"command,omitempty"`
	Protocol          string                    `json:"protocol"`
	PluginID          string                    `json:"plugin_id,omitempty"`
	ArtifactID        string                    `json:"artifact_id,omitempty"`
	RuntimeInstanceID string                    `json:"runtime_instance_id,omitempty"`
	Generation        int64                     `json:"generation"`
	TraceID           string                    `json:"trace_id,omitempty"`
	DeadlineUnixMS    int64                     `json:"deadline,omitempty"`
	OK                bool                      `json:"ok"`
	Code              string                    `json:"code,omitempty"`
	ErrorCode         string                    `json:"error_code,omitempty"`
	Error             string                    `json:"error,omitempty"`
	Handshake         *SandboxHandshakeResponse `json:"handshake,omitempty"`
	Init              *SandboxInitResponse      `json:"init,omitempty"`
	Register          *SandboxRegisterResponse  `json:"register,omitempty"`
	Metrics           *SandboxMetricsResponse   `json:"metrics,omitempty"`
	Health            *RuntimeHealth            `json:"health,omitempty"`
	Diagnostics       *SandboxDiagnosticSummary `json:"diagnostics,omitempty"`
	Secret            *SandboxSecretResponse    `json:"secret,omitempty"`
}

type sandboxHostedPlugin struct {
	process *SandboxProcess
}

type sandboxProcessStarter func(ctx context.Context, supervisor SandboxSupervisor, pluginID, artifactID, executable string, generation int64, configJSON string, resolver SandboxSecretResolver) (*SandboxProcess, error)

type sandboxControlError struct {
	code    string
	message string
}

func (e sandboxControlError) Error() string {
	if e.message == "" {
		return e.code
	}
	return e.code + ": " + e.message
}

type SandboxHandshakeRequest struct {
	ABIVersion   string   `json:"abi_version"`
	PID          int      `json:"pid,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type SandboxHandshakeResponse struct {
	Protocol          string   `json:"protocol"`
	ABIVersion        string   `json:"abi_version"`
	RuntimeInstanceID string   `json:"runtime_instance_id"`
	ControlChannel    string   `json:"control_channel"`
	Commands          []string `json:"commands"`
	Capabilities      []string `json:"capabilities,omitempty"`
}

type SandboxInitRequest struct {
	Capabilities []string `json:"capabilities,omitempty"`
}

type SandboxInitResponse struct {
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	Generation        int64  `json:"generation"`
	ConfigJSON        string `json:"config_json,omitempty"`
	State             string `json:"state"`
}

type SandboxRegisterRequest struct {
	ExtensionPoint       string                       `json:"extension_point,omitempty"`
	HandlerID            string                       `json:"handler_id,omitempty"`
	FailPolicy           string                       `json:"fail_policy,omitempty"`
	TimeoutMS            int64                        `json:"timeout_ms,omitempty"`
	SchemaVersion        int                          `json:"schema_version,omitempty"`
	DeclaredCapabilities []string                     `json:"declared_capabilities,omitempty"`
	Handlers             []SandboxHandlerRegistration `json:"handlers,omitempty"`
}

type SandboxHandlerRegistration struct {
	ExtensionPoint       string   `json:"extension_point"`
	HandlerID            string   `json:"handler_id"`
	FailPolicy           string   `json:"fail_policy"`
	TimeoutMS            int64    `json:"timeout_ms"`
	SchemaVersion        int      `json:"schema_version"`
	DeclaredCapabilities []string `json:"declared_capabilities,omitempty"`
}

type SandboxRegisterResponse struct {
	Registrations        []SandboxHandlerRegistration `json:"registrations"`
	DeclaredCapabilities []string                     `json:"declared_capabilities,omitempty"`
}

type SandboxReloadConfigRequest struct {
	ConfigJSON string `json:"config_json,omitempty"`
}

type SandboxMetricsResponse struct {
	RuntimeInstanceID  string `json:"runtime_instance_id"`
	RegisteredHandlers int    `json:"registered_handlers"`
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

func ValidateSandboxProcessProtocol(protocol string) error {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return errors.New("sandbox-process protocol is required")
	}
	if protocol != sandboxProcessProtocol {
		return fmt.Errorf("unsupported sandbox-process protocol %q", protocol)
	}
	return nil
}

func sandboxControlCommands() []string {
	return []string{
		sandboxControlCommandHandshake,
		sandboxControlCommandInit,
		sandboxControlCommandRegister,
		sandboxControlCommandReloadConfig,
		sandboxControlCommandInvoke,
		sandboxControlCommandStreamOpen,
		sandboxControlCommandStreamClose,
		sandboxControlCommandMetrics,
		sandboxControlCommandDrain,
		sandboxControlCommandStop,
	}
}

func sandboxControlCapabilities() []string {
	return []string{
		"control.handshake",
		"control.init",
		"control.register",
		"control.reload_config",
		"control.metrics",
		"control.drain",
		"control.stop",
		"secret.handle",
	}
}

func sandboxControlStartupTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultSandboxControlStartupTimeout
	}
	return timeout
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
	startProcess := a.startProcess
	if startProcess == nil {
		startProcess = func(ctx context.Context, supervisor SandboxSupervisor, pluginID, artifactID, executable string, generation int64, configJSON string, resolver SandboxSecretResolver) (*SandboxProcess, error) {
			return supervisor.Start(ctx, pluginID, artifactID, executable, generation, configJSON, resolver)
		}
	}
	process, err := startProcess(ctx, supervisor, pluginRecord.ID, artifact.ID, artifact.FilePath, pluginRecord.DesiredGeneration, pluginRecord.ConfigJSON, a.Secrets)
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

func (s SandboxSupervisor) Start(ctx context.Context, pluginID, artifactID, executable string, generation int64, configJSON string, resolver SandboxSecretResolver) (*SandboxProcess, error) {
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
	runtimeInstanceID := newSandboxRuntimeInstanceID()
	args := append([]string{}, s.ArgsPrefix...)
	cmd := exec.CommandContext(ctx, "/plugin", args...)
	cmd.Env = sandboxProcessEnv(policy, pluginID, artifactID, runtimeInstanceID, generation)
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
		PluginID:          pluginID,
		ArtifactID:        artifactID,
		RuntimeInstanceID: runtimeInstanceID,
		Generation:        generation,
		Protocol:          sandboxProcessProtocol,
		PID:               cmd.Process.Pid,
		StartedAt:         time.Now().Unix(),
		Policy:            policy,
		RootDir:           rootDir,
		SocketPath:        socketPath,
		ConfigJSON:        configJSON,
		cmd:               cmd,
		done:              make(chan struct{}),
		startupDone:       make(chan struct{}),
	}
	if err := process.startControlRPC(ctx, listener, resolver); err != nil {
		_ = process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	go process.wait()
	startupCtx, cancel := context.WithTimeout(ctx, sandboxControlStartupTimeout(s.StartupTimeout))
	defer cancel()
	if err := process.waitForStartup(startupCtx); err != nil {
		_ = process.Kill()
		select {
		case <-process.done:
		case <-time.After(time.Second):
		}
		return nil, err
	}
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
	return sandboxProcessEnv(policy, "", "", "", 0)
}

func sandboxProcessEnv(policy SandboxPolicy, pluginID, artifactID, runtimeInstanceID string, generation int64) []string {
	keys := make([]string, 0, len(policy.Env))
	for key := range policy.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)+1)
	out = append(out, "MC_GATEWAY_SANDBOX=1")
	out = append(out, "MC_GATEWAY_SANDBOX_CONTROL=unix:///run/control.sock")
	out = append(out, "MC_GATEWAY_SANDBOX_PROTOCOL="+sandboxProcessProtocol)
	out = append(out, "MC_GATEWAY_SANDBOX_ABI_VERSION="+SandboxProcessABIVersionV1)
	if strings.TrimSpace(pluginID) != "" {
		out = append(out, "MC_GATEWAY_PLUGIN_ID="+pluginID)
	}
	if strings.TrimSpace(artifactID) != "" {
		out = append(out, "MC_GATEWAY_ARTIFACT_ID="+artifactID)
	}
	if strings.TrimSpace(runtimeInstanceID) != "" {
		out = append(out, "MC_GATEWAY_RUNTIME_INSTANCE_ID="+runtimeInstanceID)
	}
	out = append(out, fmt.Sprintf("MC_GATEWAY_GENERATION=%d", generation))
	for _, key := range keys {
		if strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "token") {
			continue
		}
		out = append(out, key+"="+policy.Env[key])
	}
	return out
}

func newSandboxRuntimeInstanceID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "sandbox-" + hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("sandbox-%d", time.Now().UnixNano())
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
	encoder := json.NewEncoder(conn)
	limited := &io.LimitedReader{R: conn, N: sandboxControlMaxFrameBytes + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&req); err != nil {
		code := sandboxControlErrorBadJSON
		if limited.N <= 0 {
			code = sandboxControlErrorOversizedPayload
		}
		_ = encoder.Encode(sandboxControlErrorResponse(req, nil, code, err.Error()))
		return
	}
	if limited.N <= 0 {
		_ = encoder.Encode(sandboxControlErrorResponse(req, p, sandboxControlErrorOversizedPayload, "sandbox control request exceeds maximum frame size"))
		return
	}
	resp := p.HandleControlRequest(ctx, req, resolver)
	_ = encoder.Encode(resp)
}

func (p *SandboxProcess) HandleControlRequest(ctx context.Context, req SandboxControlRequest, resolver SandboxSecretResolver) SandboxControlResponse {
	command := strings.TrimSpace(req.Command)
	resp := p.newSandboxControlResponse(req)
	if err := validateSandboxControlEnvelope(p, command, req); err != nil {
		return p.finishSandboxControlError(command, resp, err.code, err.message)
	}
	if len(req.Payload) > sandboxControlMaxPayloadBytes {
		return p.finishSandboxControlError(command, resp, sandboxControlErrorOversizedPayload, "sandbox control payload exceeds maximum size")
	}
	switch command {
	case sandboxControlCommandHandshake:
		resp = p.handleSandboxHandshake(req, resp)
	case sandboxControlCommandInit:
		resp = p.handleSandboxInit(req, resp)
	case sandboxControlCommandRegister:
		resp = p.handleSandboxRegister(req, resp)
	case sandboxControlCommandReloadConfig:
		resp = p.handleSandboxReloadConfig(req, resp)
	case sandboxControlCommandInvoke, sandboxControlCommandStreamOpen, sandboxControlCommandStreamClose:
		resp = setSandboxControlError(resp, sandboxControlErrorNotImplemented, command+" is reserved for the sandbox data-plane slice")
	case sandboxControlCommandMetrics:
		resp.OK = true
		if p != nil {
			resp.Metrics = &SandboxMetricsResponse{RuntimeInstanceID: p.RuntimeInstanceID, RegisteredHandlers: len(p.Registrations)}
		} else {
			resp.Metrics = &SandboxMetricsResponse{}
		}
	case sandboxControlCommandDrain:
		resp.OK = true
	case sandboxControlCommandHealth:
		diag := p.Diagnostics()
		health := RuntimeHealth{
			OK:        !diag.CrashLoop && diag.State != RuntimeFailed,
			Status:    diag.State,
			Error:     diag.LastError,
			Details:   sandboxDiagnosticsDetails(diag),
			CheckedAt: time.Now().Unix(),
		}
		resp.OK = true
		resp.Health = &health
	case sandboxControlCommandDiagnostics:
		diag := p.Diagnostics()
		resp.OK = true
		resp.Diagnostics = &diag
	case sandboxControlCommandSecret:
		if resolver == nil {
			resp = setSandboxControlError(resp, sandboxControlErrorSecretUnavailable, "sandbox secret resolver is not configured")
			break
		}
		var payload SandboxSecretRequest
		if err := decodeSandboxControlPayload(req.Payload, &payload); err != nil {
			resp = setSandboxControlError(resp, sandboxControlErrorSchemaInvalid, err.Error())
			break
		}
		if strings.TrimSpace(payload.PluginID) == "" {
			payload.PluginID = p.PluginID
		}
		secret, err := resolver.ResolveSandboxSecret(ctx, payload)
		if err != nil {
			resp = setSandboxControlError(resp, sandboxControlErrorSecretUnavailable, err.Error())
			break
		}
		resp.OK = secret.OK
		resp.Secret = &secret
		resp.Error = redactSandboxControlMessage(secret.Error)
		if !secret.OK && resp.ErrorCode == "" {
			resp.Code = sandboxControlErrorSecretUnavailable
			resp.ErrorCode = sandboxControlErrorSecretUnavailable
		}
	case sandboxControlCommandStop:
		resp.OK = true
	default:
		resp = setSandboxControlError(resp, sandboxControlErrorUnknownCommand, "unsupported sandbox control command")
	}
	if !resp.OK && resp.ErrorCode == "" && resp.Code != "" {
		resp.ErrorCode = resp.Code
	}
	if !resp.OK {
		p.failSandboxStartupCommand(command, resp.ErrorCode, resp.Error)
	}
	return resp
}

func (p *SandboxProcess) newSandboxControlResponse(req SandboxControlRequest) SandboxControlResponse {
	resp := SandboxControlResponse{
		RequestID:         req.RequestID,
		Command:           strings.TrimSpace(req.Command),
		Protocol:          sandboxProcessProtocol,
		PluginID:          req.PluginID,
		ArtifactID:        req.ArtifactID,
		RuntimeInstanceID: req.RuntimeInstanceID,
		Generation:        req.Generation,
		TraceID:           req.TraceID,
		DeadlineUnixMS:    req.DeadlineUnixMS,
	}
	if p != nil {
		if resp.PluginID == "" {
			resp.PluginID = p.PluginID
		}
		if resp.ArtifactID == "" {
			resp.ArtifactID = p.ArtifactID
		}
		if resp.RuntimeInstanceID == "" {
			resp.RuntimeInstanceID = p.RuntimeInstanceID
		}
		if resp.Generation == 0 {
			resp.Generation = p.Generation
		}
		if p.Protocol != "" {
			resp.Protocol = p.Protocol
		}
	}
	return resp
}

func validateSandboxControlEnvelope(p *SandboxProcess, command string, req SandboxControlRequest) *sandboxControlError {
	if err := ValidateSandboxProcessProtocol(req.Protocol); err != nil {
		return &sandboxControlError{code: sandboxControlErrorProtocolMismatch, message: err.Error()}
	}
	if req.DeadlineUnixMS > 0 && time.Now().UnixMilli() > req.DeadlineUnixMS {
		return &sandboxControlError{code: sandboxControlErrorTimeout, message: "sandbox control request deadline exceeded"}
	}
	if !sandboxControlCommandKnown(command) {
		return &sandboxControlError{code: sandboxControlErrorUnknownCommand, message: "unsupported sandbox control command"}
	}
	if !sandboxControlCommandRequiresIdentity(command) || p == nil {
		return nil
	}
	if strings.TrimSpace(req.PluginID) == "" {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: "plugin_id is required"}
	}
	if strings.TrimSpace(req.ArtifactID) == "" {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: "artifact_id is required"}
	}
	if strings.TrimSpace(req.RuntimeInstanceID) == "" {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: "runtime_instance_id is required"}
	}
	if req.PluginID != p.PluginID {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: fmt.Sprintf("plugin_id %q does not match sandbox process", req.PluginID)}
	}
	if req.ArtifactID != p.ArtifactID {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: fmt.Sprintf("artifact_id %q does not match sandbox process", req.ArtifactID)}
	}
	if p.RuntimeInstanceID != "" && req.RuntimeInstanceID != p.RuntimeInstanceID {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: fmt.Sprintf("runtime_instance_id %q does not match sandbox process", req.RuntimeInstanceID)}
	}
	if req.Generation != p.Generation {
		return &sandboxControlError{code: sandboxControlErrorSchemaInvalid, message: fmt.Sprintf("generation %d does not match sandbox process generation %d", req.Generation, p.Generation)}
	}
	return nil
}

func sandboxControlCommandKnown(command string) bool {
	switch command {
	case sandboxControlCommandHandshake,
		sandboxControlCommandInit,
		sandboxControlCommandRegister,
		sandboxControlCommandReloadConfig,
		sandboxControlCommandInvoke,
		sandboxControlCommandStreamOpen,
		sandboxControlCommandStreamClose,
		sandboxControlCommandMetrics,
		sandboxControlCommandDrain,
		sandboxControlCommandStop,
		sandboxControlCommandHealth,
		sandboxControlCommandDiagnostics,
		sandboxControlCommandSecret:
		return true
	default:
		return false
	}
}

func sandboxControlCommandRequiresIdentity(command string) bool {
	switch command {
	case sandboxControlCommandHandshake,
		sandboxControlCommandInit,
		sandboxControlCommandRegister,
		sandboxControlCommandReloadConfig,
		sandboxControlCommandInvoke,
		sandboxControlCommandStreamOpen,
		sandboxControlCommandStreamClose,
		sandboxControlCommandMetrics,
		sandboxControlCommandDrain,
		sandboxControlCommandStop,
		sandboxControlCommandSecret:
		return true
	default:
		return false
	}
}

func (p *SandboxProcess) finishSandboxControlError(command string, resp SandboxControlResponse, code, message string) SandboxControlResponse {
	resp = setSandboxControlError(resp, code, message)
	p.failSandboxStartupCommand(command, resp.ErrorCode, resp.Error)
	return resp
}

func setSandboxControlError(resp SandboxControlResponse, code, message string) SandboxControlResponse {
	resp.OK = false
	resp.Code = code
	resp.ErrorCode = code
	resp.Error = redactSandboxControlMessage(message)
	return resp
}

func sandboxControlErrorResponse(req SandboxControlRequest, p *SandboxProcess, code, message string) SandboxControlResponse {
	resp := SandboxControlResponse{
		RequestID:         req.RequestID,
		Command:           strings.TrimSpace(req.Command),
		Protocol:          sandboxProcessProtocol,
		PluginID:          req.PluginID,
		ArtifactID:        req.ArtifactID,
		RuntimeInstanceID: req.RuntimeInstanceID,
		Generation:        req.Generation,
		TraceID:           req.TraceID,
		DeadlineUnixMS:    req.DeadlineUnixMS,
	}
	if p != nil {
		resp = p.newSandboxControlResponse(req)
	}
	return setSandboxControlError(resp, code, message)
}

func (p *SandboxProcess) handleSandboxHandshake(req SandboxControlRequest, resp SandboxControlResponse) SandboxControlResponse {
	var payload SandboxHandshakeRequest
	if err := decodeSandboxControlPayload(req.Payload, &payload); err != nil {
		return setSandboxControlError(resp, sandboxControlErrorSchemaInvalid, err.Error())
	}
	if strings.TrimSpace(payload.ABIVersion) == "" {
		return setSandboxControlError(resp, sandboxControlErrorABIMismatch, "sandbox abi_version is required")
	}
	if payload.ABIVersion != SandboxProcessABIVersionV1 {
		return setSandboxControlError(resp, sandboxControlErrorABIMismatch, fmt.Sprintf("unsupported sandbox abi_version %q", payload.ABIVersion))
	}
	if p != nil {
		p.startupMu.Lock()
		p.handshakeOK = true
		p.startupMu.Unlock()
	}
	resp.OK = true
	resp.Handshake = &SandboxHandshakeResponse{
		Protocol:          sandboxProcessProtocol,
		ABIVersion:        SandboxProcessABIVersionV1,
		RuntimeInstanceID: resp.RuntimeInstanceID,
		ControlChannel:    sandboxControlChannelUnix,
		Commands:          sandboxControlCommands(),
		Capabilities:      sandboxControlCapabilities(),
	}
	return resp
}

func (p *SandboxProcess) handleSandboxInit(req SandboxControlRequest, resp SandboxControlResponse) SandboxControlResponse {
	if err := p.requireSandboxStartupSequence(sandboxControlCommandInit); err != nil {
		return setSandboxControlError(resp, err.code, err.message)
	}
	var payload SandboxInitRequest
	if err := decodeSandboxControlPayload(req.Payload, &payload); err != nil {
		return setSandboxControlError(resp, sandboxControlErrorSchemaInvalid, err.Error())
	}
	if p != nil {
		p.startupMu.Lock()
		p.initOK = true
		p.startupMu.Unlock()
	}
	resp.OK = true
	resp.Init = &SandboxInitResponse{
		PluginID:          resp.PluginID,
		ArtifactID:        resp.ArtifactID,
		RuntimeInstanceID: resp.RuntimeInstanceID,
		Generation:        resp.Generation,
		State:             "initialized",
	}
	if p != nil {
		resp.Init.ConfigJSON = p.ConfigJSON
	}
	return resp
}

func (p *SandboxProcess) handleSandboxRegister(req SandboxControlRequest, resp SandboxControlResponse) SandboxControlResponse {
	if err := p.requireSandboxStartupSequence(sandboxControlCommandRegister); err != nil {
		return setSandboxControlError(resp, err.code, err.message)
	}
	var payload SandboxRegisterRequest
	if err := decodeSandboxControlPayload(req.Payload, &payload); err != nil {
		return setSandboxControlError(resp, sandboxControlErrorRegisterFailed, err.Error())
	}
	registrations, capabilities, err := normalizeSandboxRegistrations(payload)
	if err != nil {
		return setSandboxControlError(resp, sandboxControlErrorRegisterFailed, err.Error())
	}
	if p != nil {
		p.startupMu.Lock()
		p.registerOK = true
		p.Registrations = append([]SandboxHandlerRegistration(nil), registrations...)
		p.completeSandboxStartupLocked(nil)
		p.startupMu.Unlock()
	}
	resp.OK = true
	resp.Register = &SandboxRegisterResponse{
		Registrations:        registrations,
		DeclaredCapabilities: capabilities,
	}
	return resp
}

func (p *SandboxProcess) handleSandboxReloadConfig(req SandboxControlRequest, resp SandboxControlResponse) SandboxControlResponse {
	if err := p.requireSandboxRegistered(); err != nil {
		return setSandboxControlError(resp, err.code, err.message)
	}
	var payload SandboxReloadConfigRequest
	if len(req.Payload) > 0 {
		if err := decodeSandboxControlPayload(req.Payload, &payload); err != nil {
			return setSandboxControlError(resp, sandboxControlErrorSchemaInvalid, err.Error())
		}
	}
	resp.OK = true
	return resp
}

func (p *SandboxProcess) requireSandboxStartupSequence(command string) *sandboxControlError {
	if p == nil {
		return nil
	}
	p.startupMu.Lock()
	defer p.startupMu.Unlock()
	switch command {
	case sandboxControlCommandInit:
		if !p.handshakeOK {
			return &sandboxControlError{code: sandboxControlErrorSequenceInvalid, message: "sandbox init requires successful handshake"}
		}
	case sandboxControlCommandRegister:
		if !p.handshakeOK || !p.initOK {
			return &sandboxControlError{code: sandboxControlErrorSequenceInvalid, message: "sandbox register requires successful handshake and init"}
		}
	}
	return nil
}

func (p *SandboxProcess) requireSandboxRegistered() *sandboxControlError {
	if p == nil {
		return nil
	}
	p.startupMu.Lock()
	defer p.startupMu.Unlock()
	if !p.registerOK {
		return &sandboxControlError{code: sandboxControlErrorSequenceInvalid, message: "sandbox command requires completed register"}
	}
	return nil
}

func normalizeSandboxRegistrations(req SandboxRegisterRequest) ([]SandboxHandlerRegistration, []string, error) {
	registrations := append([]SandboxHandlerRegistration(nil), req.Handlers...)
	if len(registrations) == 0 && (strings.TrimSpace(req.ExtensionPoint) != "" || strings.TrimSpace(req.HandlerID) != "") {
		registrations = append(registrations, SandboxHandlerRegistration{
			ExtensionPoint:       req.ExtensionPoint,
			HandlerID:            req.HandlerID,
			FailPolicy:           req.FailPolicy,
			TimeoutMS:            req.TimeoutMS,
			SchemaVersion:        req.SchemaVersion,
			DeclaredCapabilities: append([]string(nil), req.DeclaredCapabilities...),
		})
	}
	if len(registrations) == 0 {
		return nil, nil, errors.New("at least one sandbox handler registration is required")
	}
	capabilities := normalizeSandboxControlStringList(req.DeclaredCapabilities)
	for i := range registrations {
		reg := &registrations[i]
		reg.ExtensionPoint = strings.TrimSpace(reg.ExtensionPoint)
		reg.HandlerID = strings.TrimSpace(reg.HandlerID)
		reg.FailPolicy = strings.TrimSpace(reg.FailPolicy)
		if reg.ExtensionPoint == "" {
			return nil, nil, errors.New("extension_point is required")
		}
		if !supportedExtensionPoint(reg.ExtensionPoint) {
			return nil, nil, fmt.Errorf("unsupported extension_point %q", reg.ExtensionPoint)
		}
		if reg.HandlerID == "" {
			return nil, nil, errors.New("handler_id is required")
		}
		if len(reg.HandlerID) > 128 {
			return nil, nil, fmt.Errorf("handler_id %q exceeds maximum length", reg.HandlerID)
		}
		if reg.FailPolicy == "" {
			reg.FailPolicy = api.FailPolicyClose
		}
		if !validSandboxControlFailPolicy(reg.FailPolicy) {
			return nil, nil, fmt.Errorf("fail_policy %q is invalid", reg.FailPolicy)
		}
		if reg.TimeoutMS == 0 {
			reg.TimeoutMS = DefaultHandlerTimeout.Milliseconds()
		}
		if reg.TimeoutMS < 0 {
			return nil, nil, errors.New("timeout_ms must be positive")
		}
		if reg.TimeoutMS > int64(time.Minute/time.Millisecond) {
			return nil, nil, errors.New("timeout_ms exceeds maximum sandbox handler timeout")
		}
		if reg.SchemaVersion <= 0 {
			return nil, nil, errors.New("schema_version must be positive")
		}
		reg.DeclaredCapabilities = normalizeSandboxControlStringList(reg.DeclaredCapabilities)
		capabilities = append(capabilities, reg.DeclaredCapabilities...)
	}
	capabilities = normalizeSandboxControlStringList(capabilities)
	return registrations, capabilities, nil
}

func validSandboxControlFailPolicy(policy string) bool {
	switch policy {
	case api.FailPolicyOpen, api.FailPolicyClose, ExternalFailPolicyDegraded, ExternalFailPolicyFallback:
		return true
	default:
		return false
	}
}

func normalizeSandboxControlStringList(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func decodeSandboxControlPayload(payload json.RawMessage, dst any) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > sandboxControlMaxPayloadBytes {
		return sandboxControlError{code: sandboxControlErrorOversizedPayload, message: "sandbox control payload exceeds maximum size"}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("payload must contain a single JSON object")
		}
		return err
	}
	return nil
}

func redactSandboxControlMessage(message string) string {
	lower := strings.ToLower(message)
	for _, marker := range []string{"secret", "token", "password", "authorization"} {
		if strings.Contains(lower, marker) {
			return "[redacted]"
		}
	}
	return message
}

func (p *SandboxProcess) failSandboxStartupCommand(command, code, message string) {
	if p == nil {
		return
	}
	switch command {
	case sandboxControlCommandHandshake, sandboxControlCommandInit, sandboxControlCommandRegister:
	default:
		return
	}
	p.startupMu.Lock()
	defer p.startupMu.Unlock()
	if p.registerOK {
		return
	}
	p.completeSandboxStartupLocked(sandboxControlError{code: code, message: message})
}

func (p *SandboxProcess) completeSandboxStartupLocked(err error) {
	if p.startupDone == nil || p.startupClosed {
		return
	}
	if err != nil {
		p.startupErr = err
		if controlErr, ok := err.(sandboxControlError); ok {
			p.startupErrorCode = controlErr.code
			p.lastError = controlErr.Error()
		} else {
			p.lastError = err.Error()
		}
	}
	p.startupClosed = true
	close(p.startupDone)
}

func (p *SandboxProcess) waitForStartup(ctx context.Context) error {
	if p == nil {
		return errors.New("sandbox process is nil")
	}
	p.startupMu.Lock()
	if p.startupDone == nil {
		p.startupDone = make(chan struct{})
	}
	done := p.startupDone
	p.startupMu.Unlock()
	select {
	case <-done:
		p.startupMu.Lock()
		defer p.startupMu.Unlock()
		if p.startupErr != nil {
			return p.startupErr
		}
		if !p.registerOK {
			return sandboxControlError{code: sandboxControlErrorRegisterFailed, message: "sandbox register did not complete"}
		}
		return nil
	case <-p.done:
		if p.lastError != "" {
			return sandboxControlError{code: sandboxControlErrorInitFailed, message: p.lastError}
		}
		return sandboxControlError{code: sandboxControlErrorInitFailed, message: "sandbox process exited before register completed"}
	case <-ctx.Done():
		err := sandboxControlError{code: sandboxControlErrorTimeout, message: "sandbox control startup timed out before handshake/init/register completed"}
		p.startupMu.Lock()
		p.completeSandboxStartupLocked(err)
		p.startupMu.Unlock()
		return err
	}
}

func SendSandboxControlRequest(ctx context.Context, socketPath string, req SandboxControlRequest) (SandboxControlResponse, error) {
	if strings.TrimSpace(req.Protocol) == "" {
		req.Protocol = sandboxProcessProtocol
	}
	if req.DeadlineUnixMS == 0 {
		if deadline, ok := ctx.Deadline(); ok {
			req.DeadlineUnixMS = deadline.UnixMilli()
		}
	}
	if len(req.Payload) > sandboxControlMaxPayloadBytes {
		return SandboxControlResponse{}, sandboxControlError{code: sandboxControlErrorOversizedPayload, message: "sandbox control payload exceeds maximum size"}
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return SandboxControlResponse{}, err
	}
	if len(frame) > sandboxControlMaxFrameBytes {
		return SandboxControlResponse{}, sandboxControlError{code: sandboxControlErrorOversizedPayload, message: "sandbox control request exceeds maximum frame size"}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return SandboxControlResponse{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return SandboxControlResponse{}, err
	}
	var resp SandboxControlResponse
	limited := &io.LimitedReader{R: conn, N: sandboxControlMaxFrameBytes + 1}
	if err := json.NewDecoder(limited).Decode(&resp); err != nil {
		if limited.N <= 0 {
			return SandboxControlResponse{}, sandboxControlError{code: sandboxControlErrorOversizedPayload, message: "sandbox control response exceeds maximum frame size"}
		}
		return SandboxControlResponse{}, err
	}
	return resp, nil
}

func (p *SandboxProcess) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
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
	p.startupMu.Lock()
	registerOK := p.registerOK
	startupErrorCode := p.startupErrorCode
	p.startupMu.Unlock()
	state := RuntimeEnabled
	if !registerOK {
		state = RuntimeNotLoaded
	}
	if p.exitedAt != 0 {
		state = RuntimeDisabled
	}
	if p.crashLoop {
		state = RuntimeFailed
	}
	policy := normalizeSandboxPolicy(p.Policy)
	envKeys := make([]string, 0, len(policy.Env))
	for key := range policy.Env {
		envKeys = append(envKeys, redactSandboxControlEnvKey(key))
	}
	sort.Strings(envKeys)
	protocol := p.Protocol
	if protocol == "" {
		protocol = sandboxProcessProtocol
	}
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
		UnsupportedReason:  unsupportedSandboxRuntimeReason(startupErrorCode),
		EnforcementAttributes: map[string]string{
			"control_channel": sandboxControlChannelUnix,
			"protocol":        protocol,
			"filesystem":      "chroot-staged-root-with-explicit-roots",
			"network":         "newnet-without-host-network-by-default",
			"environment":     "explicit-allowlist-no-secret-env",
			"cpu_memory":      "process-rlimit-policy",
			"secret_delivery": "handle-rpc-version-only",
		},
	}
}

func unsupportedSandboxRuntimeReason(code string) string {
	if code == "" {
		return ""
	}
	return "sandbox control startup failed: " + code
}

func redactSandboxControlEnvKey(key string) string {
	lower := strings.ToLower(key)
	if strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
		return "[redacted]"
	}
	return key
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
		"unsupported_reason":     summary.UnsupportedReason,
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
