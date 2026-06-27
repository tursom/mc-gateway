// internal/adminroute/repository_test.go 包含用于约束 repository 行为的测试。

package adminroute

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
)

func TestRepositoryUpsertListEnabledMapAndDelete(t *testing.T) {
	repo, closeDB := newRouteTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Upsert(ctx, "admin", "play.example", "127.0.0.1:25565", true, "primary"); err != nil {
		t.Fatalf("Upsert(play) error = %v", err)
	}
	if err := repo.Upsert(ctx, "admin", "dev.example", "127.0.0.1:25566", false, "disabled"); err != nil {
		t.Fatalf("Upsert(dev) error = %v", err)
	}

	routes, err := repo.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("List() len = %d, want 2", len(routes))
	}
	if routes[0].Host != "dev.example" || routes[0].Enabled {
		t.Fatalf("routes[0] = %+v, want disabled dev route", routes[0])
	}
	if routes[1].Host != "play.example" || !routes[1].Enabled || routes[1].CreatedAt != 100 || routes[1].UpdatedAt != 100 || routes[1].UpdatedBy != "admin" {
		t.Fatalf("routes[1] = %+v, want enabled play route with timestamps", routes[1])
	}

	enabled, err := repo.EnabledMap(ctx)
	if err != nil {
		t.Fatalf("EnabledMap() error = %v", err)
	}
	if want := map[string]string{"play.example": "127.0.0.1:25565"}; !reflect.DeepEqual(enabled, want) {
		t.Fatalf("EnabledMap() = %#v, want %#v", enabled, want)
	}

	filtered, err := repo.List(ctx, "primary")
	if err != nil {
		t.Fatalf("List(query) error = %v", err)
	}
	if len(filtered) != 1 || filtered[0].Host != "play.example" {
		t.Fatalf("List(query) = %+v", filtered)
	}

	if err := repo.Delete(ctx, "play.example"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	enabled, err = repo.EnabledMap(ctx)
	if err != nil {
		t.Fatalf("EnabledMap(after delete) error = %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("EnabledMap(after delete) = %#v, want empty", enabled)
	}
}

func TestRepositoryUpsertUpdatesExistingRoute(t *testing.T) {
	now := time.Unix(100, 0)
	repo, closeDB := newRouteTestRepository(t, now)
	defer closeDB()

	ctx := context.Background()
	if err := repo.Upsert(ctx, "admin", "play.example", "127.0.0.1:25565", true, "primary"); err != nil {
		t.Fatalf("Upsert(create) error = %v", err)
	}
	repo.now = func() time.Time { return time.Unix(200, 0) }
	if err := repo.Upsert(ctx, "member", "play.example", "127.0.0.1:25566", false, "updated"); err != nil {
		t.Fatalf("Upsert(update) error = %v", err)
	}

	routes, err := repo.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("List() len = %d, want 1", len(routes))
	}
	route := routes[0]
	if route.CreatedAt != 100 || route.UpdatedAt != 200 || route.UpdatedBy != "member" || route.Enabled {
		t.Fatalf("updated route = %+v", route)
	}
	if route.Upstream != "127.0.0.1:25566" || route.Note != "updated" {
		t.Fatalf("updated route = %+v", route)
	}
}

func TestRepositoryValidationErrors(t *testing.T) {
	repo, closeDB := newRouteTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Upsert(ctx, "admin", "bad host", "127.0.0.1:25565", true, ""); err == nil {
		t.Fatal("Upsert(invalid host) error = nil, want error")
	}
	if err := repo.Upsert(ctx, "admin", "play.example", "127.0.0.1", true, ""); err == nil {
		t.Fatal("Upsert(invalid upstream) error = nil, want error")
	}
	if err := repo.Delete(ctx, "bad/host"); err == nil {
		t.Fatal("Delete(invalid host) error = nil, want error")
	}
}

func newRouteTestRepository(t *testing.T, now time.Time) (Repository, func()) {
	t.Helper()

	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := admindb.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("Migrate() error = %v", err)
	}
	return NewRepositoryWithClock(db, func() time.Time { return now }), func() {
		_ = db.Close()
	}
}
