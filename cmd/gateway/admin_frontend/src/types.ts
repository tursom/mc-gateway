// cmd/gateway/admin_frontend/src/types.ts 声明 Admin API 返回并被各视图消费的 TypeScript 数据结构。

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
  builder_image?: string;
  builder_version?: string;
  go_version?: string;
  go_os?: string;
  go_arch?: string;
  go_amd64?: string;
  go_arm64?: string;
  cgo_enabled?: string;
  build_tags?: string;
  sdk_module?: string;
  sdk_version?: string;
  go_proxy?: string;
  go_no_sumdb?: string;
  go_private?: string;
  vendor_required?: boolean;
  source_sha256?: string;
  artifact_sha256?: string;
  module_summary_json?: string;
  go_version_m_json?: string;
  abi_fingerprint?: string;
  log_summary?: string;
  metadata_json?: string;
  error?: string;
  started_at?: number;
  ended_at?: number;
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

export interface RepositoryUpdateReport {
  repository_type: string;
  index_path: string;
  repository_name: string;
  checked_at: number;
  candidates: RepositoryUpdateCandidate[];
  updates: RepositoryUpdateCandidate[];
}

export interface RepositoryUpdateCandidate {
  candidate_id: string;
  plugin_id: string;
  available_version: string;
  candidate_sha256?: string;
  current_version?: string;
  current_artifact_id?: string;
  current_package_sha256?: string;
  update_available: boolean;
  reason: string;
  version_comparison?: number;
  version_comparison_stable: boolean;
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
  data_plane_mode?: string;
  implemented_adapter?: boolean;
  desired_maturity?: string;
  active_maturity?: string;
  applied_at?: number;
  restart_required: boolean;
  live_migration?: string;
  crash_policy?: PluginHostCrashPolicy;
  reason_code?: string;
  unsupported_reason?: string;
  last_error?: string;
  updated_by?: string;
  updated_at?: number;
}

export interface SandboxEnforcementFact {
  category: string;
  key: string;
  required: boolean;
  enforced: boolean;
  method?: string;
  unsupported_reason?: string;
  details?: Record<string, string>;
}

export interface SandboxEnvironmentStatus {
  gate_enabled: boolean;
  self_check_ok: boolean;
  data_plane_eligible: boolean;
  policy_profile?: string;
  reason_code?: string;
  reason?: string;
  enforcement_facts?: SandboxEnforcementFact[];
}

export interface PluginHostCrashPolicy {
  backoff_seconds: number;
  max_crashes?: number;
  window_seconds?: number;
}

export interface PluginServiceModeFeature {
  mode: string;
  implemented: boolean;
  maturity: string;
  data_plane: boolean;
  requires_restart: boolean;
  host_protocol?: string;
  control_channel?: string;
  unsupported_reason?: string;
}

export interface PluginRuntimeFeature {
  type: string;
  implemented: boolean;
  maturity: string;
  data_plane: boolean;
  requires_restart: boolean;
  reason_code?: string;
  unsupported_reason?: string;
  entry?: string;
}

export interface PluginRuntimeAdapterStatus {
  service_mode: string;
  runtime_type: string;
  adapter: string;
  implemented: boolean;
  maturity: string;
  data_plane: boolean;
  lifecycle: boolean;
  requires_restart: boolean;
  host_protocol?: string;
  control_channel?: string;
  unsupported_reason?: string;
}

export interface PluginExtensionPointFeature {
  key: string;
  type: string;
  implemented: boolean;
  maturity: string;
  data_plane: boolean;
  requires_restart: boolean;
  unsupported_reason?: string;
}

export interface PluginFeatureFacts {
  schema_version: string;
  api_version: string;
  runtime_types?: PluginRuntimeFeature[];
  service_modes?: PluginServiceModeFeature[];
  runtime_adapters?: PluginRuntimeAdapterStatus[];
  extension_points?: PluginExtensionPointFeature[];
}

export interface PluginHostRuntimeSummary {
  plugin_id: string;
  artifact_id: string;
  pid?: number;
  state: string;
  drain_mode: string;
  crash_loop: boolean;
  crash_count: number;
  reason_code?: string;
  last_error?: string;
  started_at?: number;
  draining_at?: number;
  exited_at?: number;
  last_crash_at?: number;
  backoff_until?: number;
  isolated?: boolean;
}

export interface PluginNodeState {
  node_id: string;
  hostname?: string;
  pid?: number;
  service_mode?: string;
  data_plane_mode?: string;
  status?: string;
  started_at?: number;
  heartbeat_at?: number;
  stale?: boolean;
}

export interface PluginNodeRuntimeState {
  node_id: string;
  plugin_id: string;
  artifact_id?: string;
  desired_state?: string;
  runtime_state?: string;
  desired_generation?: number;
  applied_generation?: number;
  loaded?: boolean;
  enabled?: boolean;
  health?: string;
  error?: string;
  updated_at?: number;
  node_heartbeat_at?: number;
  stale?: boolean;
}

export interface PluginRolloutStatus {
  plugin_id: string;
  desired_state: string;
  desired_artifact_id?: string;
  desired_generation?: number;
  ok: boolean;
  partial_failure: boolean;
  nodes_total: number;
  nodes_ready: number;
  nodes_failed: number;
  nodes_stale: number;
  node_runtime_states?: PluginNodeRuntimeState[];
  artifact_distribution?: boolean;
  artifact_distribution_mode?: string;
  artifact_distribution_status?: string;
  artifact_distribution_error?: string;
  artifact_package_sha256?: string;
  cross_node_apply?: boolean;
}

export interface PluginServiceStatus {
  service: PluginServiceState;
  service_modes?: PluginServiceModeFeature[];
  runtime_types?: PluginRuntimeFeature[];
  runtime_adapters?: PluginRuntimeAdapterStatus[];
  sandbox_environment?: SandboxEnvironmentStatus;
  hosts?: PluginHostRuntimeSummary[];
  nodes?: PluginNodeState[];
}

export interface PluginInstrumentation {
  id: number;
  name: string;
  version: string;
  profile: string;
  generated_diff_hash: string;
  gateway_binary_sha256?: string;
  ci_artifact_sha256?: string;
  provenance_json?: string;
  conformance_json?: string;
  benchmark_json?: string;
  smoke_json?: string;
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

export interface PolicySnapshot {
  profile: string;
  warning_override_ttl_seconds?: number;
  review_required_risk?: string;
  warn_benchmark_regression?: number;
  block_benchmark_regression?: number;
  require_conformance_fixture?: boolean;
  created_at?: number;
}

export interface GovernanceStatus {
  decision?: GovernanceDecision;
  policy?: PolicySnapshot;
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
  rollout_status?: PluginRolloutStatus;
  rollout_error?: string;
  node_runtime_states?: PluginNodeRuntimeState[];
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
  sandbox_runtimes?: Record<string, unknown>[];
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
