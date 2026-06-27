// internal/adminuser/repository_test.go 包含用于约束 repository 行为的测试。

package adminuser

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/admindb"
)

func TestRepositoryCreateGetListAndTableEmpty(t *testing.T) {
	repo, closeDB := newUserTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	empty, err := repo.TableEmpty(ctx)
	if err != nil {
		t.Fatalf("TableEmpty() error = %v", err)
	}
	if !empty {
		t.Fatal("TableEmpty() = false, want true")
	}

	if err := repo.Create(ctx, "admin", RoleAdmin, "admin-hash", false); err != nil {
		t.Fatalf("Create(admin) error = %v", err)
	}
	if err := repo.Create(ctx, "guest", RoleGuest, "guest-hash", true); err != nil {
		t.Fatalf("Create(guest) error = %v", err)
	}

	empty, err = repo.TableEmpty(ctx)
	if err != nil {
		t.Fatalf("TableEmpty(after create) error = %v", err)
	}
	if empty {
		t.Fatal("TableEmpty(after create) = true, want false")
	}

	user, hash, err := repo.GetWithHash(ctx, "admin")
	if err != nil {
		t.Fatalf("GetWithHash() error = %v", err)
	}
	if user.Username != "admin" || user.Role != RoleAdmin || user.Disabled || user.CreatedAt != 100 || user.UpdatedAt != 100 || hash != "admin-hash" {
		t.Fatalf("GetWithHash() user=%+v hash=%q", user, hash)
	}

	users, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []User{
		{Username: "admin", Role: RoleAdmin, CreatedAt: 100, UpdatedAt: 100},
		{Username: "guest", Role: RoleGuest, Disabled: true, CreatedAt: 100, UpdatedAt: 100},
	}
	if !reflect.DeepEqual(users, want) {
		t.Fatalf("List() = %#v, want %#v", users, want)
	}
}

func TestRepositoryPatch(t *testing.T) {
	now := time.Unix(100, 0)
	repo, closeDB := newUserTestRepository(t, now)
	defer closeDB()

	ctx := context.Background()
	if err := repo.Create(ctx, "admin", RoleAdmin, "admin-hash", false); err != nil {
		t.Fatalf("Create(admin) error = %v", err)
	}
	if err := repo.Create(ctx, "member", RoleMember, "member-hash", false); err != nil {
		t.Fatalf("Create(member) error = %v", err)
	}

	nextRole := RoleGuest
	repo.now = func() time.Time { return time.Unix(200, 0) }
	result, err := repo.Patch(ctx, "member", Patch{
		Role: &nextRole,
		PasswordHashProvider: func() (string, error) {
			return "new-member-hash", nil
		},
	})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if !result.InvalidateSessions {
		t.Fatal("Patch() InvalidateSessions = false, want true")
	}

	user, hash, err := repo.GetWithHash(ctx, "member")
	if err != nil {
		t.Fatalf("GetWithHash(member) error = %v", err)
	}
	if user.Role != RoleGuest || user.Disabled || user.UpdatedAt != 200 || hash != "new-member-hash" {
		t.Fatalf("patched user=%+v hash=%q", user, hash)
	}

	result, err = repo.Patch(ctx, "member", Patch{})
	if err != nil {
		t.Fatalf("Patch(no-op) error = %v", err)
	}
	if result.InvalidateSessions {
		t.Fatal("Patch(no-op) InvalidateSessions = true, want false")
	}
}

func TestRepositoryPreventsRemovingLastEnabledAdmin(t *testing.T) {
	repo, closeDB := newUserTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Create(ctx, "admin", RoleAdmin, "admin-hash", false); err != nil {
		t.Fatalf("Create(admin) error = %v", err)
	}

	guestRole := RoleGuest
	if _, err := repo.Patch(ctx, "admin", Patch{Role: &guestRole}); err == nil || !strings.Contains(err.Error(), "last enabled admin") {
		t.Fatalf("Patch(last admin role) error = %v, want last enabled admin", err)
	}
	disabled := true
	if _, err := repo.Patch(ctx, "admin", Patch{Disabled: &disabled}); err == nil || !strings.Contains(err.Error(), "last enabled admin") {
		t.Fatalf("Patch(last admin disabled) error = %v, want last enabled admin", err)
	}
	if err := repo.Delete(ctx, "admin"); err == nil || !strings.Contains(err.Error(), "last enabled admin") {
		t.Fatalf("Delete(last admin) error = %v, want last enabled admin", err)
	}
}

func TestRepositoryDeleteAdminWhenAnotherAdminExists(t *testing.T) {
	repo, closeDB := newUserTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Create(ctx, "admin1", RoleAdmin, "hash-1", false); err != nil {
		t.Fatalf("Create(admin1) error = %v", err)
	}
	if err := repo.Create(ctx, "admin2", RoleAdmin, "hash-2", false); err != nil {
		t.Fatalf("Create(admin2) error = %v", err)
	}
	if err := repo.Delete(ctx, "admin1"); err != nil {
		t.Fatalf("Delete(admin1) error = %v", err)
	}
	if _, _, err := repo.GetWithHash(ctx, "admin1"); err == nil || err.Error() != "invalid username or password" {
		t.Fatalf("GetWithHash(deleted) error = %v, want invalid username or password", err)
	}
}

func TestRepositoryValidationErrors(t *testing.T) {
	repo, closeDB := newUserTestRepository(t, time.Unix(100, 0))
	defer closeDB()

	ctx := context.Background()
	if err := repo.Create(ctx, "bad user", RoleAdmin, "hash", false); err == nil {
		t.Fatal("Create(invalid username) error = nil, want error")
	}
	if err := repo.Create(ctx, "admin", "owner", "hash", false); err == nil {
		t.Fatal("Create(invalid role) error = nil, want error")
	}
	if err := repo.Create(ctx, "admin", RoleAdmin, "", false); err == nil {
		t.Fatal("Create(empty hash) error = nil, want error")
	}
	if _, err := repo.Patch(ctx, "missing", Patch{}); err == nil || err.Error() != "user not found" {
		t.Fatalf("Patch(missing) error = %v, want user not found", err)
	}
	if err := repo.Delete(ctx, "missing"); err == nil || err.Error() != "user not found" {
		t.Fatalf("Delete(missing) error = %v, want user not found", err)
	}
}

func newUserTestRepository(t *testing.T, now time.Time) (Repository, func()) {
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
