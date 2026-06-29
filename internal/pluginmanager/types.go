// internal/pluginmanager/types.go 定义仓库、管理器、Admin API 和前端共用的插件管理数据模型。

package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	SchemaVersion  = "mc-gateway.plugin/v1"
	APIVersion     = "plugin-api/v1"
	GatewayRelease = "v0.1.0"

	ArtifactTypeBinary = "binary"
	ArtifactTypeSource = "source"
	RuntimeGoPlugin    = "go-plugin"
	RuntimeBuiltin     = "builtin"
	RuntimeSandbox     = "sandbox-process"
	RuntimeWASM        = "wasm"
	RuntimeEntry       = "plugin.so"
	RuntimeWASMEntry   = "plugin.wasm"
	SourceBuildEntry   = "."

	ExtensionUpstreamConnect   = "upstream.connect/v1"
	ExtensionRouteResolve      = "route.resolve/v1"
	ExtensionRouteResolver     = "route.resolver/v1"
	ExtensionRuleEvaluate      = "rule.evaluate/v1"
	ExtensionConfigValidate    = "config.validate/v1"
	ExtensionStatusPing        = "status.ping/v1"
	ExtensionConnectionFilter  = "connection.filter/v1"
	ExtensionHandshakeFilter   = "handshake.filter/v1"
	ExtensionEventSubscriber   = "event.subscriber/v1"
	ExtensionProvider          = "provider/v1"
	ExtensionAuthProvider      = "auth.provider/v1"
	ExtensionAdminAuthProvider = "admin.auth.provider/v1"
	ExtensionIngressService    = "ingress.service/v1"

	UpstreamModeDialer        = "dialer"
	UpstreamModeProtocolProxy = "protocol-proxy"

	ArtifactStatusUploaded  = "uploaded"
	ArtifactStatusValidated = "validated"
	ArtifactStatusLoadable  = "loadable"
	ArtifactStatusLoaded    = "loaded"
	ArtifactStatusRejected  = "rejected"
	ArtifactStatusDeleted   = "deleted"

	BuildStatusQueued    = "queued"
	BuildStatusRunning   = "running"
	BuildStatusSucceeded = "succeeded"
	BuildStatusFailed    = "failed"
	BuildStatusCanceled  = "canceled"

	BuilderTypeLocalProcess = "local-process"
	BuilderTypeContainer    = "container"
	BuildTypeGo             = "go"

	DesiredEnabled  = "enabled"
	DesiredDisabled = "disabled"
	DesiredDeleted  = "deleted"

	RuntimeNotLoaded = "not_loaded"
	RuntimeLoaded    = "loaded"
	RuntimeEnabled   = "enabled"
	RuntimeFailed    = "failed"
	RuntimeDisabled  = "disabled"
	RuntimeDraining  = "draining"

	PluginServiceModeInProcess       = "in-process"
	PluginServiceModeGoPluginProcess = "go-plugin-process"
	PluginServiceModeSandboxProcess  = "sandbox-process"

	GoPluginProcessPartialUnsupportedReason = "go-plugin-process supports upstream.connect/v1 dialer mode and protocol-proxy drain-only with persisted crash policy and per-node crash isolation; fd-live migration, sandbox enforcement, full isolation, and non-Linux process-table orphan discovery are not implemented"

	PluginNodeStatusOnline = "online"
	PluginNodeStatusStale  = "stale"

	TaskRunPolicyPerNode   = "per_node"
	TaskRunPolicySingleton = "singleton"
	TaskRunPolicySharded   = "sharded"

	FeatureMaturityImplemented = "implemented"
	FeatureMaturityPartial     = "partial"
	FeatureMaturityReserved    = "reserved"
	FeatureMaturityStub        = "stub"

	PluginMigrationDrainOnly = "drain-only"
	PluginMigrationFDLive    = "fd-live"
	PluginMigrationFDLiveSHM = "fd-live-shm"

	RepositoryTypeOfficial = "official"
	RepositoryTypeInternal = "internal"
	RepositoryTypeFile     = "file"
	RepositoryTypeURL      = "url"

	RepositorySyncStatusSucceeded = "succeeded"
	RepositorySyncStatusDegraded  = "degraded"
	RepositorySyncStatusFailed    = "failed"

	SignatureAlgorithmEd25519 = "ed25519"
	TrustRootStatusTrusted    = "trusted"
	TrustRootStatusRevoked    = "revoked"

	SupplyChainStatusAllowed = "allowed"
	SupplyChainStatusBlocked = "blocked"
	SupplyChainStatusWarning = "warning"

	InstrumentationStatusAvailable = "available"
	InstrumentationStatusBlocked   = "blocked"

	PromotionStatusReady   = "ready"
	PromotionStatusBlocked = "blocked"
	PromotionStatusDrift   = "drift"

	PolicyProfileDev     = "dev"
	PolicyProfileStaging = "staging"
	PolicyProfileProd    = "prod"

	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"

	GateSeverityWarning  = "warning"
	GateSeverityBlocking = "blocking"
	GateSeverityInfo     = "info"

	GovernanceActionEnable    = "enable"
	GovernanceActionRollback  = "rollback"
	GovernanceActionPromotion = "promotion_apply"

	AdvisoryActionDenylist   = "denylist"
	AdvisoryActionQuarantine = "quarantine"
	AdvisoryActionRevoke     = "revoke"
	AdvisoryActionMitigate   = "mitigate"

	ReviewDecisionApproved = "approved"
	ReviewDecisionRejected = "rejected"

	AdvisoryStatusActive  = "active"
	AdvisoryStatusRevoked = "revoked"
	AdvisoryStatusAcked   = "acknowledged"

	ExternalFailPolicyOpen     = "fail_open"
	ExternalFailPolicyClosed   = "fail_closed"
	ExternalFailPolicyDegraded = "degraded"
	ExternalFailPolicyFallback = "fallback"

	OperationsExporterPrometheus = "prometheus"
	OperationsExporterOTel       = "otel"

	VulnerabilitySeverityLow      = "low"
	VulnerabilitySeverityMedium   = "medium"
	VulnerabilitySeverityHigh     = "high"
	VulnerabilitySeverityCritical = "critical"

	DefaultPriority                      = 100
	DefaultHandlerTimeout                = 3 * time.Second
	DefaultSubscriberRetryDelay          = 100 * time.Millisecond
	DefaultSubscriberMaxRetry            = 3
	DefaultManifestMaxBytes              = 256 * 1024
	DefaultPackageMaxBytes               = 64 * 1024 * 1024
	DefaultPackageMaxEntries             = 2048
	DefaultExtractedMaxBytes             = 256 * 1024 * 1024
	DefaultNonRuntimeMaxBytes            = 16 * 1024 * 1024
	DefaultInitialWriteTimeout           = time.Second
	DefaultExternalTimeout               = 5 * time.Second
	DefaultBuildLogMaxBytes              = 64 * 1024
	DefaultEventQueueLimit               = 1000
	DefaultSubscriberDeadLetterLimit     = 1000
	DefaultEventRecentLimit              = 1000
	DefaultLabelValueMaxBytes            = 64
	DefaultPluginDataQuota               = 16 * 1024 * 1024
	DefaultPluginDataKeyLimit            = 256 * 1024
	DefaultPluginFileQuota               = 32 * 1024 * 1024
	DefaultLogRecentLimit                = 500
	DefaultTaskLeaseTTL                  = time.Minute
	DefaultPluginNodeStaleAfter          = 2 * time.Minute
	DefaultPluginHostCrashBackoffSeconds = int64(30)
	DefaultPluginHostCrashMaxCrashes     = int64(1)
	DefaultPluginHostCrashWindowSeconds  = int64(300)
	DefaultDiagnosticRetention           = 7 * 24 * time.Hour
)

var (
	ErrArtifactNotFound = errors.New("plugin artifact not found")
	ErrPluginNotFound   = errors.New("plugin not found")
)

type Manifest struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Version          string           `json:"version"`
	Description      string           `json:"description"`
	ArtifactType     string           `json:"artifact_type"`
	Runtime          RuntimeManifest  `json:"runtime"`
	Build            BuildManifest    `json:"build,omitempty"`
	APIVersion       string           `json:"api_version"`
	SDKModule        string           `json:"sdk_module"`
	SDKModuleVersion string           `json:"sdk_module_version"`
	GoVersion        string           `json:"go_version"`
	GOOS             string           `json:"go_os"`
	GOARCH           string           `json:"go_arch"`
	ExtensionPoints  []ExtensionPoint `json:"extension_points"`
	Capabilities     json.RawMessage  `json:"capabilities"`
	RuntimeLimits    RuntimeLimits    `json:"runtime_limits"`
	ConfigSchema     json.RawMessage  `json:"config_schema"`
	Secrets          []SecretSpec     `json:"secrets,omitempty"`
	Events           []EventSpec      `json:"events,omitempty"`
	CustomMetrics    []MetricSpec     `json:"custom_metrics,omitempty"`
	BackgroundTasks  []TaskSpec       `json:"background_tasks,omitempty"`
	ExternalDeps     []ExternalSpec   `json:"external_dependencies,omitempty"`
	DataStores       []DataStoreSpec  `json:"data_stores,omitempty"`
	FileStores       []FileStoreSpec  `json:"file_stores,omitempty"`
	SupplyChain      json.RawMessage  `json:"supply_chain"`
}

