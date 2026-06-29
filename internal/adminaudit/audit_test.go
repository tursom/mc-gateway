// internal/adminaudit/audit_test.go 包含用于约束 audit 行为的测试。

package adminaudit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
)

func TestRepositoryRecordAndList(t *testing.T) {
	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := admindb.Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	now := time.Unix(123, 0)
	repo := NewRepositoryWithClock(db, func() time.Time { return now })

	if err := repo.Record(context.Background(), "admin", "127.0.0.1", "route_upsert", "route", "play.example", true, "route saved"); err != nil {
		t.Fatalf("Record(success) error = %v", err)
	}
	now = time.Unix(124, 0)
	if err := repo.Record(context.Background(), "admin", "127.0.0.1", "route_delete", "route", "play.example", false, "route missing"); err != nil {
		t.Fatalf("Record(failure) error = %v", err)
	}

	logs, err := repo.List(context.Background(), DefaultListLimit)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("List() len = %d, want 2", len(logs))
	}
	if logs[0].Action != "route_delete" || logs[0].Success {
		t.Fatalf("logs[0] = %+v, want latest failure", logs[0])
	}
	if logs[0].CreatedAt != 124 {
		t.Fatalf("logs[0].CreatedAt = %d, want 124", logs[0].CreatedAt)
	}
	if logs[1].Action != "route_upsert" || !logs[1].Success {
		t.Fatalf("logs[1] = %+v, want earlier success", logs[1])
	}

	limited, err := repo.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("List(limit) error = %v", err)
	}
	if len(limited) != 1 || limited[0].Action != "route_delete" {
		t.Fatalf("List(limit) = %+v", limited)
	}
}

func TestRepositoryRecordIgnoresNilDB(t *testing.T) {
	repo := NewRepository(nil)
	if err := repo.Record(context.Background(), "admin", "127.0.0.1", "login", "user", "admin", true, "ok"); err != nil {
		t.Fatalf("Record(nil DB) error = %v", err)
	}
}

func TestRepositoryRecordRedactsSensitiveMessageAndMetadata(t *testing.T) {
	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := admindb.Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	repo := NewRepository(db)
	if err := repo.RecordWithMetadata(context.Background(), "admin", "127.0.0.1", "plugin_secret_update", "plugin_secret", "plugin://plugin-a/api_token", false, "token=plain-secret", map[string]any{
		"token": "plain-secret",
		"nested": map[string]any{
			"password": "plain-password",
			"host":     "play.example",
		},
	}); err != nil {
		t.Fatalf("RecordWithMetadata() error = %v", err)
	}
	logs, err := repo.List(context.Background(), DefaultListLimit)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %+v, want one", logs)
	}
	payload := logs[0].Message + " " + logs[0].MetadataJSON
	for _, forbidden := range []string{"plain-secret", "plain-password"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("audit record leaked %q: %+v", forbidden, logs[0])
		}
	}
	if !strings.Contains(logs[0].MetadataJSON, "play.example") {
		t.Fatalf("metadata_json = %s, want non-sensitive context retained", logs[0].MetadataJSON)
	}
}
