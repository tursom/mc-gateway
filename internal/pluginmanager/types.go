package pluginmanager

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	SchemaVersion = "mc-gateway.plugin/v1"
	APIVersion    = "plugin-api/v1"

	ArtifactTypeBinary = "binary"
	ArtifactTypeSource = "source"
	RuntimeGoPlugin    = "go-plugin"
	RuntimeEntry       = "plugin.so"
	SourceBuildEntry   = "."

	ExtensionUpstreamConnect = "upstream.connect/v1"

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

	DefaultPriority            = 100
	DefaultHandlerTimeout      = 3 * time.Second
	DefaultManifestMaxBytes    = 256 * 1024
	DefaultPackageMaxBytes     = 64 * 1024 * 1024
	DefaultPackageMaxEntries   = 2048
	DefaultExtractedMaxBytes   = 256 * 1024 * 1024
	DefaultNonRuntimeMaxBytes  = 16 * 1024 * 1024
	DefaultInitialWriteTimeout = time.Second
	DefaultBuildLogMaxBytes    = 64 * 1024
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
	SupplyChain      json.RawMessage  `json:"supply_chain"`
}

type RuntimeManifest struct {
	Type           string `json:"type"`
	Entry          string `json:"entry"`
	BuildEntry     string `json:"build_entry"`
	EntrySymbol    string `json:"entry_symbol"`
	MetadataSymbol string `json:"metadata_symbol"`
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

type CapabilitySummary struct {
	UpstreamConnect UpstreamConnectCapability `json:"upstream_connect,omitempty"`
	Minecraft       *MinecraftCapability      `json:"minecraft,omitempty"`
	Raw             json.RawMessage           `json:"raw,omitempty"`
}

type UpstreamConnectCapability struct {
	Mode string `json:"mode,omitempty"`
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

type ProxyConnectionSummary struct {
	ID         uint64 `json:"id"`
	PluginID   string `json:"plugin_id"`
	ArtifactID string `json:"artifact_id"`
	HandlerID  string `json:"handler_id"`
	StartedAt  int64  `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	Draining   bool   `json:"draining"`
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

type GCCandidate struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	PluginID   string `json:"plugin_id"`
	Path       string `json:"path"`
	Protected  bool   `json:"protected"`
	Reason     string `json:"reason"`
	SizeBytes  int64  `json:"size_bytes"`
	CreatedAt  int64  `json:"created_at"`
	Referenced bool   `json:"referenced"`
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
	Handlers  []DispatchHandlerSummary `json:"handlers"`
	UpdatedAt int64                    `json:"updated_at"`
}

type DispatchHandlerSummary struct {
	PluginID        string `json:"plugin_id"`
	ArtifactID      string `json:"artifact_id"`
	Priority        int    `json:"priority"`
	HandlerID       string `json:"handler_id"`
	ExtensionPoint  string `json:"extension_point"`
	Mode            string `json:"mode"`
	TimeoutMS       int64  `json:"timeout_ms"`
	Calls           uint64 `json:"calls"`
	Errors          uint64 `json:"errors"`
	Panics          uint64 `json:"panics"`
	Timeouts        uint64 `json:"timeouts"`
	Blocked         uint64 `json:"blocked"`
	ActiveProxy     int64  `json:"active_proxy_connections"`
	ProxyStarted    uint64 `json:"proxy_connections_started"`
	ProxyCompleted  uint64 `json:"proxy_connections_completed"`
	ProxyErrors     uint64 `json:"proxy_errors"`
	ProxyBytesIn    uint64 `json:"proxy_bytes_in"`
	ProxyBytesOut   uint64 `json:"proxy_bytes_out"`
	ProxyDurationMS uint64 `json:"proxy_duration_ms"`
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
}

func NewGateway(pluginID string, handleConn func(net.Conn), wg *sync.WaitGroup) *Gateway {
	return &Gateway{
		PluginID:   pluginID,
		handleConn: handleConn,
		wg:         wg,
		hooks:      make(map[string]any),
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

func (g *Gateway) LegacyUpstreamHandler() (api.HookHandler[func(net.Conn, string) bool, func(net.Conn, string) (net.Conn, error)], bool) {
	handler, ok := g.hooks[api.HookUpstream.Key()].(api.HookHandler[func(net.Conn, string) bool, func(net.Conn, string) (net.Conn, error)])
	return handler, ok
}

func (g *Gateway) UpstreamConnectHandler() (api.HookHandler[api.UpstreamConnectAcceptor, api.UpstreamConnectHandler], bool) {
	handler, ok := g.hooks[api.HookUpstreamConnect.Key()].(api.HookHandler[api.UpstreamConnectAcceptor, api.UpstreamConnectHandler])
	return handler, ok
}
