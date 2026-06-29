// internal/admindb/db_test.go 包含用于约束 db 行为的测试。

package admindb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenCreatesDirectoryAndConfiguresSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "gateway.sqlite3")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode error = %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout error = %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestMigrateCreatesSchema(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate() second run error = %v", err)
	}

	for _, name := range []string{"schema_migrations", "users", "routes", "services", "audit_logs"} {
		t.Run("table "+name, func(t *testing.T) {
			if !sqliteObjectExists(t, db, "table", name) {
				t.Fatalf("table %q was not created", name)
			}
		})
	}
	for _, name := range []string{"idx_routes_enabled", "idx_audit_logs_created_at"} {
		t.Run("index "+name, func(t *testing.T) {
			if !sqliteObjectExists(t, db, "index", name) {
				t.Fatalf("index %q was not created", name)
			}
		})
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 1`).Scan(&version); err != nil {
		t.Fatalf("schema migration version query error = %v", err)
	}
	if version != 1 {
		t.Fatalf("schema migration version = %d, want 1", version)
	}
	for _, column := range []string{"gateway_binary_sha256", "ci_artifact_sha256"} {
		if !sqliteColumnExists(t, db, "plugin_instrumentation", column) {
			t.Fatalf("plugin_instrumentation missing column %q", column)
		}
	}
	for _, column := range []string{"crash_backoff_seconds", "crash_max_count", "crash_window_seconds"} {
		if !sqliteColumnExists(t, db, "plugin_service_state", column) {
			t.Fatalf("plugin_service_state missing column %q", column)
		}
	}
}

func sqliteObjectExists(t *testing.T, db *sql.DB, objectType, name string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil {
		t.Fatalf("sqlite_master query error = %v", err)
	}
	return count == 1
}

func sqliteColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()

	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s) error = %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("table_info(%s) scan error = %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s) rows error = %v", table, err)
	}
	return false
}