type RuntimeManifest struct {
	Type        string `json:"type"`
	Entry       string `json:"entry"`
	BuildEntry  string `json:"build_entry"`
	EntrySymbol string `json:"entry_symbol"`
}

type BuildManifest struct {
	Type           string   `json:"type"`
	Entry          string   `json:"entry"`
	GoVersion      string   `json:"go_version"`
	CGOEnabled     *bool    `json:"cgo_enabled,omitempty"`
	Tags           []string `json:"tags"`
	VendorRequired bool     `json:"vendor_required"`
	Output         string   `json:"output"`
}

type ExtensionPoint struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

type RuntimeLimits struct {
	HandlerTimeoutMS      int `json:"handler_timeout_ms"`
	InitialWriteTimeoutMS int `json:"initial_write_timeout_ms"`
	MemoryBytes           int `json:"memory_bytes,omitempty"`
}

type SecretSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required"`
	Type        string         `json:"type,omitempty"`
	Rotation    SecretRotation `json:"rotation,omitempty"`
}

type SecretRotation struct {
	Strategy    string `json:"strategy,omitempty"`
	GracePeriod string `json:"grace_period,omitempty"`
	Reload      string `json:"reload,omitempty"`
}

type EventSpec struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields,omitempty"`
}

type MetricSpec struct {
	Name   string   `json:"name"`
	Type   string   `json:"type,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

type TaskSpec struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Interval    string `json:"interval,omitempty"`
	RunOnStart  bool   `json:"run_on_start,omitempty"`
	Jitter      string `json:"jitter,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Retry       int    `json:"retry,omitempty"`
	Manual      bool   `json:"manual,omitempty"`
	RunPolicy   string `json:"run_policy,omitempty"`
	ShardKey    string `json:"shard_key,omitempty"`
	LeaseTTL    string `json:"lease_ttl,omitempty"`
	RequireRole string `json:"require_role,omitempty"`
}

type ExternalSpec struct {
	Name        string   `json:"name"`
	Endpoint    string   `json:"endpoint"`
	Purpose     string   `json:"purpose,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retry       int      `json:"retry,omitempty"`
	FailPolicy  string   `json:"fail_policy,omitempty"`
	DataClasses []string `json:"data_classes,omitempty"`
	Traceparent bool     `json:"traceparent,omitempty"`
}

type DataStoreSpec struct {
	Name          string `json:"name"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	DataClass     string `json:"data_class,omitempty"`
	QuotaBytes    int64  `json:"quota_bytes,omitempty"`
	Retention     string `json:"retention,omitempty"`
	Exportable    bool   `json:"exportable,omitempty"`
}

type FileStoreSpec struct {
	Namespace  string `json:"namespace"`
	DataClass  string `json:"data_class,omitempty"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
	Retention  string `json:"retention,omitempty"`
	Readonly   bool   `json:"readonly,omitempty"`
}

type CapabilitySummary struct {
	UpstreamConnect UpstreamConnectCapability `json:"upstream_connect,omitempty"`
	Route           RouteCapability           `json:"route,omitempty"`
	Status          StatusCapability          `json:"status,omitempty"`
	Middleware      MiddlewareCapability      `json:"middleware,omitempty"`
	Providers       []ProviderCapability      `json:"providers,omitempty"`
	EventSubscriber EventSubscriberCapability `json:"event_subscriber,omitempty"`
	Ingress         *IngressCapability        `json:"ingress,omitempty"`
	Minecraft       *MinecraftCapability      `json:"minecraft,omitempty"`
	Events          []EventSpec               `json:"events,omitempty"`
	CustomMetrics   []MetricSpec              `json:"custom_metrics,omitempty"`
	ExternalDeps    []ExternalSpec            `json:"external_dependencies,omitempty"`
	DataStores      []DataStoreSpec           `json:"data_stores,omitempty"`
	FileStores      []FileStoreSpec           `json:"file_stores,omitempty"`
	Runtime         RuntimeCapability         `json:"runtime,omitempty"`
	Raw             json.RawMessage           `json:"raw,omitempty"`
}

type RuntimeCapability struct {
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	RequiredFeatures     []string `json:"required_features,omitempty"`
}

type UpstreamConnectCapability struct {
	Mode string `json:"mode,omitempty"`
}

type RouteCapability struct {
	CacheTTLMS int `json:"cache_ttl_ms,omitempty"`
}

type StatusCapability struct {
	Hosts []string `json:"hosts,omitempty"`
}

type MiddlewareCapability struct {
	FailPolicy string `json:"fail_policy,omitempty"`
}

type ProviderCapability struct {
	Type         string   `json:"type,omitempty"`
	Name         string   `json:"name,omitempty"`
	Priority     int      `json:"priority,omitempty"`
	Fallback     bool     `json:"fallback,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type EventSubscriberCapability struct {
	Mode       string `json:"mode,omitempty"`
	QueueLimit int    `json:"queue_limit,omitempty"`
	MaxRetry   int    `json:"max_retry,omitempty"`
}

type IngressCapability struct {
	Protocol   string                   `json:"protocol,omitempty"`
	Bind       string                   `json:"bind,omitempty"`
	Port       int                      `json:"port,omitempty"`
	TLS        *IngressTLSCapability    `json:"tls,omitempty"`
	Health     *IngressHealthCapability `json:"health,omitempty"`
	SecretRefs []string                 `json:"secret_refs,omitempty"`
}

type IngressTLSCapability struct {
	Enabled    bool   `json:"enabled,omitempty"`
	CertSecret string `json:"cert_secret,omitempty"`
	KeySecret  string `json:"key_secret,omitempty"`
}

type IngressHealthCapability struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

type IngressReservedListener struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Bind    string `json:"bind,omitempty"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
}

type FutureRuntimeGates struct {
	SandboxProcess bool `json:"sandbox_process"`
	WASM           bool `json:"wasm"`
	Ingress        bool `json:"ingress"`
}

type SandboxPolicy struct {
	FilesystemRoots []string          `json:"filesystem_roots,omitempty"`
	NetworkEnabled  bool              `json:"network_enabled"`
	Env             map[string]string `json:"env,omitempty"`
	CPUSeconds      int64             `json:"cpu_seconds,omitempty"`
	MemoryBytes     int64             `json:"memory_bytes,omitempty"`
	SecretHandles   []string          `json:"secret_handles,omitempty"`
}

type SandboxSecretRequest struct {
	PluginID string `json:"plugin_id"`
	Handle   string `json:"handle"`
}

type SandboxSecretResponse struct {
	OK      bool   `json:"ok"`
	Version int64  `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type SandboxDiagnosticSummary struct {
	PluginID              string            `json:"plugin_id"`
	ArtifactID            string            `json:"artifact_id"`
	PID                   int               `json:"pid,omitempty"`
	State                 string            `json:"state"`
	ControlRPC            bool              `json:"control_rpc"`
	FilesystemEnforced    bool              `json:"filesystem_enforced"`
	NetworkEnforced       bool              `json:"network_enforced"`
	EnvEnforced           bool              `json:"env_enforced"`
	CPUMemoryEnforced     bool              `json:"cpu_memory_enforced"`
	SecretRPC             bool              `json:"secret_rpc"`
	CrashLoop             bool              `json:"crash_loop"`
	CrashCount            int               `json:"crash_count"`
	LastError             string            `json:"last_error,omitempty"`
	SecretHandles         []string          `json:"secret_handles,omitempty"`
	EnvKeys               []string          `json:"env_keys,omitempty"`
	ControlSocket         string            `json:"control_socket,omitempty"`
	UnsupportedReason     string            `json:"unsupported_reason,omitempty"`
	EnforcementAttributes map[string]string `json:"enforcement_attributes,omitempty"`
}

type MinecraftCapability struct {
	ProtocolVersions  MinecraftProtocolVersions `json:"protocol_versions,omitempty"`
	States            map[string]string         `json:"states,omitempty"`
	AuthModes         []string                  `json:"auth_modes,omitempty"`
	Forwarding        MinecraftForwarding       `json:"forwarding,omitempty"`
	UnsupportedPolicy string                    `json:"unsupported_policy,omitempty"`
	Modded            map[string]string         `json:"modded,omitempty"`
}

type MinecraftProtocolVersions struct {
	Min               int    `json:"min,omitempty"`
	Max               int    `json:"max,omitempty"`
	Tested            []int  `json:"tested,omitempty"`
	UnsupportedPolicy string `json:"unsupported_policy,omitempty"`
}

type MinecraftForwarding struct {
	Supported      []string `json:"supported,omitempty"`
	Default        string   `json:"default,omitempty"`
	RequiresSecret bool     `json:"requires_secret,omitempty"`
}

