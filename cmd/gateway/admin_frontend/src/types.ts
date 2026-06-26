export type Role = "admin" | "member" | "guest";

export interface User {
  username: string;
  role: Role;
  disabled: boolean;
  permissions?: Record<string, boolean>;
}

export interface RouteRecord {
  host: string;
  upstream: string;
  enabled: boolean;
  note?: string;
}

export interface ServiceRecord {
  name: string;
  enabled: boolean;
  port: number;
  options?: Record<string, unknown>;
  restart_required?: boolean;
}

export interface Metrics {
  total_connections?: number;
  active_connections?: number;
  tcp_connections?: number;
  websocket_connections?: number;
  route_misses?: number;
  upstream_dial_errors?: number;
  route_hits?: Record<string, number>;
}

export interface AuditLog {
  created_at: number;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  success: boolean;
  message?: string;
}

export interface PluginArtifact {
  id: string;
  plugin_id: string;
  version: string;
  file_name?: string;
  sha256: string;
  package_sha256?: string;
  artifact_type: string;
  runtime_type: string;
  status: string;
  metadata_json?: string;
  capabilities_summary_json?: string;
  extension_points_json?: string;
  api_version?: string;
  go_version?: string;
  go_os?: string;
  go_arch?: string;
  uploaded_by?: string;
  error?: string;
  created_at?: number;
  updated_at?: number;
}

export interface PluginBuild {
  id: number;
  plugin_id: string;
  source_id: string;
  artifact_id: string;
  status: string;
  builder_type: string;
  go_version?: string;
  go_os?: string;
  go_arch?: string;
  source_sha256?: string;
  artifact_sha256?: string;
  log_summary?: string;
  error?: string;
  duration_ms?: number;
  created_at?: number;
  updated_at?: number;
}

export interface PluginSecret {
  plugin_id: string;
  name: string;
  current_version: number;
  previous_version: number;
  reload_required: boolean;
  hot_reload: boolean;
  updated_by?: string;
  updated_at?: number;
}

export interface PluginSnapshot {
  id: number;
  plugin_id: string;
  artifact_id: string;
  config_json: string;
  desired_state: string;
  priority: number;
  desired_generation: number;
  created_by?: string;
  created_at?: number;
}

export interface PluginProxyConnection {
  id: number;
  plugin_id: string;
  artifact_id: string;
  handler_id: string;
  started_at: number;
  duration_ms: number;
  draining: boolean;
}

export interface PluginServiceState {
  desired_mode: string;
  active_mode: string;
  applied_at?: number;
  restart_required: boolean;
  live_migration?: string;
  last_error?: string;
  updated_by?: string;
  updated_at?: number;
}

export interface PluginHostRuntimeSummary {
  plugin_id: string;
  artifact_id: string;
  state: string;
  drain_mode: string;
  crash_loop: boolean;
  crash_count: number;
  last_error?: string;
}

export interface PluginServiceStatus {
  service: PluginServiceState;
  hosts?: PluginHostRuntimeSummary[];
}

export interface PluginInstrumentation {
  id: number;
  name: string;
  version: string;
  profile: string;
  generated_diff_hash: string;
  runbook_rollback: string;
  status: string;
  created_by?: string;
  created_at?: number;
}

export interface GovernanceIssue {
  code: string;
  severity: string;
  message: string;
  plugin_id?: string;
  artifact_id?: string;
  details?: Record<string, unknown>;
}

export interface GovernanceDecision {
  ok: boolean;
  action: string;
  profile: string;
  risk_level: string;
  policy_hash: string;
  review_required: boolean;
  warning_override_used: boolean;
  issues?: GovernanceIssue[];
  checks?: GovernanceIssue[];
}

export interface GovernanceStatus {
  decision?: GovernanceDecision;
  policy?: Record<string, unknown>;
  reviews?: Record<string, unknown>[];
  warning_overrides?: Record<string, unknown>[];
  preflights?: Record<string, unknown>[];
  benchmarks?: Record<string, unknown>[];
  advisories?: Record<string, unknown>[];
  conflicts?: {
    ok: boolean;
    issues?: GovernanceIssue[];
    plan?: unknown;
  };
}

export interface PluginView {
  id: string;
  name?: string;
  version?: string;
  artifact_type?: string;
  runtime_type?: string;
  desired_state: string;
  runtime_state: string;
  desired_artifact_id: string;
  active_artifact_id: string;
  loaded_artifact_id: string;
  desired_artifact?: PluginArtifact;
  active_artifact?: PluginArtifact;
  loaded_artifact?: PluginArtifact;
  extension_points?: string[];
  priority: number;
  scope?: unknown;
  rollout?: unknown;
  restart_required?: boolean;
  health?: string;
  last_error?: string;
  runtime_summary?: Record<string, unknown>;
  dispatch_summary?: unknown[];
  extension_status?: Record<string, unknown>;
  capabilities_summary?: Record<string, unknown>;
  minecraft?: unknown;
  config_json?: string;
  config_schema?: Record<string, unknown>;
  secrets?: PluginSecret[];
  snapshots?: PluginSnapshot[];
  builds?: PluginBuild[];
  artifacts?: PluginArtifact[];
  manifest?: Record<string, unknown>;
  active_proxy_connections?: number;
  proxy_connections?: PluginProxyConnection[];
  governance?: GovernanceStatus;
  governance_error?: string;
  updated_at?: number;
}

export interface PluginOperations {
  handlers?: Record<string, unknown>[];
  builds?: Record<string, unknown>[];
  events?: Record<string, unknown>[];
  custom_metrics?: Record<string, unknown>[];
  logs?: Record<string, unknown>[];
  traces?: Record<string, unknown>[];
  background_tasks?: Record<string, unknown>[];
  plugin_data?: Record<string, unknown>[];
  plugin_files?: Record<string, unknown>[];
  external_dependencies?: Record<string, unknown>[];
  gc?: Record<string, unknown>[];
  event_queue?: Record<string, unknown>;
  diagnostics?: Record<string, unknown>[];
}

export interface PluginDryRunResult {
  ok: boolean;
  restart_required: boolean;
  hot_reload: boolean;
  sensitive_paths: string[];
  redacted_config_json: string;
  redacted_diff_json: string;
}

export interface SetupStatus {
  required: boolean;
}

export interface LoginResponse {
  token: string;
  user: User;
}

export interface RuntimeConfig {
  apiPrefix: string;
}
