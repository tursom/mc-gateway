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