type ArtifactRecord struct {
	ID                      string `json:"id"`
	PluginID                string `json:"plugin_id"`
	Version                 string `json:"version"`
	FileName                string `json:"file_name"`
	FilePath                string `json:"file_path"`
	SHA256                  string `json:"sha256"`
	PackageSHA256           string `json:"package_sha256"`
	SizeBytes               int64  `json:"size_bytes"`
	ArtifactType            string `json:"artifact_type"`
	RuntimeType             string `json:"runtime_type"`
	RuntimeEntry            string `json:"runtime_entry"`
	Status                  string `json:"status"`
	MetadataJSON            string `json:"metadata_json"`
	CapabilitiesSummaryJSON string `json:"capabilities_summary_json"`
	ExtensionPointsJSON     string `json:"extension_points_json"`
	APIVersion              string `json:"api_version"`
	GoVersion               string `json:"go_version"`
	GOOS                    string `json:"go_os"`
	GOARCH                  string `json:"go_arch"`
	UploadedBy              string `json:"uploaded_by"`
	Error                   string `json:"error"`
	CreatedAt               int64  `json:"created_at"`
	UpdatedAt               int64  `json:"updated_at"`
}

type PluginRecord struct {
	ID                  string `json:"id"`
	DesiredArtifactID   string `json:"desired_artifact_id"`
	ActiveArtifactID    string `json:"active_artifact_id"`
	LoadedArtifactID    string `json:"loaded_artifact_id"`
	DesiredState        string `json:"desired_state"`
	RuntimeState        string `json:"runtime_state"`
	Priority            int    `json:"priority"`
	ConfigJSON          string `json:"config_json"`
	DesiredGeneration   int64  `json:"desired_generation"`
	AppliedGeneration   int64  `json:"applied_generation"`
	LastError           string `json:"last_error"`
	RuntimeSummaryJSON  string `json:"runtime_summary_json"`
	DispatchSummaryJSON string `json:"dispatch_summary_json"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
	UpdatedBy           string `json:"updated_by"`
}

type ConfigSnapshotRecord struct {
	ID                int64  `json:"id"`
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	ConfigJSON        string `json:"config_json"`
	DesiredState      string `json:"desired_state"`
	Priority          int    `json:"priority"`
	DesiredGeneration int64  `json:"desired_generation"`
	CreatedBy         string `json:"created_by"`
	CreatedAt         int64  `json:"created_at"`
}

type ConfigDryRunResult struct {
	OK                 bool     `json:"ok"`
	PluginID           string   `json:"plugin_id"`
	ArtifactID         string   `json:"artifact_id"`
	RestartRequired    bool     `json:"restart_required"`
	HotReload          bool     `json:"hot_reload"`
	SensitivePaths     []string `json:"sensitive_paths"`
	RedactedConfigJSON string   `json:"redacted_config_json"`
	RedactedDiffJSON   string   `json:"redacted_diff_json"`
	Error              string   `json:"error,omitempty"`
}

type ConfigSnapshotDiff struct {
	SnapshotID         int64    `json:"snapshot_id"`
	PluginID           string   `json:"plugin_id"`
	ArtifactID         string   `json:"artifact_id"`
	SensitivePaths     []string `json:"sensitive_paths"`
	RedactedDiffJSON   string   `json:"redacted_diff_json"`
	RestartRequired    bool     `json:"restart_required"`
	CurrentGeneration  int64    `json:"current_generation"`
	SnapshotGeneration int64    `json:"snapshot_generation"`
}

type SecretRecord struct {
	PluginID        string `json:"plugin_id"`
	Name            string `json:"name"`
	CurrentVersion  int64  `json:"current_version"`
	PreviousVersion int64  `json:"previous_version"`
	ReloadRequired  bool   `json:"reload_required"`
	HotReload       bool   `json:"hot_reload"`
	UpdatedBy       string `json:"updated_by"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type PolicySnapshot struct {
	Profile                   string  `json:"profile"`
	WarningOverrideTTLSeconds int64   `json:"warning_override_ttl_seconds"`
	ReviewRequiredRisk        string  `json:"review_required_risk"`
	WarnBenchmarkRegression   float64 `json:"warn_benchmark_regression"`
	BlockBenchmarkRegression  float64 `json:"block_benchmark_regression"`
	RequireConformanceFixture bool    `json:"require_conformance_fixture,omitempty"`
	CreatedAt                 int64   `json:"created_at"`
}

type GovernanceIssue struct {
	Code       string         `json:"code"`
	Severity   string         `json:"severity"`
	Message    string         `json:"message"`
	PluginID   string         `json:"plugin_id,omitempty"`
	ArtifactID string         `json:"artifact_id,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

type GovernanceDecision struct {
	OK                  bool              `json:"ok"`
	Action              string            `json:"action"`
	Profile             string            `json:"profile"`
	RiskLevel           string            `json:"risk_level"`
	PolicyHash          string            `json:"policy_hash"`
	ReviewRequired      bool              `json:"review_required"`
	WarningOverrideUsed bool              `json:"warning_override_used"`
	Issues              []GovernanceIssue `json:"issues"`
	Checks              []GovernanceIssue `json:"checks"`
	CreatedAt           int64             `json:"created_at"`
}

type ConflictAnalysis struct {
	OK        bool              `json:"ok"`
	Issues    []GovernanceIssue `json:"issues"`
	Plan      DispatchPlan      `json:"plan"`
	CreatedAt int64             `json:"created_at"`
}

type PreflightCheck struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

type PreflightResult struct {
	OK        bool             `json:"ok"`
	Profile   string           `json:"profile"`
	Checks    []PreflightCheck `json:"checks"`
	CreatedAt int64            `json:"created_at"`
}

type ConformanceSummary struct {
	Source         string                      `json:"source"`
	OK             bool                        `json:"ok"`
	Total          int                         `json:"total"`
	Passed         int                         `json:"passed"`
	Skipped        int                         `json:"skipped"`
	Failed         int                         `json:"failed"`
	FailedFixtures []ConformanceFixtureSummary `json:"failed_fixtures,omitempty"`
}

type ConformanceFixtureSummary struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Extension string `json:"extension,omitempty"`
	Expected  string `json:"expected,omitempty"`
}

type GovernanceStatus struct {
	Decision         GovernanceDecision      `json:"decision"`
	Policy           PolicySnapshot          `json:"policy"`
	Reviews          []ReviewRecord          `json:"reviews"`
	WarningOverrides []WarningOverrideRecord `json:"warning_overrides"`
	Preflights       []PreflightRecord       `json:"preflights"`
	Benchmarks       []BenchmarkRecord       `json:"benchmarks"`
	Advisories       []AdvisoryRecord        `json:"advisories"`
	Conflicts        ConflictAnalysis        `json:"conflicts"`
}

type ReviewRecord struct {
	ID                int64  `json:"id"`
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	ArtifactHash      string `json:"artifact_hash"`
	Profile           string `json:"profile"`
	RiskLevel         string `json:"risk_level"`
	ConfigHash        string `json:"config_hash"`
	ScopeHash         string `json:"scope_hash"`
	RolloutHash       string `json:"rollout_hash"`
	RuntimeLimitsHash string `json:"runtime_limits_hash"`
	FeaturesHash      string `json:"features_hash"`
	PolicyHash        string `json:"policy_hash"`
	Decision          string `json:"decision"`
	Notes             string `json:"notes"`
	ReviewedBy        string `json:"reviewed_by"`
	CreatedAt         int64  `json:"created_at"`
}

type WarningOverrideRecord struct {
	ID         int64  `json:"id"`
	PluginID   string `json:"plugin_id"`
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
	Action     string `json:"action"`
	PolicyHash string `json:"policy_hash"`
	Reason     string `json:"reason"`
	CreatedBy  string `json:"created_by"`
	ExpiresAt  int64  `json:"expires_at"`
	CreatedAt  int64  `json:"created_at"`
}

type AdvisoryRecord struct {
	ID                int64  `json:"id"`
	AdvisoryID        string `json:"advisory_id"`
	Status            string `json:"status"`
	Action            string `json:"action"`
	ArtifactSHA256    string `json:"artifact_sha256"`
	PluginID          string `json:"plugin_id"`
	VersionRange      string `json:"version_range"`
	DependencyName    string `json:"dependency_name"`
	DependencyRange   string `json:"dependency_range"`
	RecommendedAction string `json:"recommended_action"`
	FixedVersion      string `json:"fixed_version"`
	Mitigation        string `json:"mitigation"`
	CreatedBy         string `json:"created_by"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type PreflightRecord struct {
	ID         int64  `json:"id"`
	PluginID   string `json:"plugin_id"`
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
	Status     string `json:"status"`
	ResultJSON string `json:"result_json"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
}

type BenchmarkRecord struct {
	ID                  int64   `json:"id"`
	PluginID            string  `json:"plugin_id"`
	ArtifactID          string  `json:"artifact_id"`
	Profile             string  `json:"profile"`
	BenchmarkProfile    string  `json:"benchmark_profile"`
	P95MS               float64 `json:"p95_ms"`
	P99MS               float64 `json:"p99_ms"`
	ErrorRate           float64 `json:"error_rate"`
	ActiveProxyCapacity int64   `json:"active_proxy_capacity"`
	BaselineDiff        float64 `json:"baseline_diff"`
	CreatedBy           string  `json:"created_by"`
	CreatedAt           int64   `json:"created_at"`
}

type GovernanceReviewRequest struct {
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
	Decision   string `json:"decision"`
	Notes      string `json:"notes"`
}

type WarningOverrideRequest struct {
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

type PreflightRequest struct {
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
	Action     string `json:"action"`
	ConfigJSON string `json:"config_json"`
}

type SelfTestRequest struct {
	ArtifactID string `json:"artifact_id"`
	Profile    string `json:"profile"`
}

type AdvisoryRequest struct {
	AdvisoryID        string `json:"advisory_id"`
	Status            string `json:"status"`
	Action            string `json:"action"`
	ArtifactSHA256    string `json:"artifact_sha256"`
	PluginID          string `json:"plugin_id"`
	VersionRange      string `json:"version_range"`
	DependencyName    string `json:"dependency_name"`
	DependencyRange   string `json:"dependency_range"`
	RecommendedAction string `json:"recommended_action"`
	FixedVersion      string `json:"fixed_version"`
	Mitigation        string `json:"mitigation"`
}

type AdvisoryFeedRequest struct {
	Source     string            `json:"source"`
	Advisories []AdvisoryRequest `json:"advisories"`
}

type AdvisoryFeedResult struct {
	Source     string               `json:"source"`
	Imported   int                  `json:"imported"`
	Advisories []AdvisoryRecord     `json:"advisories"`
	Rescan     AdvisoryRescanReport `json:"rescan"`
	CreatedBy  string               `json:"created_by"`
	CreatedAt  int64                `json:"created_at"`
}

type AdvisoryRescanReport struct {
	PluginID       string                `json:"plugin_id,omitempty"`
	ArtifactID     string                `json:"artifact_id,omitempty"`
	Scanned        int                   `json:"scanned"`
	Advisories     int                   `json:"advisories"`
	Matches        []AdvisoryRescanMatch `json:"matches"`
	Blocking       int                   `json:"blocking"`
	Warnings       int                   `json:"warnings"`
	QuarantineRuns int                   `json:"quarantine_runs"`
	OK             bool                  `json:"ok"`
}

type AdvisoryRescanMatch struct {
	AdvisoryID     string `json:"advisory_id"`
	Status         string `json:"status"`
	Action         string `json:"action"`
	Severity       string `json:"severity"`
	PluginID       string `json:"plugin_id"`
	ArtifactID     string `json:"artifact_id"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	Version        string `json:"version"`
	RuntimeState   string `json:"runtime_state,omitempty"`
	Active         bool   `json:"active"`
}

type BenchmarkRequest struct {
	ArtifactID          string  `json:"artifact_id"`
	Profile             string  `json:"profile"`
	BenchmarkProfile    string  `json:"benchmark_profile"`
	P95MS               float64 `json:"p95_ms"`
	P99MS               float64 `json:"p99_ms"`
	ErrorRate           float64 `json:"error_rate"`
	ActiveProxyCapacity int64   `json:"active_proxy_capacity"`
	BaselineDiff        float64 `json:"baseline_diff"`
}

type ProxyConnectionSummary struct {
	ID                  uint64 `json:"id"`
	PluginID            string `json:"plugin_id"`
	ArtifactID          string `json:"artifact_id"`
	HandlerID           string `json:"handler_id"`
	StartedAt           int64  `json:"started_at"`
	DurationMS          int64  `json:"duration_ms"`
	Draining            bool   `json:"draining"`
	ForceCloseRequested bool   `json:"force_close_requested"`
}

type OperationRecord struct {
	ID           int64  `json:"id"`
	PluginID     string `json:"plugin_id"`
	ArtifactID   string `json:"artifact_id"`
	Operation    string `json:"operation"`
	Status       string `json:"status"`
	Actor        string `json:"actor"`
	Message      string `json:"message"`
	MetadataJSON string `json:"metadata_json"`
	CreatedAt    int64  `json:"created_at"`
}

type BuildRecord struct {
	ID             int64  `json:"id"`
	PluginID       string `json:"plugin_id"`
	SourceID       string `json:"source_id"`
	ArtifactID     string `json:"artifact_id"`
	Status         string `json:"status"`
	BuilderType    string `json:"builder_type"`
	BuilderImage   string `json:"builder_image"`
	BuilderVersion string `json:"builder_version"`
	GoVersion      string `json:"go_version"`
	GOOS           string `json:"go_os"`
	GOARCH         string `json:"go_arch"`
	GOAMD64        string `json:"go_amd64"`
	GOARM64        string `json:"go_arm64"`
	CGOEnabled     string `json:"cgo_enabled"`
	BuildTags      string `json:"build_tags"`
	SDKModule      string `json:"sdk_module"`
	SDKVersion     string `json:"sdk_version"`
	GOPROXY        string `json:"go_proxy"`
	GONOSUMDB      string `json:"go_no_sumdb"`
	GOPRIVATE      string `json:"go_private"`
	VendorRequired bool   `json:"vendor_required"`
	SourceSHA256   string `json:"source_sha256"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	ModuleSummary  string `json:"module_summary_json"`
	GoVersionM     string `json:"go_version_m_json"`
	ABIFingerprint string `json:"abi_fingerprint"`
	LogSummary     string `json:"log_summary"`
	MetadataJSON   string `json:"metadata_json"`
	Error          string `json:"error"`
	StartedAt      int64  `json:"started_at"`
	EndedAt        int64  `json:"ended_at"`
	DurationMS     int64  `json:"duration_ms"`
	CreatedBy      string `json:"created_by"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

type BuildRequest struct {
	SourceID       string `json:"source_id"`
	BuilderType    string `json:"builder_type"`
	BuilderImage   string `json:"builder_image"`
	BuilderVersion string `json:"builder_version"`
	GOOS           string `json:"go_os"`
	GOARCH         string `json:"go_arch"`
	GOAMD64        string `json:"go_amd64"`
	GOARM64        string `json:"go_arm64"`
	CGOEnabled     string `json:"cgo_enabled"`
	BuildTags      string `json:"build_tags"`
	SDKModule      string `json:"sdk_module"`
	SDKVersion     string `json:"sdk_version"`
	GOPROXY        string `json:"go_proxy"`
	GONOSUMDB      string `json:"go_no_sumdb"`
	GOPRIVATE      string `json:"go_private"`
	VendorRequired bool   `json:"vendor_required"`
}

type RuntimeFeature struct {
	Type              string `json:"type"`
	Implemented       bool   `json:"implemented"`
	Maturity          string `json:"maturity"`
	DataPlane         bool   `json:"data_plane"`
	RequiresRestart   bool   `json:"requires_restart"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
	Entry             string `json:"entry,omitempty"`
}

type PluginServiceModeFeature struct {
	Mode              string `json:"mode"`
	Implemented       bool   `json:"implemented"`
	Maturity          string `json:"maturity"`
	DataPlane         bool   `json:"data_plane"`
	RequiresRestart   bool   `json:"requires_restart"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

type ExtensionPointFeature struct {
	Key               string `json:"key"`
	Type              string `json:"type"`
	Implemented       bool   `json:"implemented"`
	Maturity          string `json:"maturity"`
	DataPlane         bool   `json:"data_plane"`
	RequiresRestart   bool   `json:"requires_restart"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

type PluginServiceState struct {
	DesiredMode        string                `json:"desired_mode"`
	ActiveMode         string                `json:"active_mode"`
	DataPlaneMode      string                `json:"data_plane_mode"`
	ImplementedAdapter bool                  `json:"implemented_adapter"`
	DesiredMaturity    string                `json:"desired_maturity"`
	ActiveMaturity     string                `json:"active_maturity"`
	AppliedAt          int64                 `json:"applied_at"`
	RestartRequired    bool                  `json:"restart_required"`
	LiveMigration      string                `json:"live_migration"`
	CrashPolicy        PluginHostCrashPolicy `json:"crash_policy"`
	UnsupportedReason  string                `json:"unsupported_reason,omitempty"`
	LastError          string                `json:"last_error"`
	UpdatedBy          string                `json:"updated_by"`
	UpdatedAt          int64                 `json:"updated_at"`
}

type PluginHostCrashPolicy struct {
	BackoffSeconds int64 `json:"backoff_seconds"`
	MaxCrashes     int64 `json:"max_crashes"`
	WindowSeconds  int64 `json:"window_seconds"`
}

type PluginServiceStatus struct {
	Service         PluginServiceState            `json:"service"`
	Modes           []PluginServiceModeFeature    `json:"service_modes"`
	RuntimeTypes    []RuntimeFeature              `json:"runtime_types"`
	RuntimeAdapters []RuntimeAdapterFactoryStatus `json:"runtime_adapters"`
	Hosts           []PluginHostRuntimeSummary    `json:"hosts"`
	Nodes           []PluginNodeState             `json:"nodes"`
}

type PluginNodeState struct {
	NodeID        string `json:"node_id"`
	Hostname      string `json:"hostname"`
	PID           int    `json:"pid"`
	ServiceMode   string `json:"service_mode"`
	DataPlaneMode string `json:"data_plane_mode"`
	Status        string `json:"status"`
	StartedAt     int64  `json:"started_at"`
	HeartbeatAt   int64  `json:"heartbeat_at"`
	Stale         bool   `json:"stale"`
}

type PluginNodeRuntimeState struct {
	NodeID            string `json:"node_id"`
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	DesiredState      string `json:"desired_state"`
	RuntimeState      string `json:"runtime_state"`
	DesiredGeneration int64  `json:"desired_generation"`
	AppliedGeneration int64  `json:"applied_generation"`
	Loaded            bool   `json:"loaded"`
	Enabled           bool   `json:"enabled"`
	Health            string `json:"health"`
	Error             string `json:"error,omitempty"`
	UpdatedAt         int64  `json:"updated_at"`
	NodeHeartbeatAt   int64  `json:"node_heartbeat_at,omitempty"`
	Stale             bool   `json:"stale"`
}

type PluginRolloutStatus struct {
	PluginID                   string                   `json:"plugin_id"`
	DesiredState               string                   `json:"desired_state"`
	DesiredArtifactID          string                   `json:"desired_artifact_id"`
	DesiredGeneration          int64                    `json:"desired_generation"`
	OK                         bool                     `json:"ok"`
	PartialFailure             bool                     `json:"partial_failure"`
	NodesTotal                 int                      `json:"nodes_total"`
	NodesReady                 int                      `json:"nodes_ready"`
	NodesFailed                int                      `json:"nodes_failed"`
	NodesStale                 int                      `json:"nodes_stale"`
	NodeRuntimeStates          []PluginNodeRuntimeState `json:"node_runtime_states"`
	ArtifactDistribution       bool                     `json:"artifact_distribution"`
	ArtifactDistributionMode   string                   `json:"artifact_distribution_mode,omitempty"`
	ArtifactDistributionStatus string                   `json:"artifact_distribution_status,omitempty"`
	ArtifactDistributionError  string                   `json:"artifact_distribution_error,omitempty"`
	ArtifactPackageSHA256      string                   `json:"artifact_package_sha256,omitempty"`
	CrossNodeApply             bool                     `json:"cross_node_apply"`
}

type PluginHostRuntimeSummary struct {
	PluginID     string `json:"plugin_id"`
	ArtifactID   string `json:"artifact_id"`
	PID          int    `json:"pid"`
	State        string `json:"state"`
	DrainMode    string `json:"drain_mode"`
	CrashLoop    bool   `json:"crash_loop"`
	CrashCount   int    `json:"crash_count"`
	LastError    string `json:"last_error"`
	StartedAt    int64  `json:"started_at"`
	DrainingAt   int64  `json:"draining_at"`
	ExitedAt     int64  `json:"exited_at"`
	LastCrashAt  int64  `json:"last_crash_at"`
	BackoffUntil int64  `json:"backoff_until"`
	Isolated     bool   `json:"isolated"`
}

type RepositoryImportRequest struct {
	RepositoryType string `json:"repository_type"`
	IndexPath      string `json:"index_path"`
	ArtifactID     string `json:"artifact_id"`
	PluginID       string `json:"plugin_id"`
	Version        string `json:"version"`
	TrustPolicy    string `json:"trust_policy"`
}

type RepositoryImportRecord struct {
	ID             int64  `json:"id"`
	RepositoryType string `json:"repository_type"`
	IndexPath      string `json:"index_path"`
	RepositoryName string `json:"repository_name"`
	CandidateID    string `json:"candidate_id"`
	PluginID       string `json:"plugin_id"`
	Version        string `json:"version"`
	ArtifactID     string `json:"artifact_id"`
	PackageSHA256  string `json:"package_sha256"`
	TrustPolicy    string `json:"trust_policy"`
	AdmissionJSON  string `json:"admission_json"`
	ImportedBy     string `json:"imported_by"`
	CreatedAt      int64  `json:"created_at"`
}

type RepositoryIndexSyncRecord struct {
	ID             int64  `json:"id"`
	RepositoryType string `json:"repository_type"`
	IndexPath      string `json:"index_path"`
	RepositoryName string `json:"repository_name"`
	Status         string `json:"status"`
	CacheKey       string `json:"cache_key"`
	CandidateCount int    `json:"candidate_count"`
	Error          string `json:"error,omitempty"`
	SyncedBy       string `json:"synced_by"`
	CreatedAt      int64  `json:"created_at"`
}

type RepositoryImportApplyResult struct {
	Status  string                  `json:"status"`
	OK      bool                    `json:"ok"`
	DryRun  bool                    `json:"dry_run"`
	Checks  []PromotionCheck        `json:"checks"`
	Import  RepositoryImportRecord  `json:"import"`
	Rollout *PluginRolloutStatus    `json:"rollout,omitempty"`
	Applied *PromotionAppliedPlugin `json:"applied,omitempty"`
}

type RepositoryUpdateReport struct {
	RepositoryType string                      `json:"repository_type"`
	IndexPath      string                      `json:"index_path"`
	RepositoryName string                      `json:"repository_name"`
	CheckedAt      int64                       `json:"checked_at"`
	Sync           RepositoryIndexSyncRecord   `json:"sync,omitempty"`
	Candidates     []RepositoryUpdateCandidate `json:"candidates"`
	Updates        []RepositoryUpdateCandidate `json:"updates"`
}

type RepositoryUpdateCandidate struct {
	CandidateID             string `json:"candidate_id"`
	PluginID                string `json:"plugin_id"`
	AvailableVersion        string `json:"available_version"`
	CandidateSHA256         string `json:"candidate_sha256,omitempty"`
	CurrentVersion          string `json:"current_version,omitempty"`
	CurrentArtifactID       string `json:"current_artifact_id,omitempty"`
	CurrentPackageSHA256    string `json:"current_package_sha256,omitempty"`
	UpdateAvailable         bool   `json:"update_available"`
	Reason                  string `json:"reason"`
	VersionComparison       int    `json:"version_comparison,omitempty"`
	VersionComparisonStable bool   `json:"version_comparison_stable"`
}

type SupplyChainAssessment struct {
	ID         int64             `json:"id"`
	PluginID   string            `json:"plugin_id"`
	ArtifactID string            `json:"artifact_id"`
	Status     string            `json:"status"`
	Issues     []GovernanceIssue `json:"issues"`
	Signature  map[string]any    `json:"signature,omitempty"`
	SBOM       map[string]any    `json:"sbom,omitempty"`
	License    map[string]any    `json:"license,omitempty"`
	Advisory   map[string]any    `json:"advisory,omitempty"`
	Metadata   map[string]any    `json:"metadata,omitempty"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  int64             `json:"created_at"`
}

type TrustRootRecord struct {
	ID               int64  `json:"id"`
	RootID           string `json:"root_id"`
	KeyID            string `json:"key_id"`
	Algorithm        string `json:"algorithm"`
	PublicKey        string `json:"public_key"`
	PublicKeySHA256  string `json:"public_key_sha256"`
	Status           string `json:"status"`
	PolicyJSON       string `json:"policy_json"`
	CreatedBy        string `json:"created_by"`
	RotatedAt        int64  `json:"rotated_at"`
	RevokedAt        int64  `json:"revoked_at,omitempty"`
	RevocationReason string `json:"revocation_reason,omitempty"`
	UpdatedAt        int64  `json:"updated_at"`
}

type TrustRootRequest struct {
	RootID    string         `json:"root_id"`
	KeyID     string         `json:"key_id"`
	Algorithm string         `json:"algorithm"`
	PublicKey string         `json:"public_key"`
	Policy    map[string]any `json:"policy,omitempty"`
}

type TrustRootRevokeRequest struct {
	RootID string `json:"root_id"`
	KeyID  string `json:"key_id"`
	Reason string `json:"reason,omitempty"`
}

type SignatureVerificationRequest struct {
	ArtifactID string `json:"artifact_id"`
	PluginID   string `json:"plugin_id,omitempty"`
	RootID     string `json:"root_id,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
	Signature  string `json:"signature"`
}

type SignatureVerificationResult struct {
	PluginID         string `json:"plugin_id"`
	ArtifactID       string `json:"artifact_id"`
	RootID           string `json:"root_id,omitempty"`
	KeyID            string `json:"key_id,omitempty"`
	Verified         bool   `json:"verified"`
	SignatureValid   bool   `json:"signature_valid"`
	Trusted          bool   `json:"trusted"`
	TrustStatus      string `json:"trust_status"`
	PublicKeySHA256  string `json:"public_key_sha256,omitempty"`
	Revoked          bool   `json:"revoked"`
	RevocationReason string `json:"revocation_reason,omitempty"`
	Error            string `json:"error,omitempty"`
}

type VulnerabilityRequest struct {
	VulnerabilityID string   `json:"vulnerability_id"`
	Source          string   `json:"source,omitempty"`
	Status          string   `json:"status,omitempty"`
	PackageName     string   `json:"package_name"`
	VersionRange    string   `json:"version_range,omitempty"`
	Severity        string   `json:"severity,omitempty"`
	Action          string   `json:"action,omitempty"`
	FixedVersion    string   `json:"fixed_version,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	References      []string `json:"references,omitempty"`
}

type VulnerabilityRecord struct {
	ID              int64    `json:"id"`
	VulnerabilityID string   `json:"vulnerability_id"`
	Source          string   `json:"source,omitempty"`
	Status          string   `json:"status"`
	PackageName     string   `json:"package_name"`
	VersionRange    string   `json:"version_range,omitempty"`
	Severity        string   `json:"severity,omitempty"`
	Action          string   `json:"action"`
	FixedVersion    string   `json:"fixed_version,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	References      []string `json:"references,omitempty"`
	CreatedBy       string   `json:"created_by"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}

type VulnerabilityDBRequest struct {
	Source          string                 `json:"source,omitempty"`
	Vulnerabilities []VulnerabilityRequest `json:"vulnerabilities"`
}

type VulnerabilityDBResult struct {
	Source          string                  `json:"source"`
	Imported        int                     `json:"imported"`
	Vulnerabilities []VulnerabilityRecord   `json:"vulnerabilities"`
	Scan            VulnerabilityScanReport `json:"scan"`
	CreatedBy       string                  `json:"created_by"`
	CreatedAt       int64                   `json:"created_at"`
}

type VulnerabilityScanReport struct {
	PluginID        string                   `json:"plugin_id,omitempty"`
	ArtifactID      string                   `json:"artifact_id,omitempty"`
	Vulnerabilities int                      `json:"vulnerabilities"`
	Scanned         int                      `json:"scanned"`
	Matches         []VulnerabilityScanMatch `json:"matches"`
	Blocking        int                      `json:"blocking"`
	Warnings        int                      `json:"warnings"`
	QuarantineRuns  int                      `json:"quarantine_runs"`
	OK              bool                     `json:"ok"`
}

type VulnerabilityScanMatch struct {
	VulnerabilityID string `json:"vulnerability_id"`
	Source          string `json:"source,omitempty"`
	Status          string `json:"status"`
	PackageName     string `json:"package_name"`
	PackageVersion  string `json:"package_version,omitempty"`
	VersionRange    string `json:"version_range,omitempty"`
	Severity        string `json:"severity,omitempty"`
	Action          string `json:"action"`
	PluginID        string `json:"plugin_id"`
	ArtifactID      string `json:"artifact_id"`
	ArtifactSHA256  string `json:"artifact_sha256"`
	RuntimeState    string `json:"runtime_state,omitempty"`
	Active          bool   `json:"active"`
	FixedVersion    string `json:"fixed_version,omitempty"`
	Summary         string `json:"summary,omitempty"`
}

type InstrumentationRecord struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	Version             string `json:"version"`
	Profile             string `json:"profile"`
	GeneratedDiffHash   string `json:"generated_diff_hash"`
	GatewayBinarySHA256 string `json:"gateway_binary_sha256"`
	CIArtifactSHA256    string `json:"ci_artifact_sha256"`
	ProvenanceJSON      string `json:"provenance_json"`
	ConformanceJSON     string `json:"conformance_json"`
	BenchmarkJSON       string `json:"benchmark_json"`
	SmokeJSON           string `json:"smoke_json"`
	RunbookRollback     string `json:"runbook_rollback"`
	Status              string `json:"status"`
	CreatedBy           string `json:"created_by"`
	CreatedAt           int64  `json:"created_at"`
}

type InstrumentationRequest struct {
	Name                string         `json:"name"`
	Version             string         `json:"version"`
	Profile             string         `json:"profile"`
	GeneratedDiffHash   string         `json:"generated_diff_hash"`
	GatewayBinarySHA256 string         `json:"gateway_binary_sha256"`
	CIArtifactSHA256    string         `json:"ci_artifact_sha256"`
	Provenance          map[string]any `json:"provenance"`
	Conformance         map[string]any `json:"conformance"`
	Benchmark           map[string]any `json:"benchmark"`
	Smoke               map[string]any `json:"smoke"`
	RunbookRollback     string         `json:"runbook_rollback"`
	Status              string         `json:"status"`
}

type PromotionBundle struct {
	SchemaVersion string            `json:"schema_version"`
	APIVersion    string            `json:"api_version"`
	BundleID      string            `json:"bundle_id"`
	Profile       string            `json:"profile"`
	Source        string            `json:"source"`
	Plugins       []PromotionPlugin `json:"plugins"`
	CreatedAt     int64             `json:"created_at"`
}

type PromotionPlugin struct {
	PluginID          string            `json:"plugin_id"`
	Version           string            `json:"version"`
	ArtifactType      string            `json:"artifact_type"`
	RuntimeType       string            `json:"runtime_type"`
	APIVersion        string            `json:"api_version"`
	ArtifactSHA256    string            `json:"artifact_sha256"`
	PackageSHA256     string            `json:"package_sha256"`
	DesiredState      string            `json:"desired_state"`
	ConfigHash        string            `json:"config_hash"`
	ScopeHash         string            `json:"scope_hash"`
	RolloutHash       string            `json:"rollout_hash"`
	RuntimeLimitsHash string            `json:"runtime_limits_hash"`
	FeaturesHash      string            `json:"features_hash"`
	SecretRefs        []string          `json:"secret_refs,omitempty"`
	Environment       string            `json:"environment,omitempty"`
	Overrides         map[string]any    `json:"overrides,omitempty"`
	SecretMapping     map[string]string `json:"secret_mapping,omitempty"`
	Provenance        map[string]any    `json:"provenance,omitempty"`
}

type PromotionCheck struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	Message  string         `json:"message"`
	PluginID string         `json:"plugin_id,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

type PromotionReport struct {
	Status string           `json:"status"`
	OK     bool             `json:"ok"`
	Checks []PromotionCheck `json:"checks"`
	Bundle PromotionBundle  `json:"bundle"`
}

type PromotionApplyResult struct {
	Status  string                   `json:"status"`
	OK      bool                     `json:"ok"`
	DryRun  bool                     `json:"dry_run"`
	Checks  []PromotionCheck         `json:"checks"`
	Bundle  PromotionBundle          `json:"bundle"`
	Rollout []PluginRolloutStatus    `json:"rollout,omitempty"`
	Applied []PromotionAppliedPlugin `json:"applied,omitempty"`
}

type PromotionAppliedPlugin struct {
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	DesiredState      string `json:"desired_state"`
	DesiredGeneration int64  `json:"desired_generation"`
	Priority          int    `json:"priority"`
}

type PromotionDiff struct {
	PluginID string `json:"plugin_id"`
	Field    string `json:"field"`
	Current  string `json:"current"`
	Target   string `json:"target"`
	Reason   string `json:"reason,omitempty"`
}

type PromotionDriftReport struct {
	Status string          `json:"status"`
	OK     bool            `json:"ok"`
	Diff   []PromotionDiff `json:"diff"`
}

type PromotionDRDrillReport struct {
	Status string           `json:"status"`
	OK     bool             `json:"ok"`
	Checks []PromotionCheck `json:"checks"`
}

type GCCandidate struct {
	Kind             string `json:"kind"`
	Category         string `json:"category,omitempty"`
	ID               string `json:"id"`
	PluginID         string `json:"plugin_id"`
	Path             string `json:"path"`
	Protected        bool   `json:"protected"`
	Reason           string `json:"reason"`
	RetentionRule    string `json:"retention_rule,omitempty"`
	RetentionSeconds int64  `json:"retention_seconds,omitempty"`
	SizeBytes        int64  `json:"size_bytes"`
	CreatedAt        int64  `json:"created_at"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	Referenced       bool   `json:"referenced"`
}

type ConfigSnapshot struct {
	ID                int64  `json:"id"`
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	ConfigJSON        string `json:"config_json"`
	DesiredState      string `json:"desired_state"`
	Priority          int    `json:"priority"`
	DesiredGeneration int64  `json:"desired_generation"`
	CreatedBy         string `json:"created_by"`
	CreatedAt         int64  `json:"created_at"`
}

type DispatchPlan struct {
	Handlers    []DispatchHandlerSummary `json:"handlers"`
	Routes      []DispatchHandlerSummary `json:"routes"`
	Rules       []DispatchHandlerSummary `json:"rules"`
	Statuses    []DispatchHandlerSummary `json:"statuses"`
	Middleware  []DispatchHandlerSummary `json:"middleware"`
	Subscribers []DispatchHandlerSummary `json:"subscribers"`
	Providers   []ProviderSummary        `json:"providers"`
	RouteCache  []RouteDecisionSummary   `json:"route_cache"`
	UpdatedAt   int64                    `json:"updated_at"`
}

type DispatchHandlerSummary struct {
	PluginID         string `json:"plugin_id"`
	ArtifactID       string `json:"artifact_id"`
	Priority         int    `json:"priority"`
	HandlerID        string `json:"handler_id"`
	ExtensionPoint   string `json:"extension_point"`
	Mode             string `json:"mode"`
	TimeoutMS        int64  `json:"timeout_ms"`
	Calls            uint64 `json:"calls"`
	Errors           uint64 `json:"errors"`
	Panics           uint64 `json:"panics"`
	Timeouts         uint64 `json:"timeouts"`
	Blocked          uint64 `json:"blocked"`
	ActiveProxy      int64  `json:"active_proxy_connections"`
	DrainingProxy    int64  `json:"draining_proxy_connections"`
	ProxyStarted     uint64 `json:"proxy_connections_started"`
	ProxyCompleted   uint64 `json:"proxy_connections_completed"`
	ProxyForceClosed uint64 `json:"proxy_connections_force_closed"`
	ProxyErrors      uint64 `json:"proxy_errors"`
	LastProxyError   string `json:"last_proxy_error,omitempty"`
	ProxyBytesIn     uint64 `json:"proxy_bytes_in"`
	ProxyBytesOut    uint64 `json:"proxy_bytes_out"`
	ProxyDurationMS  uint64 `json:"proxy_duration_ms"`
	DurationCount    uint64 `json:"duration_count"`
	DurationSumMS    uint64 `json:"duration_sum_ms"`
	DurationMaxMS    uint64 `json:"duration_max_ms"`
}

type OperationsSnapshot struct {
	PluginID             string                      `json:"plugin_id,omitempty"`
	UpdatedAt            int64                       `json:"updated_at"`
	Exporters            []OperationsExporterStatus  `json:"exporters,omitempty"`
	Handlers             []DispatchHandlerSummary    `json:"handlers"`
	Builds               []BuildMetricSummary        `json:"builds"`
	Events               []EventSummary              `json:"events"`
	CustomMetrics        []CustomMetricSummary       `json:"custom_metrics"`
	Logs                 []LogSummary                `json:"logs"`
	Traces               []TraceSummary              `json:"traces"`
	BackgroundTasks      []BackgroundTaskSummary     `json:"background_tasks"`
	PluginData           []PluginDataSummary         `json:"plugin_data"`
	PluginFiles          []PluginFileSummary         `json:"plugin_files"`
	ExternalDependencies []ExternalDependencySummary `json:"external_dependencies"`
	GC                   []GCCandidate               `json:"gc,omitempty"`
	EventQueue           EventQueueSummary           `json:"event_queue"`
	Diagnostics          []DiagnosticPackageSummary  `json:"diagnostics,omitempty"`
}

type BuildMetricSummary struct {
	PluginID      string `json:"plugin_id"`
	BuildID       int64  `json:"build_id"`
	Status        string `json:"status"`
	DurationMS    int64  `json:"duration_ms"`
	Failed        bool   `json:"failed"`
	ErrorRedacted string `json:"error_redacted,omitempty"`
	CreatedAt     int64  `json:"created_at"`
}

type EventSummary struct {
	PluginID    string            `json:"plugin_id"`
	Name        string            `json:"name"`
	Count       uint64            `json:"count"`
	Dropped     uint64            `json:"dropped"`
	DeadLetters uint64            `json:"dead_letters"`
	Fields      map[string]string `json:"fields,omitempty"`
	LastSeenAt  int64             `json:"last_seen_at"`
}

type SubscriberDeadLetterRecord struct {
	ID                   int64             `json:"id"`
	SubscriberPluginID   string            `json:"subscriber_plugin_id"`
	SubscriberArtifactID string            `json:"subscriber_artifact_id,omitempty"`
	EventPluginID        string            `json:"event_plugin_id"`
	EventName            string            `json:"event_name"`
	Fields               map[string]string `json:"fields,omitempty"`
	TraceID              string            `json:"trace_id,omitempty"`
	ConnectionID         string            `json:"connection_id,omitempty"`
	DeliveryMode         string            `json:"delivery_mode,omitempty"`
	Attempts             int               `json:"attempts,omitempty"`
	NodeID               string            `json:"node_id,omitempty"`
	Status               string            `json:"status"`
	Reason               string            `json:"reason,omitempty"`
	CreatedAt            int64             `json:"created_at"`
	UpdatedAt            int64             `json:"updated_at"`
}

type EventQueueSummary struct {
	Limit                 int    `json:"limit"`
	Queued                int    `json:"queued"`
	Dropped               uint64 `json:"dropped"`
	DeadLetters           uint64 `json:"dead_letters"`
	SubscriberQueued      uint64 `json:"subscriber_queued"`
	SubscriberDropped     uint64 `json:"subscriber_dropped"`
	SubscriberDeadLetters uint64 `json:"subscriber_dead_letters"`
}

type RouteDecisionSummary struct {
	Host       string            `json:"host"`
	Action     string            `json:"action"`
	Upstream   string            `json:"upstream,omitempty"`
	ProviderID string            `json:"provider_id,omitempty"`
	Source     string            `json:"source"`
	Reason     string            `json:"reason,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  int64             `json:"created_at"`
	ExpiresAt  int64             `json:"expires_at,omitempty"`
}

type ProviderSummary struct {
	PluginID       string            `json:"plugin_id"`
	ArtifactID     string            `json:"artifact_id"`
	ExtensionPoint string            `json:"extension_point"`
	Type           string            `json:"type"`
	Name           string            `json:"name"`
	Priority       int               `json:"priority"`
	Fallback       bool              `json:"fallback"`
	Dependencies   []string          `json:"dependencies,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Status         string            `json:"status"`
	Error          string            `json:"error,omitempty"`
}

type CustomMetricSummary struct {
	PluginID   string            `json:"plugin_id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Count      uint64            `json:"count"`
	LastValue  float64           `json:"last_value"`
	Labels     map[string]string `json:"labels,omitempty"`
	LastSeenAt int64             `json:"last_seen_at"`
}

type LogSummary struct {
	PluginID     string            `json:"plugin_id"`
	Level        string            `json:"level"`
	Message      string            `json:"message"`
	Fields       map[string]string `json:"fields,omitempty"`
	TraceID      string            `json:"trace_id,omitempty"`
	ConnectionID string            `json:"connection_id,omitempty"`
	CreatedAt    int64             `json:"created_at"`
}

type TraceSummary struct {
	PluginID     string `json:"plugin_id"`
	TraceID      string `json:"trace_id"`
	ConnectionID string `json:"connection_id"`
	HandlerID    string `json:"handler_id,omitempty"`
	Operation    string `json:"operation"`
	Status       string `json:"status"`
	DurationMS   int64  `json:"duration_ms"`
	CreatedAt    int64  `json:"created_at"`
}

type BackgroundTaskSummary struct {
	PluginID            string `json:"plugin_id"`
	TaskID              string `json:"task_id"`
	Name                string `json:"name"`
	Mode                string `json:"mode"`
	RunPolicy           string `json:"run_policy"`
	NodeID              string `json:"node_id,omitempty"`
	ShardKey            string `json:"shard_key,omitempty"`
	IntervalMS          int64  `json:"interval_ms"`
	RunOnStart          bool   `json:"run_on_start"`
	TimeoutMS           int64  `json:"timeout_ms"`
	Manual              bool   `json:"manual"`
	Running             bool   `json:"running"`
	LastRunAt           int64  `json:"last_run_at"`
	NextRunAt           int64  `json:"next_run_at"`
	LastDurationMS      int64  `json:"last_duration_ms"`
	LastError           string `json:"last_error,omitempty"`
	Skipped             uint64 `json:"skipped"`
	Retry               int    `json:"retry"`
	LastAttempts        int    `json:"last_attempts"`
	LeaseRequired       bool   `json:"lease_required"`
	LeaseAcquired       bool   `json:"lease_acquired"`
	LeaseOwner          string `json:"lease_owner,omitempty"`
	LeaseExpiresAt      int64  `json:"lease_expires_at,omitempty"`
	LeaseSkipped        uint64 `json:"lease_skipped"`
	ConsecutiveFailures uint64 `json:"consecutive_failures"`
}

type TaskLeaseRecord struct {
	PluginID    string `json:"plugin_id"`
	TaskID      string `json:"task_id"`
	ShardKey    string `json:"shard_key"`
	OwnerNodeID string `json:"owner_node_id"`
	ExpiresAt   int64  `json:"expires_at"`
	AcquiredAt  int64  `json:"acquired_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type PluginDataSummary struct {
	PluginID      string `json:"plugin_id"`
	Key           string `json:"key"`
	SchemaVersion int    `json:"schema_version"`
	DataClass     string `json:"data_class"`
	Exportable    bool   `json:"exportable"`
	SizeBytes     int64  `json:"size_bytes"`
	ExpiresAt     int64  `json:"expires_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type PluginDataRecord struct {
	PluginDataSummary
	Value []byte `json:"-"`
}

type PluginFileSummary struct {
	PluginID  string `json:"plugin_id"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
	DataClass string `json:"data_class"`
	SizeBytes int64  `json:"size_bytes"`
	ExpiresAt int64  `json:"expires_at"`
	UpdatedAt int64  `json:"updated_at"`
	Orphaned  bool   `json:"orphaned"`
	Readonly  bool   `json:"readonly"`
}

type PluginFileRecord struct {
	PluginFileSummary
}

type ExternalDependencySummary struct {
	PluginID            string   `json:"plugin_id"`
	Name                string   `json:"name"`
	Endpoint            string   `json:"endpoint"`
	Purpose             string   `json:"purpose"`
	Required            bool     `json:"required"`
	FailPolicy          string   `json:"fail_policy"`
	DataClasses         []string `json:"data_classes,omitempty"`
	Requests            uint64   `json:"requests"`
	Errors              uint64   `json:"errors"`
	Inflight            int64    `json:"inflight"`
	DurationCount       uint64   `json:"duration_count"`
	DurationSumMS       uint64   `json:"duration_sum_ms"`
	CircuitState        string   `json:"circuit_state"`
	ConsecutiveFailures uint64   `json:"consecutive_failures"`
	RecentError         string   `json:"recent_error,omitempty"`
	LastStatus          string   `json:"last_status,omitempty"`
	LastSeenAt          int64    `json:"last_seen_at"`
	NetworkBoundary     string   `json:"network_boundary"`
	NetworkEnforced     bool     `json:"network_enforced"`
}

type ExternalDependencyHealthCheck struct {
	PluginID  string                    `json:"plugin_id"`
	Name      string                    `json:"name"`
	OK        bool                      `json:"ok"`
	Error     string                    `json:"error,omitempty"`
	Summary   ExternalDependencySummary `json:"summary"`
	CheckedBy string                    `json:"checked_by"`
	CheckedAt int64                     `json:"checked_at"`
}

type DiagnosticPackageSummary struct {
	PluginID  string   `json:"plugin_id"`
	CreatedAt int64    `json:"created_at"`
	SizeBytes int64    `json:"size_bytes"`
	Sections  []string `json:"sections"`
}

type OperationsExporterConfig struct {
	Type     string `json:"type"`
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint,omitempty"`
}

type OperationsExporterStatus struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Enabled            bool   `json:"enabled"`
	Endpoint           string `json:"endpoint,omitempty"`
	Degraded           bool   `json:"degraded"`
	FailOpen           bool   `json:"fail_open"`
	LowCardinalityGate bool   `json:"low_cardinality_gate"`
	SensitiveFieldGate bool   `json:"sensitive_field_gate"`
	LastError          string `json:"last_error,omitempty"`
	FailureCount       uint64 `json:"failure_count"`
	LastFailureAt      int64  `json:"last_failure_at,omitempty"`
	LastSuccessAt      int64  `json:"last_success_at,omitempty"`
	RollbackCount      uint64 `json:"rollback_count"`
	LastRollbackAt     int64  `json:"last_rollback_at,omitempty"`
	Boundary           string `json:"boundary"`
	UnsupportedReason  string `json:"unsupported_reason,omitempty"`
}

type OperationsExportSample struct {
	PluginID  string            `json:"plugin_id"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Value     float64           `json:"value,omitempty"`
	CreatedAt int64             `json:"created_at"`
}

type OperationsExportBatch struct {
	ExporterType string                   `json:"exporter_type"`
	Samples      []OperationsExportSample `json:"samples"`
}

type OperationsExporterSink interface {
	ExportOperations(context.Context, OperationsExportBatch) error
}

type UpstreamResult struct {
	Conn            net.Conn
	Handled         bool
	Mode            string
	PluginID        string
	HandlerID       string
	InitialDataSent bool
	Proxied         bool
}

type Gateway struct {
	PluginID   string
	handleConn func(net.Conn)
	wg         *sync.WaitGroup
	hooks      map[string]any
	ops        *PluginOperations
}

func NewGateway(pluginID string, handleConn func(net.Conn), wg *sync.WaitGroup, ops *PluginOperations) *Gateway {
	return &Gateway{
		PluginID:   pluginID,
		handleConn: handleConn,
		wg:         wg,
		hooks:      make(map[string]any),
		ops:        ops,
	}
}

func (g *Gateway) RegisteredHooks() map[string]any {
	copied := make(map[string]any, len(g.hooks))
	for key, value := range g.hooks {
		copied[key] = value
	}
	return copied
}

func (g *Gateway) Hook(hook string, handler any) error {
	g.hooks[hook] = handler
	return nil
}

func (g *Gateway) HandleConn(conn net.Conn) {
	if g.handleConn != nil {
		g.handleConn(conn)
	}
}

func (g *Gateway) ExitWaitGroup() *sync.WaitGroup {
	if g.wg == nil {
		g.wg = &sync.WaitGroup{}
	}
	return g.wg
}

func (g *Gateway) EmitEvent(ctx context.Context, name string, fields map[string]string) error {
	if g.ops == nil {
		return nil
	}
	return g.ops.EmitEvent(ctx, name, fields)
}

func (g *Gateway) ObserveMetric(ctx context.Context, name string, value float64, labels map[string]string) error {
	if g.ops == nil {
		return nil
	}
	return g.ops.ObserveMetric(ctx, name, value, labels)
}

func (g *Gateway) Logger() api.Logger {
	if g.ops == nil {
		return noopOperationsLogger{}
	}
	return g.ops.Logger()
}

func (g *Gateway) DataStore() api.DataStore {
	if g.ops == nil {
		return noopOperationsDataStore{}
	}
	return g.ops.DataStore()
}

func (g *Gateway) FileStore() api.FileStore {
	if g.ops == nil {
		return noopOperationsFileStore{}
	}
	return g.ops.FileStore()
}

func (g *Gateway) ExternalClient(name string) api.ExternalClient {
	if g.ops == nil {
		return noopOperationsExternalClient{}
	}
	return g.ops.ExternalClient(name)
}

func (g *Gateway) RegisterBackgroundTask(task api.BackgroundTask) error {
	if g.ops == nil {
		return nil
	}
	return g.ops.RegisterBackgroundTask(task)
}

func (g *Gateway) LegacyUpstreamHandler() (api.HookHandler[func(net.Conn, string) bool, func(net.Conn, string) (net.Conn, error)], bool) {
	handler, ok := g.hooks[api.HookUpstream.Key()].(api.HookHandler[func(net.Conn, string) bool, func(net.Conn, string) (net.Conn, error)])
	return handler, ok
}

func (g *Gateway) UpstreamConnectHandler() (api.HookHandler[api.UpstreamConnectAcceptor, api.UpstreamConnectHandler], bool) {
	handler, ok := g.hooks[api.HookUpstreamConnect.Key()].(api.HookHandler[api.UpstreamConnectAcceptor, api.UpstreamConnectHandler])
	return handler, ok
}

func (g *Gateway) RouteResolveHandler() (api.HookHandler[api.RouteResolveAcceptor, api.RouteResolveHandler], bool) {
	handler, ok := g.hooks[api.HookRouteResolve.Key()].(api.HookHandler[api.RouteResolveAcceptor, api.RouteResolveHandler])
	return handler, ok
}

func (g *Gateway) RouteResolverHandler() (api.HookHandler[api.RouteResolveAcceptor, api.RouteResolveHandler], bool) {
	handler, ok := g.hooks[api.HookRouteResolver.Key()].(api.HookHandler[api.RouteResolveAcceptor, api.RouteResolveHandler])
	return handler, ok
}

func (g *Gateway) RuleEvaluateHandler() (api.HookHandler[api.RuleEvaluateAcceptor, api.RuleEvaluateHandler], bool) {
	handler, ok := g.hooks[api.HookRuleEvaluate.Key()].(api.HookHandler[api.RuleEvaluateAcceptor, api.RuleEvaluateHandler])
	return handler, ok
}

func (g *Gateway) StatusPingHandler() (api.HookHandler[api.StatusPingAcceptor, api.StatusPingHandler], bool) {
	handler, ok := g.hooks[api.HookStatusPing.Key()].(api.HookHandler[api.StatusPingAcceptor, api.StatusPingHandler])
	return handler, ok
}

func (g *Gateway) ConnectionFilterHandler() (api.HookHandler[api.ConnectionFilterAcceptor, api.ConnectionFilterHandler], bool) {
	handler, ok := g.hooks[api.HookConnectionFilter.Key()].(api.HookHandler[api.ConnectionFilterAcceptor, api.ConnectionFilterHandler])
	return handler, ok
}

func (g *Gateway) HandshakeFilterHandler() (api.HookHandler[api.HandshakeFilterAcceptor, api.HandshakeFilterHandler], bool) {
	handler, ok := g.hooks[api.HookHandshakeFilter.Key()].(api.HookHandler[api.HandshakeFilterAcceptor, api.HandshakeFilterHandler])
	return handler, ok
}

func (g *Gateway) EventSubscriberHandler() (api.HookHandler[api.EventSubscriberAcceptor, api.EventSubscriberHandler], bool) {
	handler, ok := g.hooks[api.HookEventSubscriber.Key()].(api.HookHandler[api.EventSubscriberAcceptor, api.EventSubscriberHandler])
	return handler, ok
}

func (g *Gateway) ProviderHandler() (api.HookHandler[api.ProviderAcceptor, api.ProviderHandler], bool) {
	handler, ok := g.hooks[api.HookProvider.Key()].(api.HookHandler[api.ProviderAcceptor, api.ProviderHandler])
	return handler, ok
}

func (g *Gateway) AuthProviderHandler() (api.HookHandler[api.ProviderAcceptor, api.ProviderHandler], bool) {
	handler, ok := g.hooks[api.HookAuthProvider.Key()].(api.HookHandler[api.ProviderAcceptor, api.ProviderHandler])
	return handler, ok
}

func (g *Gateway) AdminAuthProviderHandler() (api.HookHandler[api.ProviderAcceptor, api.ProviderHandler], bool) {
	handler, ok := g.hooks[api.HookAdminAuthProvider.Key()].(api.HookHandler[api.ProviderAcceptor, api.ProviderHandler])
	return handler, ok
}
