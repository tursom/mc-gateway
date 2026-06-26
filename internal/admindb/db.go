//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (openbsd && (amd64 || arm64)) || (windows && (386 || amd64 || arm64))

package admindb

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

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
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

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

CREATE INDEX IF NOT EXISTS idx_routes_enabled ON routes(enabled);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_artifacts_plugin_id ON plugin_artifacts(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugins_desired_state ON plugins(desired_state, priority);
CREATE INDEX IF NOT EXISTS idx_plugin_operations_plugin_id ON plugin_operations(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_builds_plugin_id ON plugin_builds(plugin_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_builds_source_id ON plugin_builds(source_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plugin_config_snapshots_plugin_id ON plugin_config_snapshots(plugin_id, created_at);
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, strftime('%s','now'));
`
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	return ensureColumn(db, "audit_logs", "metadata_json", "TEXT NOT NULL DEFAULT '{}'")
}

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
