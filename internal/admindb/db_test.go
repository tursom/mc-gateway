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
}

func sqliteObjectExists(t *testing.T, db *sql.DB, objectType, name string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil {
		t.Fatalf("sqlite_master query error = %v", err)
	}
	return count == 1
}
