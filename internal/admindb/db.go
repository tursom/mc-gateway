//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (openbsd && (amd64 || arm64)) || (windows && (386 || amd64 || arm64))

// internal/admindb/db.go 打开跨平台 SQLite 数据库，并应用管理、路由和插件运行态共用的表结构。

package admindb

import (
	"database/sql"
	"os"
	"path/filepath"

	// modernc.org/sqlite 是纯 Go SQLite 驱动，便于容器和跨平台构建时避免 CGO 依赖。
	_ "modernc.org/sqlite"
)

// Open 创建或打开管理运行态数据库。调用方传入的路径可以包含尚不存在的目录，
// 这里会先创建目录，再打开 SQLite 连接。
func Open(dbPath string) (*sql.DB, error) {
	if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// SQLite 对单写者最友好；限制连接数可以避免 database/sql 在高并发下
	// 打开多条连接后互相争用写锁。
	db.SetMaxOpenConns(1)

	// WAL 让读请求不会被普通写事务完全阻塞，适合管理端读多写少的状态库。
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, err
	}
	// 写锁短暂冲突时等待一小段时间，减少管理端并发操作产生的偶发 busy 错误。
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// Migrate 以幂等方式应用当前 schema。所有 CREATE TABLE 都使用
// IF NOT EXISTS，后续字段演进通过 ensureColumn 补齐，便于老数据库平滑升级。
func Migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    username TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    disabled INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS routes (
    host TEXT PRIMARY KEY,
    upstream TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    note TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS services (
    name TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL,
    port INTEGER NOT NULL DEFAULT 0,
    options_json TEXT NOT NULL DEFAULT '{}',
    restart_required INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    source_ip TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    success INTEGER NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_artifacts (
    id TEXT PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    version TEXT NOT NULL,
    file_name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    package_sha256 TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL,
    artifact_type TEXT NOT NULL DEFAULT 'binary',
    runtime_type TEXT NOT NULL DEFAULT '',
    runtime_entry TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'uploaded',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    capabilities_summary_json TEXT NOT NULL DEFAULT '{}',
    extension_points_json TEXT NOT NULL DEFAULT '[]',
    api_version TEXT NOT NULL DEFAULT '',
    go_version TEXT NOT NULL DEFAULT '',
    go_os TEXT NOT NULL DEFAULT '',
    go_arch TEXT NOT NULL DEFAULT '',
    uploaded_by TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugins (
    id TEXT PRIMARY KEY,
    desired_artifact_id TEXT NOT NULL DEFAULT '',
    active_artifact_id TEXT NOT NULL DEFAULT '',
    loaded_artifact_id TEXT NOT NULL DEFAULT '',
    desired_state TEXT NOT NULL DEFAULT 'disabled',
    runtime_state TEXT NOT NULL DEFAULT 'not_loaded',
    priority INTEGER NOT NULL DEFAULT 100,
    config_json TEXT NOT NULL DEFAULT '{}',
    desired_generation INTEGER NOT NULL DEFAULT 1,
    applied_generation INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    runtime_summary_json TEXT NOT NULL DEFAULT '{}',
    dispatch_summary_json TEXT NOT NULL DEFAULT '{}',
    deleted_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (desired_artifact_id) REFERENCES plugin_artifacts(id)
);

CREATE TABLE IF NOT EXISTS plugin_operations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL DEFAULT '',
    artifact_id TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL,
    status TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_builds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    artifact_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued',
    builder_type TEXT NOT NULL DEFAULT '',
    builder_image TEXT NOT NULL DEFAULT '',
    builder_version TEXT NOT NULL DEFAULT '',
    go_version TEXT NOT NULL DEFAULT '',
    go_os TEXT NOT NULL DEFAULT '',
    go_arch TEXT NOT NULL DEFAULT '',
    go_amd64 TEXT NOT NULL DEFAULT '',
    go_arm64 TEXT NOT NULL DEFAULT '',
    cgo_enabled TEXT NOT NULL DEFAULT '',
    build_tags TEXT NOT NULL DEFAULT '',
    sdk_module TEXT NOT NULL DEFAULT '',
    sdk_version TEXT NOT NULL DEFAULT '',
    go_proxy TEXT NOT NULL DEFAULT '',
    go_no_sumdb TEXT NOT NULL DEFAULT '',
    go_private TEXT NOT NULL DEFAULT '',
    vendor_required INTEGER NOT NULL DEFAULT 0,
    source_sha256 TEXT NOT NULL DEFAULT '',
    artifact_sha256 TEXT NOT NULL DEFAULT '',
    module_summary_json TEXT NOT NULL DEFAULT '[]',
    go_version_m_json TEXT NOT NULL DEFAULT '{}',
    abi_fingerprint TEXT NOT NULL DEFAULT '',
    log_summary TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    error TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL DEFAULT 0,
    ended_at INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_config_snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL DEFAULT '',
    config_json TEXT NOT NULL DEFAULT '{}',
    desired_state TEXT NOT NULL DEFAULT 'disabled',
    priority INTEGER NOT NULL DEFAULT 100,
    desired_generation INTEGER NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_secrets (
    plugin_id TEXT NOT NULL,
    name TEXT NOT NULL,
    current_version INTEGER NOT NULL DEFAULT 1,
    previous_version INTEGER NOT NULL DEFAULT 0,
    current_value TEXT NOT NULL DEFAULT '',
    previous_value TEXT NOT NULL DEFAULT '',
    reload_required INTEGER NOT NULL DEFAULT 0,
    hot_reload INTEGER NOT NULL DEFAULT 0,
    updated_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(plugin_id, name)
);

CREATE TABLE IF NOT EXISTS plugin_reviews (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    artifact_hash TEXT NOT NULL DEFAULT '',
    profile TEXT NOT NULL DEFAULT 'dev',
    risk_level TEXT NOT NULL DEFAULT 'low',
    config_hash TEXT NOT NULL DEFAULT '',
    scope_hash TEXT NOT NULL DEFAULT '',
    rollout_hash TEXT NOT NULL DEFAULT '',
    runtime_limits_hash TEXT NOT NULL DEFAULT '',
    features_hash TEXT NOT NULL DEFAULT '',
    policy_hash TEXT NOT NULL DEFAULT '',
    decision TEXT NOT NULL DEFAULT 'approved',
    notes TEXT NOT NULL DEFAULT '',
    reviewed_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_warning_overrides (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'dev',
    action TEXT NOT NULL DEFAULT '',
    policy_hash TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_advisories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    advisory_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    action TEXT NOT NULL DEFAULT 'denylist',
    artifact_sha256 TEXT NOT NULL DEFAULT '',
    plugin_id TEXT NOT NULL DEFAULT '',
    version_range TEXT NOT NULL DEFAULT '',
    dependency_name TEXT NOT NULL DEFAULT '',
    dependency_range TEXT NOT NULL DEFAULT '',
    recommended_action TEXT NOT NULL DEFAULT '',
    fixed_version TEXT NOT NULL DEFAULT '',
    mitigation TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_vulnerabilities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    vulnerability_id TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    package_name TEXT NOT NULL,
    version_range TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL DEFAULT 'denylist',
    fixed_version TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    references_json TEXT NOT NULL DEFAULT '[]',
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(vulnerability_id, package_name)
);

CREATE TABLE IF NOT EXISTS plugin_preflight_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'dev',
    status TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_benchmarks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'dev',
    benchmark_profile TEXT NOT NULL DEFAULT '',
    p95_ms REAL NOT NULL DEFAULT 0,
    p99_ms REAL NOT NULL DEFAULT 0,
    error_rate REAL NOT NULL DEFAULT 0,
    active_proxy_capacity INTEGER NOT NULL DEFAULT 0,
    baseline_diff REAL NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    name TEXT NOT NULL,
    fields_json TEXT NOT NULL DEFAULT '{}',
    dropped INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_subscriber_dead_letters (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    subscriber_plugin_id TEXT NOT NULL,
    subscriber_artifact_id TEXT NOT NULL DEFAULT '',
    event_plugin_id TEXT NOT NULL,
    event_name TEXT NOT NULL,
    fields_json TEXT NOT NULL DEFAULT '{}',
    trace_id TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    delivery_mode TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    node_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    level TEXT NOT NULL,
    message TEXT NOT NULL,
    fields_json TEXT NOT NULL DEFAULT '{}',
    trace_id TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_traces (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL,
    connection_id TEXT NOT NULL DEFAULT '',
    handler_id TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    fields_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_data (
    plugin_id TEXT NOT NULL,
    key TEXT NOT NULL,
    value BLOB NOT NULL,
    schema_version INTEGER NOT NULL DEFAULT 0,
    data_class TEXT NOT NULL DEFAULT '',
    exportable INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(plugin_id, key)
);

CREATE TABLE IF NOT EXISTS plugin_files (
    plugin_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    path TEXT NOT NULL,
    disk_path TEXT NOT NULL,
    data_class TEXT NOT NULL DEFAULT '',
    exportable INTEGER NOT NULL DEFAULT 0,
    readonly INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(plugin_id, namespace, path)
);

CREATE TABLE IF NOT EXISTS plugin_task_leases (
    plugin_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    shard_key TEXT NOT NULL DEFAULT '',
    owner_node_id TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    acquired_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(plugin_id, task_id, shard_key)
);

CREATE TABLE IF NOT EXISTS plugin_diagnostics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    sections_json TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_service_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    desired_mode TEXT NOT NULL DEFAULT 'in-process',
    active_mode TEXT NOT NULL DEFAULT 'in-process',
    applied_at INTEGER NOT NULL DEFAULT 0,
    live_migration TEXT NOT NULL DEFAULT 'drain-only',
    crash_backoff_seconds INTEGER NOT NULL DEFAULT 30,
    crash_max_count INTEGER NOT NULL DEFAULT 1,
    crash_window_seconds INTEGER NOT NULL DEFAULT 300,
    last_error TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS plugin_nodes (
    node_id TEXT PRIMARY KEY,
    hostname TEXT NOT NULL DEFAULT '',
    pid INTEGER NOT NULL DEFAULT 0,
    service_mode TEXT NOT NULL DEFAULT 'in-process',
    data_plane_mode TEXT NOT NULL DEFAULT 'in-process',
    status TEXT NOT NULL DEFAULT 'online',
    started_at INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_node_states (
    node_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL DEFAULT '',
    desired_state TEXT NOT NULL DEFAULT '',
    runtime_state TEXT NOT NULL DEFAULT '',
    desired_generation INTEGER NOT NULL DEFAULT 0,
    applied_generation INTEGER NOT NULL DEFAULT 0,
    loaded INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 0,
    health TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(node_id, plugin_id)
);

CREATE TABLE IF NOT EXISTS plugin_repository_imports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_type TEXT NOT NULL,
    index_path TEXT NOT NULL DEFAULT '',
    repository_name TEXT NOT NULL DEFAULT '',
    candidate_id TEXT NOT NULL DEFAULT '',
    plugin_id TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    artifact_id TEXT NOT NULL DEFAULT '',
    package_sha256 TEXT NOT NULL DEFAULT '',
    trust_policy TEXT NOT NULL DEFAULT '',
    admission_json TEXT NOT NULL DEFAULT '{}',
    imported_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_repository_index_syncs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_type TEXT NOT NULL,
    index_path TEXT NOT NULL DEFAULT '',
    repository_name TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'succeeded',
    cache_key TEXT NOT NULL DEFAULT '',
    candidate_count INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    synced_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_supply_chain_assessments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'allowed',
    issues_json TEXT NOT NULL DEFAULT '[]',
    signature_json TEXT NOT NULL DEFAULT '{}',
    sbom_json TEXT NOT NULL DEFAULT '{}',
    license_json TEXT NOT NULL DEFAULT '{}',
    advisory_json TEXT NOT NULL DEFAULT '{}',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_trust_roots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    root_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    algorithm TEXT NOT NULL DEFAULT 'ed25519',
    public_key TEXT NOT NULL,
    public_key_sha256 TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'trusted',
    policy_json TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    rotated_at INTEGER NOT NULL DEFAULT 0,
    revoked_at INTEGER NOT NULL DEFAULT 0,
    revocation_reason TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    UNIQUE(root_id, key_id)
);

CREATE TABLE IF NOT EXISTS plugin_instrumentation (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    profile TEXT NOT NULL DEFAULT '',
    generated_diff_hash TEXT NOT NULL DEFAULT '',
    gateway_binary_sha256 TEXT NOT NULL DEFAULT '',
    ci_artifact_sha256 TEXT NOT NULL DEFAULT '',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    conformance_json TEXT NOT NULL DEFAULT '{}',
    benchmark_json TEXT NOT NULL DEFAULT '{}',
    smoke_json TEXT NOT NULL DEFAULT '{}',
    runbook_rollback TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'available',
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_routes_enabled ON routes(enabled);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_artifacts_plugin_id ON plugin_artifacts(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugins_desired_state ON plugins(desired_state, priority);
CREATE INDEX IF NOT EXISTS idx_plugin_operations_plugin_id ON plugin_operations(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_builds_plugin_id ON plugin_builds(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_builds_source_id ON plugin_builds(source_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_config_snapshots_plugin_id ON plugin_config_snapshots(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_secrets_plugin_id ON plugin_secrets(plugin_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_plugin_reviews_lookup ON plugin_reviews(plugin_id, artifact_id, profile, policy_hash, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_warning_overrides_lookup ON plugin_warning_overrides(plugin_id, artifact_id, profile, action, expires_at);
CREATE INDEX IF NOT EXISTS idx_plugin_advisories_artifact ON plugin_advisories(artifact_sha256, status);
CREATE INDEX IF NOT EXISTS idx_plugin_advisories_plugin ON plugin_advisories(plugin_id, status);
CREATE INDEX IF NOT EXISTS idx_plugin_vulnerabilities_package ON plugin_vulnerabilities(package_name, status);
CREATE INDEX IF NOT EXISTS idx_plugin_preflight_lookup ON plugin_preflight_results(plugin_id, artifact_id, profile, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_benchmarks_lookup ON plugin_benchmarks(plugin_id, artifact_id, profile, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_events_lookup ON plugin_events(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_subscriber_dead_letters_status ON plugin_subscriber_dead_letters(status, subscriber_plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_logs_lookup ON plugin_logs(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_traces_lookup ON plugin_traces(plugin_id, trace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_data_expires ON plugin_data(expires_at);
CREATE INDEX IF NOT EXISTS idx_plugin_files_expires ON plugin_files(expires_at);
CREATE INDEX IF NOT EXISTS idx_plugin_task_leases_expires ON plugin_task_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_plugin_diagnostics_lookup ON plugin_diagnostics(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_nodes_heartbeat ON plugin_nodes(heartbeat_at);
CREATE INDEX IF NOT EXISTS idx_plugin_node_states_plugin ON plugin_node_states(plugin_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_plugin_repository_imports_artifact ON plugin_repository_imports(artifact_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_repository_index_syncs_lookup ON plugin_repository_index_syncs(repository_type, index_path, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_supply_chain_lookup ON plugin_supply_chain_assessments(plugin_id, artifact_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_trust_roots_lookup ON plugin_trust_roots(root_id, key_id, status);
CREATE INDEX IF NOT EXISTS idx_plugin_instrumentation_lookup ON plugin_instrumentation(name, created_at);
INSERT OR IGNORE INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, updated_by, updated_at)
VALUES (1, 'in-process', 'in-process', strftime('%s','now'), 'drain-only', 30, 1, 300, 'system', strftime('%s','now'));
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, strftime('%s','now'));
`
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// CREATE TABLE 不会修改已存在的表，因此历史版本新增字段需要显式补齐。
	// 每个 ensureColumn 都是幂等的，可以安全地在每次启动迁移时执行。
	if err := ensureColumn(db, "audit_logs", "metadata_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_secrets", "reload_required", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_secrets", "hot_reload", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_service_state", "live_migration", "TEXT NOT NULL DEFAULT 'drain-only'"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_service_state", "last_error", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_service_state", "crash_backoff_seconds", "INTEGER NOT NULL DEFAULT 30"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_service_state", "crash_max_count", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_service_state", "crash_window_seconds", "INTEGER NOT NULL DEFAULT 300"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_reviews", "artifact_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := db.Exec(`
UPDATE plugin_reviews
SET artifact_hash = COALESCE((SELECT sha256 FROM plugin_artifacts WHERE plugin_artifacts.id = plugin_reviews.artifact_id), '')
WHERE artifact_hash = ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_instrumentation", "gateway_binary_sha256", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plugin_instrumentation", "ci_artifact_sha256", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// ensureColumn 在表缺字段时执行 ALTER TABLE。table/column/definition 只由
// 受控迁移代码传入，不接收外部输入，避免把 PRAGMA 语句做成动态用户入口。
func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}
