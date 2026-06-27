// internal/adminservice/repository_test.go 包含用于约束 repository 行为的测试。

package adminservice

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
)

func TestRepositoryEnsureDefaultsAndList(t *testing.T) {
	repo, closeDB := newServiceTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.EnsureDefaults(ctx, 25575); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := repo.EnsureDefaults(ctx, 25576); err != nil {
		t.Fatalf("EnsureDefaults(second) error = %v", err)
	}

	services, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(services) != 4 {
		t.Fatalf("List() len = %d, want 4", len(services))
	}
	names := []string{services[0].Name, services[1].Name, services[2].Name, services[3].Name}
	if want := []string{NameTCPAdmin, NameKCP, NameQUIC, NameWebSocket}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service order = %#v, want %#v", names, want)
	}
	if services[0].Port != 25575 || !services[0].Enabled || services[0].CreatedAt != 100 || services[0].UpdatedAt != 100 {
		t.Fatalf("tcp_admin service = %+v", services[0])
	}
	if services[1].Options["data_shards"] != float64(DefaultKCPDataShards) {
		t.Fatalf("kcp options = %#v", services[1].Options)
	}
	if got := StringSliceOption(services[2].Options, "application_protocols"); !reflect.DeepEqual(got, []string{"minecraft", "quic", "raw", "h3"}) {
		t.Fatalf("quic protocols = %#v", got)
	}
}

func TestRepositoryUpdate(t *testing.T) {
	repo, closeDB := newServiceTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.EnsureDefaults(ctx, 25565); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	repo.now = func() time.Time { return time.Unix(200, 0) }
	if err := repo.Update(ctx, "admin", NameWebSocket, true, 25580, map[string]any{"path": "gateway"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	services, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var websocket Record
	for _, service := range services {
		if service.Name == NameWebSocket {
			websocket = service
			break
		}
	}
	if !websocket.Enabled || websocket.Port != 25580 || !websocket.RestartRequired || websocket.UpdatedAt != 200 || websocket.UpdatedBy != "admin" {
		t.Fatalf("websocket service = %+v", websocket)
	}
	if websocket.Options["path"] != "/gateway" {
		t.Fatalf("websocket options = %#v", websocket.Options)
	}
}

func TestRepositoryUpdateValidationErrors(t *testing.T) {
	repo, closeDB := newServiceTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Update(ctx, "admin", "unknown", true, 25565, nil); err == nil {
		t.Fatal("Update(unknown) error = nil, want error")
	}
	if err := repo.Update(ctx, "admin", NameKCP, true, 0, nil); err == nil {
		t.Fatal("Update(port 0) error = nil, want error")
	}
	if err := repo.Update(ctx, "admin", NameTCPAdmin, false, 25565, nil); err == nil {
		t.Fatal("Update(disable tcp_admin) error = nil, want error")
	}
}

func newServiceTestRepository(t *testing.T, now time.Time) (Repository, func()) {
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
