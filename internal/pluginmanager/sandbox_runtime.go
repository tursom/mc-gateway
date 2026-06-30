package pluginmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	sandboxControlCommandExternal        = "external.request"
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
	sandboxControlErrorExternalDenied    = "external_dependency_denied"
	sandboxControlErrorBadResponse       = "bad_response"
	sandboxControlErrorProcessExited     = "process_exited"
	sandboxDefaultWritableRuntimeDir     = "/tmp"
	sandboxControlRuntimeDir             = "/run"
	sandboxSecretDefaultTTL              = 60 * time.Second
	sandboxSecretMaxTTL                  = 5 * time.Minute
)

type SandboxProcessAdapter struct {
	Supervisor SandboxSupervisor
	Policy     SandboxPolicy
	Secrets    SandboxSecretResolver
	SelfCheck  SandboxEnvironmentSelfCheck

	startProcess sandboxProcessStarter
}

type SandboxSupervisor struct {
	Executable     string
	ArgsPrefix     []string
	Policy         SandboxPolicy
	StartupTimeout time.Duration
	SelfCheck      SandboxEnvironmentSelfCheck
}

type SandboxProcess struct {
	PluginID          string
	ArtifactID        string
	RuntimeInstanceID string
	Generation        int64
	Protocol          string
	UpstreamMode      string
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
	release  func()

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

	controlInvoker sandboxControlInvoker
	streamDialer   sandboxStreamDialer
	operations     *PluginOperations
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
	RequestID         string                     `json:"request_id,omitempty"`
	Command           string                     `json:"command,omitempty"`
	Protocol          string                     `json:"protocol"`
	PluginID          string                     `json:"plugin_id,omitempty"`
	ArtifactID        string                     `json:"artifact_id,omitempty"`
	RuntimeInstanceID string                     `json:"runtime_instance_id,omitempty"`
	Generation        int64                      `json:"generation"`
	TraceID           string                     `json:"trace_id,omitempty"`
	DeadlineUnixMS    int64                      `json:"deadline,omitempty"`
	OK                bool                       `json:"ok"`
	Code              string                     `json:"code,omitempty"`
	ErrorCode         string                     `json:"error_code,omitempty"`
	Error             string                     `json:"error,omitempty"`
	Handshake         *SandboxHandshakeResponse  `json:"handshake,omitempty"`
	Init              *SandboxInitResponse       `json:"init,omitempty"`
	Register          *SandboxRegisterResponse   `json:"register,omitempty"`
	Metrics           *SandboxMetricsResponse    `json:"metrics,omitempty"`
	Health            *RuntimeHealth             `json:"health,omitempty"`
	Diagnostics       *SandboxDiagnosticSummary  `json:"diagnostics,omitempty"`
	Secret            *SandboxSecretResponse     `json:"secret,omitempty"`
	External          *SandboxExternalResponse   `json:"external,omitempty"`
	Invoke            *SandboxInvokeResponse     `json:"invoke,omitempty"`
	Stream            *SandboxStreamOpenResponse `json:"stream,omitempty"`
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

type SandboxInvokeRequest struct {
	ExtensionPoint string `json:"extension_point"`
	HandlerID      string `json:"handler_id"`
	FailPolicy     string `json:"fail_policy,omitempty"`

	ConfigJSON    json.RawMessage              `json:"config_json,omitempty"`
	RouteResolve  *api.RouteResolveRequest     `json:"route_resolve,omitempty"`
	RuleEvaluate  *api.RuleEvaluateRequest     `json:"rule_evaluate,omitempty"`
	StatusPing    *api.StatusPingRequest       `json:"status_ping,omitempty"`
	Provider      *SandboxProviderQueryRequest `json:"provider,omitempty"`
	EventDelivery *api.EventDeliveryRequest    `json:"event_delivery,omitempty"`
}

type SandboxInvokeResponse struct {
	ExtensionPoint string `json:"extension_point"`
	HandlerID      string `json:"handler_id"`

	OK             bool                      `json:"ok"`
	Valid          *bool                     `json:"valid,omitempty"`
	RouteDecision  *api.RouteDecision        `json:"route_decision,omitempty"`
	RuleDecision   *api.RuleEvaluateDecision `json:"rule_decision,omitempty"`
	StatusResponse *api.StatusPingResponse   `json:"status_response,omitempty"`
	Provider       *api.ProviderRegistration `json:"provider,omitempty"`
	EventResult    *api.EventDeliveryResult  `json:"event_result,omitempty"`
	Reason         string                    `json:"reason,omitempty"`
	ErrorCode      string                    `json:"error_code,omitempty"`
	Error          string                    `json:"error,omitempty"`
}

type SandboxProviderQueryRequest struct {
	ExtensionPoint string `json:"extension_point"`
}

type sandboxControlInvoker func(context.Context, string, SandboxControlRequest) (SandboxControlResponse, error)

type sandboxStreamDialer func(context.Context, *SandboxProcess, SandboxStreamOpenResponse) (net.Conn, error)

type SandboxStreamOpenRequest struct {
	ExtensionPoint   string            `json:"extension_point"`
	HandlerID        string            `json:"handler_id"`
	FailPolicy       string            `json:"fail_policy,omitempty"`
	Protocol         string            `json:"protocol"`
	StreamID         string            `json:"stream_id"`
	ConnectionID     string            `json:"connection_id,omitempty"`
	TraceID          string            `json:"trace_id,omitempty"`
	Host             string            `json:"host,omitempty"`
	Upstream         string            `json:"upstream,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	SourceAddr       string            `json:"source_addr,omitempty"`
	ServerHost       string            `json:"server_host,omitempty"`
	RawServerHost    string            `json:"raw_server_host,omitempty"`
	ProtocolVersion  int               `json:"protocol_version,omitempty"`
	NextState        int               `json:"next_state,omitempty"`
	RouteID          string            `json:"route_id,omitempty"`
	RouteTags        []string          `json:"route_tags,omitempty"`
	UpstreamRaw      string            `json:"upstream_raw,omitempty"`
	UpstreamProtocol string            `json:"upstream_protocol,omitempty"`
	UpstreamAddress  string            `json:"upstream_address,omitempty"`
	Transport        string            `json:"transport,omitempty"`
	ServiceName      string            `json:"service_name,omitempty"`
	ListenerPort     int               `json:"listener_port,omitempty"`
	DeadlineUnixMS   int64             `json:"deadline_unix_ms,omitempty"`
}

type SandboxStreamOpenResponse struct {
	Connected    bool   `json:"connected"`
	Protocol     string `json:"protocol"`
	StreamID     string `json:"stream_id"`
	Endpoint     string `json:"endpoint"`
	EndpointType string `json:"endpoint_type,omitempty"`
}

type SandboxStreamCloseRequest struct {
	Protocol string `json:"protocol"`
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

func normalizeSandboxPolicy(policy SandboxPolicy) SandboxPolicy {
	if policy.CPUSeconds <= 0 {
		policy.CPUSeconds = 2
	}
	if policy.MemoryBytes <= 0 {
		policy.MemoryBytes = 64 * 1024 * 1024
	}
	if policy.FileQuotaBytes <= 0 {
		policy.FileQuotaBytes = DefaultPluginFileQuota
	}
	if policy.Env == nil {
		policy.Env = map[string]string{}
	}
	policy.FilesystemRoots = normalizeSandboxControlStringList(policy.FilesystemRoots)
	policy.SecretHandles = normalizeSandboxControlStringList(policy.SecretHandles)
	return policy
}

func sandboxPolicyEmpty(policy SandboxPolicy) bool {
	return len(policy.FilesystemRoots) == 0 &&
		!policy.NetworkEnabled &&
		len(policy.Env) == 0 &&
		policy.CPUSeconds == 0 &&
		policy.MemoryBytes == 0 &&
		policy.FileQuotaBytes == 0 &&
		len(policy.SecretHandles) == 0 &&
		!policy.ExternalIsolation
}

func validateSandboxPolicyEnforceable(policy SandboxPolicy) error {
	if policy.NetworkEnabled {
		return errors.New("sandbox-process network access cannot be enabled until network policy enforcement is available")
	}
	for _, root := range policy.FilesystemRoots {
		if _, err := sandboxFilesystemRootRel(root); err != nil {
			return err
		}
	}
	for key := range policy.Env {
		lower := strings.ToLower(strings.TrimSpace(key))
		if sandboxSensitiveName(lower) {
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
	return validateSandboxEnforcementSupported(policy)
}

func sandboxEnvironmentSelfCheck(selfCheck SandboxEnvironmentSelfCheck, policy SandboxPolicy) error {
	if selfCheck == nil {
		selfCheck = defaultSandboxEnvironmentSelfCheck
	}
	return selfCheck(normalizeSandboxPolicy(policy))
}

type sandboxEnforcementReport struct {
	Facts []SandboxEnforcementFact `json:"facts"`
}

func (r sandboxEnforcementReport) requiredFailures() []SandboxEnforcementFact {
	failures := make([]SandboxEnforcementFact, 0)
	for _, fact := range r.Facts {
		if fact.Required && !fact.Enforced {
			failures = append(failures, fact)
		}
	}
	return failures
}

func (r sandboxEnforcementReport) err() error {
	failures := r.requiredFailures()
	if len(failures) == 0 {
		return nil
	}
	parts := make([]string, 0, len(failures))
	for _, fact := range failures {
		reason := fact.UnsupportedReason
		if reason == "" {
			reason = "not enforced"
		}
		parts = append(parts, fact.Category+"."+fact.Key+"="+reason)
	}
	sort.Strings(parts)
	return fmt.Errorf("sandbox-process required enforcement unavailable: %s", strings.Join(parts, "; "))
}

func sandboxEnforcementFacts(policy SandboxPolicy) []SandboxEnforcementFact {
	policy = normalizeSandboxPolicy(policy)
	filesystemMethod, filesystemEnforced, filesystemReason := sandboxFilesystemEnforcementFact(policy)
	facts := []SandboxEnforcementFact{
		{
			Category:          "filesystem",
			Key:               "readonly_root",
			Required:          true,
			Enforced:          filesystemEnforced,
			Method:            filesystemMethod,
			UnsupportedReason: filesystemReason,
		},
		{
			Category:          "filesystem",
			Key:               "artifact_readonly_staging",
			Required:          true,
			Enforced:          filesystemEnforced,
			Method:            sandboxEnforcementMethod(filesystemMethod, "copied-runtime-entry-mode-0555"),
			UnsupportedReason: filesystemReason,
		},
		{
			Category:          "filesystem",
			Key:               "runtime_writable_volume",
			Required:          true,
			Enforced:          filesystemEnforced,
			Method:            sandboxEnforcementMethod(filesystemMethod, "writable-runtime-dir"),
			UnsupportedReason: filesystemReason,
			Details:           map[string]string{"path": sandboxDefaultWritableRuntimeDir},
		},
		{
			Category:          "filesystem",
			Key:               "path_allowlist",
			Required:          true,
			Enforced:          filesystemEnforced,
			Method:            sandboxEnforcementMethod(filesystemMethod, "explicit-writable-roots-only"),
			UnsupportedReason: filesystemReason,
			Details:           map[string]string{"roots": strings.Join(policy.FilesystemRoots, ",")},
		},
		{
			Category: "filesystem",
			Key:      "quota",
			Required: false,
			Enforced: policy.FileQuotaBytes > 0,
			Method:   "rlimit_fsize_per_file",
			Details:  map[string]string{"bytes": fmt.Sprintf("%d", policy.FileQuotaBytes)},
		},
		{
			Category: "network",
			Key:      "no_host_network",
			Required: true,
			Enforced: !policy.NetworkEnabled,
			Method:   "linux-network-namespace-without-host-network",
		},
		{
			Category: "network",
			Key:      "egress_policy",
			Required: true,
			Enforced: !policy.NetworkEnabled && policy.ExternalIsolation,
			Method:   sandboxEgressPolicyMethod(policy),
			UnsupportedReason: func() string {
				if policy.NetworkEnabled {
					return "egress proxy/firewall/sidecar enforcement is not configured"
				}
				if !policy.ExternalIsolation {
					return "host-mediated external dependency policy requires sandbox policy external_isolation=true"
				}
				return ""
			}(),
		},
		{
			Category: "environment",
			Key:      "allowlist_no_secret_env",
			Required: true,
			Enforced: true,
			Method:   "explicit-env-map-secret-token-filter",
		},
		{
			Category: "resource",
			Key:      "cpu_memory",
			Required: true,
			Enforced: policy.CPUSeconds > 0 && policy.MemoryBytes > 0,
			Method:   "rlimit_cpu_rlimit_as",
			Details: map[string]string{
				"cpu_seconds":  fmt.Sprintf("%d", policy.CPUSeconds),
				"memory_bytes": fmt.Sprintf("%d", policy.MemoryBytes),
			},
		},
		{
			Category: "secret",
			Key:      "handle_rpc",
			Required: true,
			Enforced: true,
			Method:   "version-only-control-rpc",
		},
	}
	facts = append(facts, sandboxPlatformEnforcementFacts(policy)...)
	return facts
}

func sandboxEnforcementReportForPolicy(policy SandboxPolicy) sandboxEnforcementReport {
	return sandboxEnforcementReport{Facts: sandboxEnforcementFacts(policy)}
}

func sandboxEgressPolicyMethod(policy SandboxPolicy) string {
	if policy.ExternalIsolation && !policy.NetworkEnabled {
		return "default-deny-raw-network+host-mediated-external-client"
	}
	return "default-deny-no-egress-proxy"
}

func sandboxEnforcementMethod(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "+")
}

func sandboxFactsAllRequiredEnforced(facts []SandboxEnforcementFact, category string, keys ...string) bool {
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = false
	}
	for _, fact := range facts {
		if fact.Category != category {
			continue
		}
		if _, ok := wanted[fact.Key]; !ok {
			continue
		}
		wanted[fact.Key] = fact.Enforced
	}
	for _, enforced := range wanted {
		if !enforced {
			return false
		}
	}
	return true
}

func sandboxSensitiveName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"secret", "token", "password", "credential", "authorization"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
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
		sandboxControlCommandHealth,
		sandboxControlCommandDiagnostics,
		sandboxControlCommandSecret,
		sandboxControlCommandExternal,
	}
}

func sandboxControlCapabilities() []string {
	capabilities := []string{
		"control.handshake",
		"control.init",
		"control.register",
		"control.reload_config",
		"control.metrics",
		"control.drain",
		"control.stop",
		"secret.handle",
		"external.dependency",
		"network.egress",
		StreamProxyProtocolV1,
		"stream.open",
		"stream.close",
		"stream.half_close",
		"stream.deadline",
		"stream.backpressure",
		"stream.cancel",
		"stream.byte_accounting",
	}
	capabilities = append(capabilities, StreamProxyCapabilities()...)
	return capabilities
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
	return sandboxEnvironmentSelfCheck(a.SelfCheck, policy)
}

func (a SandboxProcessAdapter) Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error) {
	if err := a.ValidateArtifact(ctx, artifact); err != nil {
		return RuntimePrepared{}, err
	}
	if pluginRecord.ID != "" && artifact.PluginID != "" && pluginRecord.ID != artifact.PluginID {
		return RuntimePrepared{}, errors.New("artifact plugin_id does not match")
	}
	if pluginRecord.DesiredArtifactID != "" && pluginRecord.DesiredArtifactID != artifact.ID {
		return RuntimePrepared{}, errors.New("desired artifact_id does not match")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return RuntimePrepared{}, fmt.Errorf("decode sandbox manifest: %w", err)
	}
	if err := validateSandboxArtifactMetadata(artifact, manifest); err != nil {
		return RuntimePrepared{}, err
	}
	caps := requiredRuntimeCapabilities(artifact)
	if missing := sandboxExternalDependencyCapabilityMissing(manifest, caps); len(missing) > 0 {
		return RuntimePrepared{}, fmt.Errorf("sandbox-process external dependencies require runtime capability network.egress: %s", strings.Join(missing, ","))
	}
	policy := a.Supervisor.Policy
	if sandboxPolicyEmpty(policy) {
		policy = a.Policy
	}
	if unsupported := unsupportedSandboxRequiredCapabilities(policy, caps); len(unsupported) > 0 {
		return RuntimePrepared{}, fmt.Errorf("sandbox-process cannot enforce required capabilities: %s", strings.Join(unsupported, ","))
	}
	return runtimePreparedFor(artifact, pluginRecord, PluginServiceModeSandboxProcess), nil
}

func (a SandboxProcessAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error) {
	supervisor := a.Supervisor
	if sandboxPolicyEmpty(supervisor.Policy) {
		supervisor.Policy = a.Policy
	}
	if supervisor.SelfCheck == nil {
		supervisor.SelfCheck = a.SelfCheck
	}
	supervisor.Policy = normalizeSandboxPolicy(supervisor.Policy)
	if err := validateSandboxPolicyEnforceable(supervisor.Policy); err != nil {
		return RuntimeInstance{}, err
	}
	if err := sandboxEnvironmentSelfCheck(supervisor.SelfCheck, supervisor.Policy); err != nil {
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
	process.UpstreamMode = upstreamModeFromArtifact(artifact)
	plugin := sandboxHostedPlugin{process: process}
	if err := plugin.Init(gateway); err != nil {
		_ = process.Stop(ctx)
		return RuntimeInstance{}, err
	}
	prepared.RuntimeInstanceID = process.RuntimeInstanceID
	return RuntimeInstance{
		RuntimePrepared: prepared,
		Plugin:          plugin,
		StartedAt:       process.StartedAt,
	}, nil
}

func (a SandboxProcessAdapter) HealthCheck(_ context.Context, instance RuntimeInstance) RuntimeHealth {
	now := time.Now().Unix()
	process := sandboxHostedPluginFromInstance(instance.Plugin)
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

func (a SandboxProcessAdapter) DryRunConfig(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) error {
	prepared, err := a.Prepare(ctx, artifact, pluginRecord)
	if err != nil {
		return err
	}
	instance, err := a.Start(ctx, prepared, artifact, pluginRecord, NewGateway(pluginRecord.ID, nil, nil, nil))
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), sandboxControlStartupTimeout(a.Supervisor.StartupTimeout))
	defer cancel()
	defer func() { _ = a.Stop(stopCtx, instance) }()
	return instance.Plugin.ReloadConfig(json.RawMessage(defaultJSONObject(pluginRecord.ConfigJSON)))
}

func (a SandboxProcessAdapter) ReloadConfig(ctx context.Context, instance RuntimeInstance, configJSON string) error {
	hosted := sandboxHostedPluginFromInstance(instance.Plugin)
	if hosted.process == nil {
		return errors.New("sandbox process is nil")
	}
	if err := hosted.validateConfig(ctx, json.RawMessage(defaultJSONObject(configJSON))); err != nil {
		return err
	}
	payload, err := json.Marshal(SandboxReloadConfigRequest{ConfigJSON: defaultJSONObject(configJSON)})
	if err != nil {
		return err
	}
	resp, err := hosted.process.sendControlRequest(ctx, sandboxControlCommandReloadConfig, "", payload)
	if err != nil {
		return err
	}
	if !resp.OK {
		return sandboxResponseError(resp, sandboxFailPolicyForExtension(ExtensionConfigValidate))
	}
	hosted.process.ConfigJSON = defaultJSONObject(configJSON)
	return nil
}

func (a SandboxProcessAdapter) Drain(ctx context.Context, instance RuntimeInstance) error {
	hosted := sandboxHostedPluginFromInstance(instance.Plugin)
	if hosted.process == nil {
		return nil
	}
	resp, err := hosted.process.sendControlRequest(ctx, sandboxControlCommandDrain, "", nil)
	if err != nil {
		return err
	}
	if !resp.OK {
		return sandboxResponseError(resp, api.FailPolicyClose)
	}
	return nil
}

func (a SandboxProcessAdapter) Stop(ctx context.Context, instance RuntimeInstance) error {
	process := sandboxHostedPluginFromInstance(instance.Plugin)
	if process.process == nil {
		return nil
	}
	return process.process.Stop(ctx)
}

func (a SandboxProcessAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	process := sandboxHostedPluginFromInstance(instance.Plugin)
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
	if err := sandboxEnvironmentSelfCheck(s.SelfCheck, policy); err != nil {
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
	if err := finalizeSandboxRoot(rootDir, policy, socketPath); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	runtimeInstanceID := newSandboxRuntimeInstanceID()
	args := append([]string{}, s.ArgsPrefix...)
	cmd := exec.CommandContext(ctx, "/plugin", args...)
	cmd.Env = sandboxProcessEnv(policy, pluginID, artifactID, runtimeInstanceID, generation)
	configureSandboxCommand(cmd, rootDir, policy)
	cmd.Dir = "/"
	if policy.CPUSeconds > 0 {
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				return killSandboxProcessTree(cmd.Process.Pid)
			}
			return nil
		}
	}
	release, err := startSandboxCommand(cmd)
	if err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(rootDir)
		return nil, err
	}
	if err := applySandboxRLimits(cmd.Process.Pid, policy); err != nil {
		_ = listener.Close()
		_ = killSandboxProcessTree(cmd.Process.Pid)
		_ = cmd.Wait()
		release()
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
		release:           release,
		startupDone:       make(chan struct{}),
	}
	if err := process.startControlRPC(ctx, listener, resolver); err != nil {
		_ = process.Kill()
		_ = cmd.Wait()
		release()
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
	if err := os.Chmod(filepath.Join(rootDir, "plugin"), 0555); err != nil {
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
		clean, err := sandboxFilesystemRootRel(root)
		if err != nil {
			_ = os.RemoveAll(rootDir)
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(rootDir, clean), 0700); err != nil {
			_ = os.RemoveAll(rootDir)
			return "", err
		}
	}
	return rootDir, nil
}

func finalizeSandboxRoot(rootDir string, policy SandboxPolicy, socketPath string) error {
	if strings.TrimSpace(rootDir) == "" {
		return errors.New("sandbox root is required")
	}
	writable := map[string]bool{
		filepath.Clean(filepath.Join(rootDir, strings.TrimPrefix(sandboxDefaultWritableRuntimeDir, "/"))): true,
	}
	for _, root := range policy.FilesystemRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		clean, err := sandboxFilesystemRootRel(root)
		if err != nil {
			return err
		}
		writable[filepath.Clean(filepath.Join(rootDir, clean))] = true
	}
	if err := filepath.WalkDir(rootDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			if filepath.Clean(path) == filepath.Join(rootDir, "plugin") {
				return os.Chmod(path, 0555)
			}
		case info.IsDir():
			if writable[filepath.Clean(path)] {
				return chmodSandboxWritable(path)
			}
			return os.Chmod(path, 0555)
		}
		return nil
	}); err != nil {
		return err
	}
	if strings.TrimSpace(socketPath) != "" {
		if err := os.Chmod(socketPath, 0666); err != nil {
			return err
		}
		if err := chownSandboxWritable(socketPath); err != nil {
			return err
		}
	}
	return nil
}

func chmodSandboxWritable(path string) error {
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return chownSandboxWritable(path)
}

func sandboxFilesystemRootRel(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", errors.New("sandbox filesystem root cannot be empty")
	}
	clean := filepath.Clean(trimmed)
	clean = strings.TrimPrefix(clean, string(filepath.Separator))
	switch {
	case clean == "", clean == ".":
		return "", fmt.Errorf("sandbox filesystem root %q must not be the sandbox root", root)
	case strings.HasPrefix(clean, ".."):
		return "", fmt.Errorf("sandbox filesystem root %q escapes sandbox root", root)
	}
	first := clean
	if idx := strings.IndexRune(clean, filepath.Separator); idx >= 0 {
		first = clean[:idx]
	}
	switch first {
	case "plugin", strings.TrimPrefix(sandboxControlRuntimeDir, "/"):
		return "", fmt.Errorf("sandbox filesystem root %q targets reserved sandbox path %q", root, first)
	}
	return clean, nil
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
		if sandboxSensitiveName(key) {
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
		if strings.TrimSpace(payload.ArtifactID) == "" {
			payload.ArtifactID = req.ArtifactID
		}
		if strings.TrimSpace(payload.RuntimeInstanceID) == "" {
			payload.RuntimeInstanceID = req.RuntimeInstanceID
		}
		if payload.Generation == 0 {
			payload.Generation = req.Generation
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
			if secret.ErrorCode != "" {
				resp.Code = secret.ErrorCode
				resp.ErrorCode = secret.ErrorCode
			}
		}
	case sandboxControlCommandExternal:
		external, err := p.handleSandboxExternalRequest(ctx, req)
		if err != nil {
			resp = setSandboxControlError(resp, sandboxControlErrorExternalDenied, err.Error())
			break
		}
		resp.OK = external.OK
		resp.External = &external
		resp.Error = redactSandboxControlMessage(external.Error)
		if !external.OK {
			resp.Code = sandboxControlErrorExternalDenied
			resp.ErrorCode = sandboxControlErrorExternalDenied
			if external.ErrorCode != "" {
				resp.Code = external.ErrorCode
				resp.ErrorCode = external.ErrorCode
			}
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

func (p *SandboxProcess) handleSandboxExternalRequest(ctx context.Context, envelope SandboxControlRequest) (SandboxExternalResponse, error) {
	var payload SandboxExternalRequest
	if err := decodeSandboxControlPayload(envelope.Payload, &payload); err != nil {
		return SandboxExternalResponse{
			OK:        false,
			ErrorCode: sandboxControlErrorSchemaInvalid,
			Error:     redactSensitive(err.Error()),
		}, nil
	}
	payload.Name = strings.TrimSpace(payload.Name)
	if payload.Name == "" {
		return SandboxExternalResponse{OK: false, ErrorCode: "external_dependency_name_required", Error: "external dependency name is required"}, nil
	}
	if p == nil || p.operations == nil {
		return SandboxExternalResponse{OK: false, Name: payload.Name, ErrorCode: "external_dependency_unavailable", Error: "sandbox external dependency client is unavailable"}, nil
	}
	if envelope.TraceID != "" {
		ctx = context.WithValue(ctx, traceContextKey{}, traceContext{PluginID: p.PluginID, TraceID: envelope.TraceID})
	}
	if envelope.DeadlineUnixMS > 0 {
		deadline := time.UnixMilli(envelope.DeadlineUnixMS)
		if time.Until(deadline) <= 0 {
			return SandboxExternalResponse{OK: false, Name: payload.Name, ErrorCode: "external_dependency_deadline_exceeded", Error: "external dependency request deadline exceeded"}, nil
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	summary, err := p.operations.ExternalDependencySummary(payload.Name)
	if err != nil {
		return sandboxExternalDeniedResponse(payload.Name, "external_dependency_not_declared", err, nil), nil
	}
	client := p.operations.ExternalClient(payload.Name)
	timeout := time.Duration(payload.TimeoutMS) * time.Millisecond
	if payload.HealthCheck {
		err := client.HealthCheck(ctx)
		refreshed, summaryErr := p.operations.ExternalDependencySummary(payload.Name)
		if summaryErr == nil {
			summary = refreshed
		}
		if err != nil {
			return sandboxExternalDeniedResponse(payload.Name, "external_dependency_health_failed", err, &summary), nil
		}
		return SandboxExternalResponse{
			OK:         true,
			Name:       payload.Name,
			TimeoutMS:  payload.TimeoutMS,
			FailPolicy: summary.FailPolicy,
			Summary:    &summary,
		}, nil
	}
	switch strings.ToLower(strings.TrimSpace(payload.Protocol)) {
	case "", "http", "https":
		resp, err := client.DoHTTP(ctx, api.ExternalRequest{
			Method:  payload.Method,
			URL:     payload.URL,
			Header:  http.Header(payload.Headers),
			Body:    bytes.NewReader(payload.Body),
			Timeout: timeout,
		})
		refreshed, summaryErr := p.operations.ExternalDependencySummary(payload.Name)
		if summaryErr == nil {
			summary = refreshed
		}
		if err != nil {
			return sandboxExternalDeniedResponse(payload.Name, "external_dependency_request_failed", err, &summary), nil
		}
		return SandboxExternalResponse{
			OK:         true,
			Name:       payload.Name,
			StatusCode: resp.StatusCode,
			Headers:    map[string][]string(resp.Header),
			Body:       resp.Body,
			TimeoutMS:  payload.TimeoutMS,
			FailPolicy: summary.FailPolicy,
			Summary:    &summary,
		}, nil
	case "tcp":
		conn, err := client.DialTCP(ctx, payload.URL, timeout)
		if conn != nil {
			_ = conn.Close()
		}
		refreshed, summaryErr := p.operations.ExternalDependencySummary(payload.Name)
		if summaryErr == nil {
			summary = refreshed
		}
		if err != nil {
			return sandboxExternalDeniedResponse(payload.Name, "external_dependency_request_failed", err, &summary), nil
		}
		return SandboxExternalResponse{
			OK:         true,
			Name:       payload.Name,
			TimeoutMS:  payload.TimeoutMS,
			FailPolicy: summary.FailPolicy,
			Summary:    &summary,
		}, nil
	default:
		return SandboxExternalResponse{
			OK:        false,
			Name:      payload.Name,
			ErrorCode: "external_dependency_protocol_unsupported",
			Error:     "external dependency protocol is unsupported",
			Summary:   &summary,
		}, nil
	}
}

func sandboxExternalDeniedResponse(name, code string, err error, summary *ExternalDependencySummary) SandboxExternalResponse {
	message := "external dependency request denied"
	if err != nil {
		message = redactSensitive(err.Error())
	}
	resp := SandboxExternalResponse{
		OK:        false,
		Name:      name,
		ErrorCode: code,
		Error:     message,
		Summary:   summary,
	}
	if summary != nil {
		resp.FailPolicy = summary.FailPolicy
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
		sandboxControlCommandSecret,
		sandboxControlCommandExternal:
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
		sandboxControlCommandSecret,
		sandboxControlCommandExternal:
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
		if !supportedSandboxRegistrationExtensionPoint(reg.ExtensionPoint) {
			return nil, nil, fmt.Errorf("unsupported sandbox extension_point %q", reg.ExtensionPoint)
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

func supportedSandboxRequestResponseExtensionPoint(point string) bool {
	switch point {
	case ExtensionRouteResolve,
		ExtensionRouteResolver,
		ExtensionRuleEvaluate,
		ExtensionConfigValidate,
		ExtensionStatusPing,
		ExtensionProvider,
		ExtensionEventSubscriber:
		return true
	default:
		return false
	}
}

func supportedSandboxRegistrationExtensionPoint(point string) bool {
	return supportedSandboxRequestResponseExtensionPoint(point) || point == ExtensionUpstreamConnect
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
	_ = signalSandboxProcessTree(p.cmd.Process.Pid, os.Interrupt)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		_ = killSandboxProcessTree(p.cmd.Process.Pid)
		<-p.done
		return ctx.Err()
	case <-time.After(time.Second):
		_ = killSandboxProcessTree(p.cmd.Process.Pid)
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
	return killSandboxProcessTree(p.cmd.Process.Pid)
}

func (p *SandboxProcess) wait() {
	err := p.cmd.Wait()
	if p.release != nil {
		p.release()
	}
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
	facts := sandboxEnforcementFacts(policy)
	return SandboxDiagnosticSummary{
		PluginID:           p.PluginID,
		ArtifactID:         p.ArtifactID,
		PID:                p.PID,
		State:              state,
		NamespaceEnforced:  sandboxFactsAllRequiredEnforced(facts, "namespace", "mount", "network", "pid", "uts", "ipc", "user"),
		ControlRPC:         true,
		FilesystemEnforced: sandboxFactsAllRequiredEnforced(facts, "filesystem", "readonly_root", "artifact_readonly_staging", "runtime_writable_volume", "path_allowlist"),
		NetworkEnforced:    sandboxFactsAllRequiredEnforced(facts, "network", "no_host_network", "egress_policy"),
		EnvEnforced:        sandboxFactsAllRequiredEnforced(facts, "environment", "allowlist_no_secret_env"),
		CPUMemoryEnforced:  sandboxFactsAllRequiredEnforced(facts, "resource", "cpu_memory"),
		ProcessEnforced:    sandboxFactsAllRequiredEnforced(facts, "process", "no_new_privs", "capabilities_dropped", "seccomp", "fork_exec_policy"),
		CleanupEnforced:    sandboxFactsAllRequiredEnforced(facts, "cleanup", "process_tree", "orphan"),
		SecretRPC:          true,
		CrashLoop:          p.crashLoop,
		CrashCount:         p.crashCount,
		LastError:          p.lastError,
		SecretHandles:      append([]string(nil), policy.SecretHandles...),
		EnvKeys:            envKeys,
		ControlSocket:      "unix:///run/control.sock",
		UnsupportedReason:  unsupportedSandboxRuntimeReason(startupErrorCode),
		EnforcementFacts:   facts,
		EnforcementAttributes: map[string]string{
			"control_channel": sandboxControlChannelUnix,
			"protocol":        protocol,
			"stream_protocol": StreamProxyProtocolV1,
			"filesystem":      "readonly-chroot-staged-root-with-explicit-writable-roots",
			"network":         "newnet-default-deny-no-host-network",
			"environment":     "explicit-allowlist-no-secret-env",
			"cpu_memory":      "process-rlimit-policy",
			"process":         "pid-user-namespace-no-new-privs-capability-drop-external-seccomp-required",
			"cleanup":         "pid-namespace-process-group-kill-pdeathsig",
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
	if sandboxSensitiveName(key) {
		return "[redacted]"
	}
	return key
}

func sandboxDiagnosticsDetails(summary SandboxDiagnosticSummary) map[string]any {
	return map[string]any{
		"protocol":               sandboxProcessProtocol,
		"control_channel":        sandboxControlChannelUnix,
		"stream_protocol":        StreamProxyProtocolV1,
		"pid":                    summary.PID,
		"namespace_enforced":     summary.NamespaceEnforced,
		"control_rpc":            summary.ControlRPC,
		"filesystem_enforced":    summary.FilesystemEnforced,
		"network_enforced":       summary.NetworkEnforced,
		"env_enforced":           summary.EnvEnforced,
		"cpu_memory_enforced":    summary.CPUMemoryEnforced,
		"process_enforced":       summary.ProcessEnforced,
		"cleanup_enforced":       summary.CleanupEnforced,
		"secret_rpc":             summary.SecretRPC,
		"crash_loop":             summary.CrashLoop,
		"crash_count":            summary.CrashCount,
		"last_error":             summary.LastError,
		"unsupported_reason":     summary.UnsupportedReason,
		"secret_handles":         append([]string(nil), summary.SecretHandles...),
		"env_keys":               append([]string(nil), summary.EnvKeys...),
		"control_socket":         summary.ControlSocket,
		"enforcement_attributes": summary.EnforcementAttributes,
		"enforcement_facts":      append([]SandboxEnforcementFact(nil), summary.EnforcementFacts...),
	}
}

func (p sandboxHostedPlugin) Init(gateway api.Gateway) error {
	if p.process == nil {
		return errors.New("sandbox process is nil")
	}
	if concrete, ok := gateway.(*Gateway); ok {
		p.process.operations = concrete.ops
	}
	for _, registration := range p.process.Registrations {
		reg := registration
		switch reg.ExtensionPoint {
		case ExtensionUpstreamConnect:
			if p.process.UpstreamMode != UpstreamModeProtocolProxy {
				return errors.New("sandbox-process upstream.connect/v1 requires stream.proxy/v1 protocol-proxy mode; dialer semantics are unsupported")
			}
			if err := api.RegisterHookHandler(
				gateway,
				api.HookUpstreamConnect,
				func(api.UpstreamConnectRequest) bool { return true },
				func(req api.UpstreamConnectRequest) (net.Conn, error) {
					return p.openStream(req.Context, req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionRouteResolve:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookRouteResolve,
				func(api.RouteResolveRequest) bool { return true },
				func(req api.RouteResolveRequest) (api.RouteDecision, error) {
					return p.resolveRoute(req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionRouteResolver:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookRouteResolver,
				func(api.RouteResolveRequest) bool { return true },
				func(req api.RouteResolveRequest) (api.RouteDecision, error) {
					return p.resolveRoute(req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionRuleEvaluate:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookRuleEvaluate,
				func(api.RuleEvaluateRequest) bool { return true },
				func(req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
					return p.evaluateRule(req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionStatusPing:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookStatusPing,
				func(api.StatusPingRequest) bool { return true },
				func(req api.StatusPingRequest) (api.StatusPingResponse, error) {
					return p.statusPing(req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionProvider:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookProvider,
				func(api.ProviderRegistration) bool { return true },
				func() (api.ProviderRegistration, error) {
					return p.queryProvider(reg)
				},
			); err != nil {
				return err
			}
		case ExtensionEventSubscriber:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookEventSubscriber,
				func(api.EventDeliveryRequest) bool { return true },
				func(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
					return p.deliverEvent(req, reg)
				},
			); err != nil {
				return err
			}
		case ExtensionConfigValidate:
			// Config validation is called through ReloadConfig/DryRunConfig, not
			// through Gateway dispatch snapshots.
		default:
			return fmt.Errorf("sandbox extension point %q is not supported by the sandbox data-plane", reg.ExtensionPoint)
		}
	}
	return nil
}

func (p sandboxHostedPlugin) Destroy() error { return nil }

func (p sandboxHostedPlugin) NewConfigObj() any { return json.RawMessage(`{}`) }

func (p sandboxHostedPlugin) ReloadConfig(config any) error {
	configJSON, err := sandboxConfigJSON(config)
	if err != nil {
		return err
	}
	return p.validateConfig(context.Background(), configJSON)
}

func (p sandboxHostedPlugin) validateConfig(ctx context.Context, configJSON json.RawMessage) error {
	reg, ok := p.registrationFor(ExtensionConfigValidate)
	if !ok {
		return nil
	}
	resp, err := p.invoke(ctx, reg, SandboxInvokeRequest{ConfigJSON: append(json.RawMessage(nil), configJSON...)})
	if err != nil {
		if sandboxFailOpen(reg.FailPolicy) {
			return nil
		}
		return err
	}
	if resp.Valid == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox config.validate/v1 response missing valid")
		if sandboxFailOpen(reg.FailPolicy) {
			return nil
		}
		return err
	}
	if !*resp.Valid {
		reason := resp.Reason
		if reason == "" {
			reason = "sandbox config validation failed"
		}
		return fmt.Errorf("sandbox config validation failed: %s", reason)
	}
	return nil
}

func (p sandboxHostedPlugin) resolveRoute(req api.RouteResolveRequest, reg SandboxHandlerRegistration) (api.RouteDecision, error) {
	resp, err := p.invoke(req.Context, reg, SandboxInvokeRequest{RouteResolve: &req})
	if err != nil {
		if sandboxFailClosed(reg.FailPolicy) {
			return api.RouteDecision{
				Action:     api.RouteDecisionReject,
				Host:       req.Host,
				ProviderID: p.pluginID(),
				Reason:     sandboxStableErrorReason(err),
			}, nil
		}
		return api.RouteDecision{}, api.ErrPass
	}
	if resp.RouteDecision == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox route.resolve/v1 response missing route_decision")
		if sandboxFailClosed(reg.FailPolicy) {
			return api.RouteDecision{
				Action:     api.RouteDecisionReject,
				Host:       req.Host,
				ProviderID: p.pluginID(),
				Reason:     err.Error(),
			}, nil
		}
		return api.RouteDecision{}, api.ErrPass
	}
	decision := *resp.RouteDecision
	if decision.ProviderID == "" {
		decision.ProviderID = p.pluginID()
	}
	if decision.Host == "" {
		decision.Host = req.Host
	}
	return decision, nil
}

func (p sandboxHostedPlugin) evaluateRule(req api.RuleEvaluateRequest, reg SandboxHandlerRegistration) (api.RuleEvaluateDecision, error) {
	resp, err := p.invoke(req.Context, reg, SandboxInvokeRequest{RuleEvaluate: &req})
	if err != nil {
		if sandboxFailClosed(reg.FailPolicy) {
			return api.RuleEvaluateDecision{
				Deny:       true,
				Reject:     true,
				ProviderID: p.pluginID(),
				Reason:     sandboxStableErrorReason(err),
			}, nil
		}
		return api.RuleEvaluateDecision{}, api.ErrPass
	}
	if resp.RuleDecision == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox rule.evaluate/v1 response missing rule_decision")
		if sandboxFailClosed(reg.FailPolicy) {
			return api.RuleEvaluateDecision{
				Deny:       true,
				Reject:     true,
				ProviderID: p.pluginID(),
				Reason:     err.Error(),
			}, nil
		}
		return api.RuleEvaluateDecision{}, api.ErrPass
	}
	decision := *resp.RuleDecision
	if decision.ProviderID == "" {
		decision.ProviderID = p.pluginID()
	}
	return decision, nil
}

func (p sandboxHostedPlugin) statusPing(req api.StatusPingRequest, reg SandboxHandlerRegistration) (api.StatusPingResponse, error) {
	resp, err := p.invoke(req.Context, reg, SandboxInvokeRequest{StatusPing: &req})
	if err != nil {
		if sandboxFailOpen(reg.FailPolicy) {
			return api.StatusPingResponse{}, api.ErrPass
		}
		return api.StatusPingResponse{}, err
	}
	if resp.StatusResponse == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox status.ping/v1 response missing status_response")
		if sandboxFailOpen(reg.FailPolicy) {
			return api.StatusPingResponse{}, api.ErrPass
		}
		return api.StatusPingResponse{}, err
	}
	return *resp.StatusResponse, nil
}

func (p sandboxHostedPlugin) queryProvider(reg SandboxHandlerRegistration) (api.ProviderRegistration, error) {
	resp, err := p.invoke(context.Background(), reg, SandboxInvokeRequest{
		Provider: &SandboxProviderQueryRequest{ExtensionPoint: reg.ExtensionPoint},
	})
	if err != nil {
		return api.ProviderRegistration{}, err
	}
	if resp.Provider == nil {
		return api.ProviderRegistration{}, newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox provider/v1 response missing provider")
	}
	provider := *resp.Provider
	if provider.Type == "" {
		provider.Type = ExtensionProvider
	}
	if provider.Name == "" {
		provider.Name = p.pluginID()
	}
	return provider, nil
}

func (p sandboxHostedPlugin) deliverEvent(req api.EventDeliveryRequest, reg SandboxHandlerRegistration) (api.EventDeliveryResult, error) {
	resp, err := p.invoke(req.Context, reg, SandboxInvokeRequest{EventDelivery: &req})
	if err != nil {
		if sandboxFailOpen(reg.FailPolicy) {
			return api.EventDeliveryResult{OK: true, Reason: sandboxStableErrorReason(err)}, nil
		}
		return api.EventDeliveryResult{}, err
	}
	if resp.EventResult == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox event.subscriber/v1 response missing event_result")
		if sandboxFailOpen(reg.FailPolicy) {
			return api.EventDeliveryResult{OK: true, Reason: err.Error()}, nil
		}
		return api.EventDeliveryResult{}, err
	}
	return *resp.EventResult, nil
}

func (p sandboxHostedPlugin) openStream(ctx context.Context, req api.UpstreamConnectRequest, reg SandboxHandlerRegistration) (net.Conn, error) {
	if p.process == nil {
		return nil, newSandboxInvocationError(sandboxControlErrorProcessExited, reg.FailPolicy, "sandbox process is nil")
	}
	if p.process.UpstreamMode != UpstreamModeProtocolProxy {
		return nil, newSandboxInvocationError(sandboxControlErrorNotImplemented, reg.FailPolicy, "sandbox-process only supports upstream.connect/v1 through stream.proxy/v1 protocol-proxy")
	}
	callerCtx := pluginHostCallerContext(ctx)
	if callerCtx == nil {
		callerCtx = context.Background()
	}
	controlCtx := callerCtx
	if timeout := sandboxHandlerTimeoutForExtension(p.process.Registrations, reg.ExtensionPoint); timeout > 0 {
		var cancel context.CancelFunc
		controlCtx, cancel = context.WithTimeout(callerCtx, timeout)
		defer cancel()
	}
	streamReq := sandboxStreamOpenRequest(req, reg)
	payload, err := json.Marshal(streamReq)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := p.process.sendControlRequest(controlCtx, sandboxControlCommandStreamOpen, reg.ExtensionPoint, payload)
	duration := time.Since(start)
	if err != nil {
		p.process.recordSandboxTrace(callerCtx, reg, sandboxErrorCode(err), duration)
		return nil, err
	}
	if !resp.OK {
		err := sandboxResponseError(resp, reg.FailPolicy)
		p.process.recordSandboxTrace(callerCtx, reg, sandboxErrorCode(err), duration)
		return nil, err
	}
	if resp.Stream == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox stream_open response missing stream payload")
		p.process.recordSandboxTrace(callerCtx, reg, sandboxControlErrorBadResponse, duration)
		return nil, err
	}
	stream := *resp.Stream
	if stream.Protocol == "" {
		stream.Protocol = StreamProxyProtocolV1
	}
	if stream.StreamID == "" {
		stream.StreamID = streamReq.StreamID
	}
	if err := validateSandboxStreamOpenResponse(streamReq, stream); err != nil {
		status := sandboxControlErrorBadResponse
		if errors.Is(err, api.ErrPass) {
			status = "pass"
		}
		p.process.recordSandboxTrace(callerCtx, reg, status, duration)
		return nil, err
	}
	dialer := p.process.streamDialer
	if dialer == nil {
		dialer = dialSandboxStreamEndpoint
	}
	endpoint, err := dialer(controlCtx, p.process, stream)
	if err != nil {
		p.process.recordSandboxTrace(callerCtx, reg, sandboxErrorCode(err), duration)
		return nil, err
	}
	if deadline, ok := callerCtx.Deadline(); ok {
		_ = endpoint.SetDeadline(deadline)
	}
	endpoint = bindSandboxStreamEndpoint(callerCtx, p.process, stream.StreamID, endpoint)
	p.process.recordSandboxTrace(callerCtx, reg, "ok", duration)
	return endpoint, nil
}

func sandboxStreamOpenRequest(req api.UpstreamConnectRequest, reg SandboxHandlerRegistration) SandboxStreamOpenRequest {
	sourceAddr := req.SourceAddr
	if sourceAddr == "" && req.Source != nil && req.Source.RemoteAddr() != nil {
		sourceAddr = req.Source.RemoteAddr().String()
	}
	out := SandboxStreamOpenRequest{
		ExtensionPoint:   reg.ExtensionPoint,
		HandlerID:        reg.HandlerID,
		FailPolicy:       reg.FailPolicy,
		Protocol:         StreamProxyProtocolV1,
		StreamID:         newSandboxStreamID(),
		ConnectionID:     req.ConnectionID,
		TraceID:          req.TraceID,
		Host:             req.Host,
		Upstream:         req.Upstream,
		Metadata:         redactSandboxStreamMetadata(req.Metadata),
		SourceAddr:       sourceAddr,
		ServerHost:       req.ServerHost,
		RawServerHost:    req.RawServerHost,
		ProtocolVersion:  req.ProtocolVersion,
		NextState:        req.NextState,
		RouteID:          req.RouteID,
		RouteTags:        append([]string(nil), req.RouteTags...),
		UpstreamRaw:      req.UpstreamRaw,
		UpstreamProtocol: req.UpstreamProtocol,
		UpstreamAddress:  req.UpstreamAddress,
		Transport:        req.Transport,
		ServiceName:      req.ServiceName,
		ListenerPort:     req.ListenerPort,
	}
	if ctx := pluginHostCallerContext(req.Context); ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			out.DeadlineUnixMS = deadline.UnixMilli()
		}
	}
	return out
}

func validateSandboxStreamOpenResponse(req SandboxStreamOpenRequest, resp SandboxStreamOpenResponse) error {
	if !resp.Connected {
		return api.ErrPass
	}
	if strings.TrimSpace(resp.Protocol) != StreamProxyProtocolV1 {
		return newSandboxInvocationError(sandboxControlErrorBadResponse, req.FailPolicy, "sandbox stream_open response protocol mismatch")
	}
	if strings.TrimSpace(resp.StreamID) != req.StreamID {
		return newSandboxInvocationError(sandboxControlErrorBadResponse, req.FailPolicy, "sandbox stream_open response stream_id mismatch")
	}
	if strings.TrimSpace(resp.Endpoint) == "" {
		return newSandboxInvocationError(sandboxControlErrorBadResponse, req.FailPolicy, "sandbox stream_open response endpoint is required")
	}
	if endpointType := strings.TrimSpace(resp.EndpointType); endpointType != "" && endpointType != "unix" {
		return newSandboxInvocationError(sandboxControlErrorBadResponse, req.FailPolicy, "sandbox stream_open endpoint_type must be unix")
	}
	return nil
}

func dialSandboxStreamEndpoint(ctx context.Context, process *SandboxProcess, stream SandboxStreamOpenResponse) (net.Conn, error) {
	address, err := sandboxStreamUnixAddress(process, stream)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	return conn, nil
}

func sandboxStreamUnixAddress(process *SandboxProcess, stream SandboxStreamOpenResponse) (string, error) {
	endpoint := strings.TrimSpace(stream.Endpoint)
	if strings.HasPrefix(endpoint, "unix://") {
		endpoint = strings.TrimPrefix(endpoint, "unix://")
	}
	if endpoint == "" {
		return "", errors.New("sandbox stream endpoint is required")
	}
	if strings.Contains(endpoint, "\x00") {
		return "", errors.New("sandbox stream endpoint contains NUL")
	}
	if strings.HasPrefix(endpoint, "/run/") && process != nil && strings.TrimSpace(process.RootDir) != "" {
		endpoint = filepath.Join(process.RootDir, strings.TrimPrefix(endpoint, "/"))
	} else if !filepath.IsAbs(endpoint) {
		if process == nil || strings.TrimSpace(process.RootDir) == "" {
			return "", errors.New("sandbox stream endpoint must be absolute")
		}
		endpoint = filepath.Join(process.RootDir, endpoint)
	}
	if process != nil && strings.TrimSpace(process.RootDir) != "" {
		root, err := filepath.Abs(process.RootDir)
		if err != nil {
			return "", err
		}
		address, err := filepath.Abs(endpoint)
		if err != nil {
			return "", err
		}
		if address != root && !strings.HasPrefix(address, root+string(filepath.Separator)) {
			return "", errors.New("sandbox stream endpoint escapes sandbox root")
		}
		endpoint = address
	}
	return endpoint, nil
}

func bindSandboxStreamEndpoint(ctx context.Context, process *SandboxProcess, streamID string, conn net.Conn) net.Conn {
	if conn == nil {
		return nil
	}
	done := make(chan struct{})
	closeSignal := make(chan struct{})
	var once sync.Once
	var signalOnce sync.Once
	stop := func() {
		once.Do(func() { close(done) })
	}
	signalClose := func() {
		signalOnce.Do(func() { close(closeSignal) })
	}
	go func() {
		var processDone <-chan struct{}
		if process != nil {
			processDone = process.done
		}
		var ctxDone <-chan struct{}
		if ctx != nil {
			ctxDone = ctx.Done()
		}
		select {
		case <-ctxDone:
			signalClose()
			_ = conn.Close()
		case <-processDone:
			signalClose()
			_ = conn.Close()
		case <-done:
		}
	}()
	return &sandboxStreamConn{
		Conn:        &contextBoundConn{Conn: conn, stop: stop},
		process:     process,
		streamID:    streamID,
		closeSignal: closeSignal,
	}
}

type sandboxStreamConn struct {
	net.Conn
	process     *SandboxProcess
	streamID    string
	closeSignal <-chan struct{}
	once        sync.Once
}

func (c *sandboxStreamConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.notifyClosed()
	})
	return err
}

func (c *sandboxStreamConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (c *sandboxStreamConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
}

func (c *sandboxStreamConn) ProxyCloseSignal() <-chan struct{} {
	return c.closeSignal
}

func (c *sandboxStreamConn) notifyClosed() {
	if c.process == nil || strings.TrimSpace(c.streamID) == "" {
		return
	}
	select {
	case <-c.process.done:
		return
	default:
	}
	payload, err := json.Marshal(SandboxStreamCloseRequest{
		Protocol: StreamProxyProtocolV1,
		StreamID: c.streamID,
		Reason:   "gateway_stream_closed",
	})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = c.process.sendControlRequest(ctx, sandboxControlCommandStreamClose, ExtensionUpstreamConnect, payload)
	}()
}

func newSandboxStreamID() string {
	return "stream-" + strings.TrimPrefix(newSandboxRuntimeInstanceID(), "sandbox-")
}

func redactSandboxStreamMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		cleanKey := strings.TrimSpace(key)
		if cleanKey == "" {
			continue
		}
		value := metadata[key]
		if sandboxStreamSensitiveText(cleanKey) || sandboxStreamSensitiveText(value) {
			out[cleanKey] = "[redacted]"
			continue
		}
		out[cleanKey] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sandboxStreamSensitiveText(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"secret", "token", "password", "authorization"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (p sandboxHostedPlugin) invoke(ctx context.Context, reg SandboxHandlerRegistration, req SandboxInvokeRequest) (SandboxInvokeResponse, error) {
	if p.process == nil {
		return SandboxInvokeResponse{}, newSandboxInvocationError(sandboxControlErrorProcessExited, reg.FailPolicy, "sandbox process is nil")
	}
	req.ExtensionPoint = reg.ExtensionPoint
	req.HandlerID = reg.HandlerID
	if req.FailPolicy == "" {
		req.FailPolicy = reg.FailPolicy
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return SandboxInvokeResponse{}, err
	}
	start := time.Now()
	resp, err := p.process.sendControlRequest(ctx, sandboxControlCommandInvoke, reg.ExtensionPoint, payload)
	duration := time.Since(start)
	if err != nil {
		p.process.recordSandboxTrace(ctx, reg, sandboxErrorCode(err), duration)
		return SandboxInvokeResponse{}, err
	}
	if !resp.OK {
		err := sandboxResponseError(resp, reg.FailPolicy)
		p.process.recordSandboxTrace(ctx, reg, sandboxErrorCode(err), duration)
		return SandboxInvokeResponse{}, err
	}
	if resp.Invoke == nil {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox invoke response missing invoke payload")
		p.process.recordSandboxTrace(ctx, reg, sandboxControlErrorBadResponse, duration)
		return SandboxInvokeResponse{}, err
	}
	invoke := *resp.Invoke
	if invoke.ExtensionPoint == "" {
		invoke.ExtensionPoint = reg.ExtensionPoint
	}
	if invoke.HandlerID == "" {
		invoke.HandlerID = reg.HandlerID
	}
	if invoke.ExtensionPoint != reg.ExtensionPoint || invoke.HandlerID != reg.HandlerID {
		err := newSandboxInvocationError(sandboxControlErrorBadResponse, reg.FailPolicy, "sandbox invoke response identity mismatch")
		p.process.recordSandboxTrace(ctx, reg, sandboxControlErrorBadResponse, duration)
		return SandboxInvokeResponse{}, err
	}
	if !invoke.OK && invoke.ErrorCode != "" {
		err := newSandboxInvocationError(invoke.ErrorCode, reg.FailPolicy, invoke.Error)
		p.process.recordSandboxTrace(ctx, reg, sandboxErrorCode(err), duration)
		return SandboxInvokeResponse{}, err
	}
	p.process.recordSandboxTrace(ctx, reg, "ok", duration)
	return invoke, nil
}

func (p sandboxHostedPlugin) registrationFor(point string) (SandboxHandlerRegistration, bool) {
	if p.process == nil {
		return SandboxHandlerRegistration{}, false
	}
	for _, reg := range p.process.Registrations {
		if reg.ExtensionPoint == point {
			return reg, true
		}
	}
	return SandboxHandlerRegistration{}, false
}

func (p sandboxHostedPlugin) pluginID() string {
	if p.process == nil {
		return ""
	}
	return p.process.PluginID
}

func sandboxHostedPluginFromInstance(instance api.Plugin) sandboxHostedPlugin {
	switch typed := instance.(type) {
	case sandboxHostedPlugin:
		return typed
	case *sandboxHostedPlugin:
		if typed != nil {
			return *typed
		}
	}
	return sandboxHostedPlugin{}
}

func (p *SandboxProcess) sendControlRequest(ctx context.Context, command, extensionPoint string, payload json.RawMessage) (SandboxControlResponse, error) {
	if p == nil {
		return SandboxControlResponse{}, newSandboxInvocationError(sandboxControlErrorProcessExited, sandboxFailPolicyForExtension(extensionPoint), "sandbox process is nil")
	}
	select {
	case <-p.done:
		return SandboxControlResponse{}, newSandboxInvocationError(sandboxControlErrorProcessExited, sandboxFailPolicyForExtension(extensionPoint), "sandbox process exited")
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout := sandboxHandlerTimeoutForExtension(p.Registrations, extensionPoint); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req := SandboxControlRequest{
		RequestID:         newSandboxRuntimeInstanceID(),
		Command:           command,
		Protocol:          sandboxProcessProtocol,
		PluginID:          p.PluginID,
		ArtifactID:        p.ArtifactID,
		RuntimeInstanceID: p.RuntimeInstanceID,
		Generation:        p.Generation,
		TraceID:           sandboxTraceID(ctx),
		Payload:           payload,
	}
	if deadline, ok := ctx.Deadline(); ok {
		req.DeadlineUnixMS = deadline.UnixMilli()
	}
	invoker := p.controlInvoker
	if invoker == nil {
		invoker = SendSandboxControlRequest
	}
	resp, err := invoker(ctx, p.SocketPath, req)
	if err != nil {
		return SandboxControlResponse{}, mapSandboxControlInvokeError(ctx, p, sandboxFailPolicyForExtension(extensionPoint), err)
	}
	if err := validateSandboxControlInvokeEnvelope(p, req, resp); err != nil {
		return SandboxControlResponse{}, err
	}
	return resp, nil
}

func sandboxHandlerTimeoutForExtension(registrations []SandboxHandlerRegistration, extensionPoint string) time.Duration {
	for _, reg := range registrations {
		if reg.ExtensionPoint == extensionPoint && reg.TimeoutMS > 0 {
			return time.Duration(reg.TimeoutMS) * time.Millisecond
		}
	}
	return DefaultHandlerTimeout
}

func validateSandboxControlInvokeEnvelope(p *SandboxProcess, req SandboxControlRequest, resp SandboxControlResponse) error {
	if resp.Protocol != sandboxProcessProtocol {
		return newSandboxInvocationError(sandboxControlErrorBadResponse, sandboxFailPolicyForExtension(""), "sandbox response protocol mismatch")
	}
	if p == nil {
		return nil
	}
	failPolicy := sandboxFailPolicyForExtension("")
	if resp.Invoke != nil {
		failPolicy = sandboxFailPolicyForExtension(resp.Invoke.ExtensionPoint)
	}
	switch {
	case resp.PluginID != "" && resp.PluginID != p.PluginID:
		return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, "sandbox response plugin_id mismatch")
	case resp.ArtifactID != "" && resp.ArtifactID != p.ArtifactID:
		return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, "sandbox response artifact_id mismatch")
	case resp.RuntimeInstanceID != "" && p.RuntimeInstanceID != "" && resp.RuntimeInstanceID != p.RuntimeInstanceID:
		return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, "sandbox response runtime_instance_id mismatch")
	case resp.Generation != 0 && resp.Generation != p.Generation:
		return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, "sandbox response generation mismatch")
	case resp.RequestID != "" && req.RequestID != "" && resp.RequestID != req.RequestID:
		return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, "sandbox response request_id mismatch")
	}
	return nil
}

func (p *SandboxProcess) recordSandboxTrace(ctx context.Context, reg SandboxHandlerRegistration, status string, duration time.Duration) {
	if p == nil || p.operations == nil || p.operations.parent == nil {
		return
	}
	trace := traceFromContext(ctx)
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	if status == "" {
		status = "error"
	}
	_ = p.operations.parent.repo.SaveTrace(context.Background(), TraceSummary{
		PluginID:     p.PluginID,
		TraceID:      trace.TraceID,
		ConnectionID: trace.ConnectionID,
		HandlerID:    reg.HandlerID,
		Operation:    "sandbox.invoke." + reg.ExtensionPoint,
		Status:       status,
		DurationMS:   durationMS,
	}, map[string]string{
		"extension_point": reg.ExtensionPoint,
		"fail_policy":     reg.FailPolicy,
	})
}

type sandboxInvocationError struct {
	code       string
	failPolicy string
	message    string
}

func (e sandboxInvocationError) Error() string {
	if e.message == "" {
		return "sandbox invoke " + e.code
	}
	return "sandbox invoke " + e.code + ": " + e.message
}

func newSandboxInvocationError(code, failPolicy, message string) sandboxInvocationError {
	if code == "" {
		code = sandboxControlErrorBadResponse
	}
	if failPolicy == "" {
		failPolicy = api.FailPolicyClose
	}
	return sandboxInvocationError{code: code, failPolicy: failPolicy, message: redactSandboxControlMessage(message)}
}

func sandboxResponseError(resp SandboxControlResponse, failPolicy string) error {
	code := resp.ErrorCode
	if code == "" {
		code = resp.Code
	}
	if code == "" {
		code = sandboxControlErrorBadResponse
	}
	message := resp.Error
	if message == "" {
		message = "sandbox control request failed"
	}
	return newSandboxInvocationError(code, failPolicy, message)
}

func mapSandboxControlInvokeError(ctx context.Context, p *SandboxProcess, failPolicy string, err error) error {
	if err == nil {
		return nil
	}
	var invokeErr sandboxInvocationError
	if errors.As(err, &invokeErr) {
		return invokeErr
	}
	var controlErr sandboxControlError
	if errors.As(err, &controlErr) {
		return newSandboxInvocationError(controlErr.code, failPolicy, controlErr.message)
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return newSandboxInvocationError(sandboxControlErrorTimeout, failPolicy, ctx.Err().Error())
	}
	if p != nil {
		select {
		case <-p.done:
			return newSandboxInvocationError(sandboxControlErrorProcessExited, failPolicy, "sandbox process exited")
		default:
		}
	}
	return newSandboxInvocationError(sandboxControlErrorBadResponse, failPolicy, err.Error())
}

func sandboxErrorCode(err error) string {
	if err == nil {
		return "ok"
	}
	var invokeErr sandboxInvocationError
	if errors.As(err, &invokeErr) {
		return invokeErr.code
	}
	var controlErr sandboxControlError
	if errors.As(err, &controlErr) {
		return controlErr.code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return sandboxControlErrorTimeout
	}
	return sandboxControlErrorBadResponse
}

func sandboxStableErrorReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sandboxFailOpen(policy string) bool {
	switch policy {
	case api.FailPolicyOpen, ExternalFailPolicyDegraded, ExternalFailPolicyFallback:
		return true
	default:
		return false
	}
}

func sandboxFailClosed(policy string) bool {
	return !sandboxFailOpen(policy)
}

func sandboxFailPolicyForExtension(extensionPoint string) string {
	switch extensionPoint {
	case ExtensionRouteResolve, ExtensionRouteResolver, ExtensionStatusPing, ExtensionEventSubscriber, ExtensionProvider:
		return api.FailPolicyOpen
	default:
		return api.FailPolicyClose
	}
}

func sandboxConfigJSON(config any) (json.RawMessage, error) {
	switch typed := config.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		if len(typed) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return append(json.RawMessage(nil), typed...), nil
	case []byte:
		if len(typed) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return append(json.RawMessage(nil), typed...), nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(typed), nil
	default:
		data, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 || string(data) == "null" {
			return json.RawMessage(`{}`), nil
		}
		return data, nil
	}
}

func sandboxTraceID(ctx context.Context) string {
	trace := traceFromContext(ctx)
	if trace.TraceID != "" {
		return trace.TraceID
	}
	return newSandboxRuntimeInstanceID()
}

func (m *Manager) ResolveSandboxSecret(ctx context.Context, req SandboxSecretRequest) (SandboxSecretResponse, error) {
	req.PluginID = strings.TrimSpace(req.PluginID)
	req.ArtifactID = strings.TrimSpace(req.ArtifactID)
	req.Handle = strings.TrimSpace(req.Handle)
	if req.PluginID == "" || req.ArtifactID == "" || req.Handle == "" || req.Generation <= 0 {
		resp := sandboxSecretDeny("invalid_request", "plugin_id, artifact_id, generation and handle are required")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	plugin, err := m.repo.Plugin(ctx, req.PluginID)
	if err != nil {
		resp := sandboxSecretDeny("plugin_not_found", "plugin is not configured")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	artifact, err := m.repo.Artifact(ctx, req.ArtifactID)
	if err != nil {
		resp := sandboxSecretDeny("artifact_not_found", "artifact is not configured")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	if artifact.PluginID != req.PluginID || !sandboxSecretArtifactCurrent(plugin, req.ArtifactID) {
		resp := sandboxSecretDeny("artifact_mismatch", "secret request artifact does not match current plugin state")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	if req.Generation != plugin.DesiredGeneration && (plugin.AppliedGeneration == 0 || req.Generation != plugin.AppliedGeneration) {
		resp := sandboxSecretDeny("generation_mismatch", "secret request generation does not match current plugin state")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		resp := sandboxSecretDeny("manifest_invalid", "artifact manifest is invalid")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	spec, declared := sandboxManifestSecretSpec(manifest, req.Handle)
	if !declared {
		resp := sandboxSecretDeny("secret_not_declared", "secret handle is not declared by manifest")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	inScope, err := sandboxSecretInCurrentConfigScope(manifest, plugin.ConfigJSON, req.Handle)
	if err != nil {
		resp := sandboxSecretDeny("config_scope_invalid", "current config secret scope is invalid")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	if !inScope {
		resp := sandboxSecretDeny("secret_not_in_config_scope", "secret handle is not in current config scope")
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	material, err := m.repo.SecretMaterial(ctx, req.PluginID, req.Handle)
	if err != nil {
		code := "secret_not_configured"
		if !errors.Is(err, sql.ErrNoRows) {
			code = "secret_lookup_failed"
		}
		resp := sandboxSecretDeny(code, "secret handle is not configured")
		m.auditSandboxSecretDenial(ctx, req, resp)
		if errors.Is(err, sql.ErrNoRows) {
			return resp, nil
		}
		return SandboxSecretResponse{}, err
	}
	value, version, ttl, errCode, errMessage := sandboxSecretVersionValue(req, spec, material, m.repo.now())
	if errCode != "" {
		resp := sandboxSecretDeny(errCode, errMessage)
		m.auditSandboxSecretDenial(ctx, req, resp)
		return resp, nil
	}
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = "plugin:" + req.PluginID + ":secret:" + req.Handle
	}
	now := m.repo.now()
	ttlSeconds := int64((ttl + time.Second - 1) / time.Second)
	expiresAt := now.Add(ttl).Unix()
	resp := SandboxSecretResponse{
		OK:              true,
		TTLSeconds:      ttlSeconds,
		ExpiresAt:       expiresAt,
		Version:         version,
		Scope:           scope,
		RedactionHandle: sandboxSecretRedactionHandle(req.PluginID, req.Handle, version),
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	secretType := strings.ToLower(strings.TrimSpace(spec.Type))
	if mode == "token" || (mode == "" && strings.Contains(secretType, "token")) {
		resp.Token = value
	} else {
		resp.Value = value
	}
	return resp, nil
}

func sandboxSecretArtifactCurrent(plugin PluginRecord, artifactID string) bool {
	for _, candidate := range []string{plugin.ActiveArtifactID, plugin.LoadedArtifactID} {
		if candidate != "" && candidate == artifactID {
			return true
		}
	}
	return plugin.ActiveArtifactID == "" && plugin.LoadedArtifactID == "" && plugin.DesiredArtifactID == artifactID
}

func sandboxManifestSecretSpec(manifest Manifest, handle string) (SecretSpec, bool) {
	for _, spec := range manifest.Secrets {
		if strings.TrimSpace(spec.Name) == handle {
			return spec, true
		}
	}
	return SecretSpec{}, false
}

func sandboxSecretInCurrentConfigScope(manifest Manifest, configJSON, handle string) (bool, error) {
	refs, err := collectConfigSecretRefs(configJSON)
	if err != nil {
		return false, err
	}
	for _, spec := range manifest.Secrets {
		if spec.Required && strings.TrimSpace(spec.Name) != "" {
			refs[spec.Name] = true
		}
	}
	return refs[handle], nil
}

func sandboxSecretVersionValue(req SandboxSecretRequest, spec SecretSpec, material secretMaterial, now time.Time) (string, int64, time.Duration, string, string) {
	version := material.CurrentVersion
	value := material.CurrentValue
	ttl := sandboxSecretDefaultTTL
	if ttl > sandboxSecretMaxTTL {
		ttl = sandboxSecretMaxTTL
	}
	if req.Version == 0 || req.Version == material.CurrentVersion {
		if material.CurrentVersion <= 0 || material.CurrentValue == "" {
			return "", 0, 0, "secret_not_configured", "secret handle is not configured"
		}
		return value, version, ttl, "", ""
	}
	if req.Version != material.PreviousVersion || material.PreviousVersion <= 0 || material.PreviousValue == "" {
		return "", 0, 0, "secret_version_not_available", "requested secret version is not available"
	}
	grace := parseDurationDefault(spec.Rotation.GracePeriod, 30*time.Second)
	if grace <= 0 {
		return "", 0, 0, "previous_secret_expired", "previous secret version grace period has expired"
	}
	expiresAt := time.Unix(material.UpdatedAt, 0).Add(grace)
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return "", 0, 0, "previous_secret_expired", "previous secret version grace period has expired"
	}
	if remaining < ttl {
		ttl = remaining
	}
	return material.PreviousValue, material.PreviousVersion, ttl, "", ""
}

func sandboxSecretRedactionHandle(pluginID, handle string, version int64) string {
	return fmt.Sprintf("plugin-secret://%s/%s@v%d#redacted", pluginID, handle, version)
}

func sandboxSecretDeny(code, message string) SandboxSecretResponse {
	return SandboxSecretResponse{
		OK:        false,
		ErrorCode: code,
		Error:     redactSensitive(message),
	}
}

func (m *Manager) auditSandboxSecretDenial(ctx context.Context, req SandboxSecretRequest, resp SandboxSecretResponse) {
	if m == nil {
		return
	}
	_ = m.repo.RecordOperation(ctx, req.PluginID, req.ArtifactID, "sandbox_secret_resolve", "failed", "sandbox", "sandbox secret request denied", map[string]any{
		"handle":      req.Handle,
		"version":     req.Version,
		"generation":  req.Generation,
		"error_code":  resp.ErrorCode,
		"redacted":    true,
		"secret_ref":  "plugin://" + req.PluginID + "/" + req.Handle,
		"artifact_id": req.ArtifactID,
	})
}
