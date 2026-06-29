// internal/adminhttp/requests.go 定义 Admin API 测试网关和命令处理器接受的请求载荷。

package adminhttp

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type SetupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type RouteRequest struct {
	Upstream string `json:"upstream"`
	Enabled  *bool  `json:"enabled"`
	Note     string `json:"note"`
}

type ServiceRequest struct {
	Enabled *bool          `json:"enabled"`
	Port    int            `json:"port"`
	Options map[string]any `json:"options"`
}

type CreateUserRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Password string `json:"password"`
	Disabled bool   `json:"disabled"`
}

type PatchUserRequest struct {
	Role     *string `json:"role"`
	Password *string `json:"password"`
	Disabled *bool   `json:"disabled"`
}

type PluginDesiredRequest struct {
	ArtifactID   string         `json:"artifact_id"`
	DesiredState string         `json:"desired_state"`
	Priority     int            `json:"priority"`
	Config       map[string]any `json:"config"`
	ConfigJSON   string         `json:"config_json"`
}

type PluginConfigRequest struct {
	ArtifactID   string         `json:"artifact_id"`
	DesiredState string         `json:"desired_state"`
	Priority     int            `json:"priority"`
	Config       map[string]any `json:"config"`
	ConfigJSON   string         `json:"config_json"`
}

type PluginSecretRequest struct {
	ArtifactID     string `json:"artifact_id"`
	Name           string `json:"name"`
	Value          string `json:"value"`
	ReloadRequired bool   `json:"reload_required"`
	HotReload      bool   `json:"hot_reload"`
}

type PluginRollbackRequest struct {
	ArtifactID  string `json:"artifact_id"`
	SnapshotID  int64  `json:"snapshot_id"`
	FullDesired bool   `json:"full_desired"`
}

type PluginServiceRequest struct {
	DesiredMode string                           `json:"desired_mode"`
	CrashPolicy *PluginServiceCrashPolicyRequest `json:"crash_policy,omitempty"`
}

type PluginServiceCrashPolicyRequest struct {
	BackoffSeconds *int64 `json:"backoff_seconds,omitempty"`
	MaxCrashes     *int64 `json:"max_crashes,omitempty"`
	WindowSeconds  *int64 `json:"window_seconds,omitempty"`
}

type PluginRepositoryImportRequest struct {
	RepositoryType string         `json:"repository_type"`
	IndexPath      string         `json:"index_path"`
	ArtifactID     string         `json:"artifact_id"`
	PluginID       string         `json:"plugin_id"`
	Version        string         `json:"version"`
	TrustPolicy    string         `json:"trust_policy"`
	Action         string         `json:"action"`
	ImportID       int64          `json:"import_id"`
	DesiredState   string         `json:"desired_state"`
	Priority       int            `json:"priority"`
	Config         map[string]any `json:"config"`
	ConfigJSON     string         `json:"config_json"`
	DryRun         bool           `json:"dry_run"`
}

type PluginSupplyChainRequest struct {
	PluginID   string         `json:"plugin_id"`
	ArtifactID string         `json:"artifact_id"`
	Metadata   map[string]any `json:"metadata"`
}
