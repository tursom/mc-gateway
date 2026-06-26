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
	RuntimeGoPlugin    = "go-plugin"
	RuntimeEntry       = "plugin.so"

	ExtensionUpstreamConnect = "upstream.connect/v1"

	ArtifactStatusUploaded  = "uploaded"
	ArtifactStatusValidated = "validated"
	ArtifactStatusLoadable  = "loadable"
	ArtifactStatusLoaded    = "loaded"
	ArtifactStatusRejected  = "rejected"
	ArtifactStatusDeleted   = "deleted"

	DesiredEnabled  = "enabled"
	DesiredDisabled = "disabled"
	DesiredDeleted  = "deleted"

	RuntimeNotLoaded = "not_loaded"
	RuntimeLoaded    = "loaded"
	RuntimeEnabled   = "enabled"
	RuntimeFailed    = "failed"
	RuntimeDisabled  = "disabled"

	DefaultPriority           = 100
	DefaultHandlerTimeout     = 3 * time.Second
	DefaultManifestMaxBytes   = 256 * 1024
	DefaultPackageMaxBytes    = 64 * 1024 * 1024
	DefaultPackageMaxEntries  = 2048
	DefaultExtractedMaxBytes  = 256 * 1024 * 1024
	DefaultNonRuntimeMaxBytes = 16 * 1024 * 1024
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
	SupplyChain      json.RawMessage  `json:"supply_chain"`
}

type RuntimeManifest struct {
	Type           string `json:"type"`
	Entry          string `json:"entry"`
	EntrySymbol    string `json:"entry_symbol"`
	MetadataSymbol string `json:"metadata_symbol"`
}

type ExtensionPoint struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

type RuntimeLimits struct {
	HandlerTimeoutMS int `json:"handler_timeout_ms"`
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
	PluginID       string `json:"plugin_id"`
	ArtifactID     string `json:"artifact_id"`
	Priority       int    `json:"priority"`
	HandlerID      string `json:"handler_id"`
	ExtensionPoint string `json:"extension_point"`
	TimeoutMS      int64  `json:"timeout_ms"`
	Calls          uint64 `json:"calls"`
	Errors         uint64 `json:"errors"`
	Panics         uint64 `json:"panics"`
	Timeouts       uint64 `json:"timeouts"`
}

type UpstreamResult struct {
	Conn    net.Conn
	Handled bool
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
