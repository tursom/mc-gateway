// internal/adminsession/session_test.go 包含用于约束 session 行为的测试。

package adminsession

import (
	"testing"
	"time"
)

func TestManagerCreateAndGet(t *testing.T) {
	now := time.Unix(100, 0)
	manager := NewManagerWithClock(func() time.Time { return now })

	session, err := manager.Create("admin", "admin", time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if session.Token == "" {
		t.Fatal("Create() token is empty")
	}
	if len(session.Token) != 43 {
		t.Fatalf("Create() token len = %d, want 43", len(session.Token))
	}
	if !session.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want %v", session.ExpiresAt, now.Add(time.Hour))
	}

	got, ok := manager.Get(session.Token)
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if got.Username != "admin" || got.Role != "admin" {
		t.Fatalf("Get() = %+v", got)
	}
}

func TestManagerExpiresSessions(t *testing.T) {
	now := time.Unix(100, 0)
	manager := NewManagerWithClock(func() time.Time { return now })

	session, err := manager.Create("admin", "admin", time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	now = now.Add(time.Hour + time.Second)
	if _, ok := manager.Get(session.Token); ok {
		t.Fatal("Get(expired) ok = true, want false")
	}
	if _, ok := manager.Get(session.Token); ok {
		t.Fatal("Get(expired deleted) ok = true, want false")
	}
}

func TestManagerDeleteAndRemoveUser(t *testing.T) {
	manager := NewManager()

	admin, err := manager.Create("admin", "admin", time.Hour)
	if err != nil {
		t.Fatalf("Create(admin) error = %v", err)
	}
	member, err := manager.Create("member", "member", time.Hour)
	if err != nil {
		t.Fatalf("Create(member) error = %v", err)
	}
	guest, err := manager.Create("guest", "guest", time.Hour)
	if err != nil {
		t.Fatalf("Create(guest) error = %v", err)
	}

	manager.Delete(guest.Token)
	if _, ok := manager.Get(guest.Token); ok {
		t.Fatal("guest token still exists after Delete")
	}

	manager.RemoveUser("admin")
	if _, ok := manager.Get(admin.Token); ok {
		t.Fatal("admin token still exists after RemoveUser")
	}
	if _, ok := manager.Get(member.Token); !ok {
		t.Fatal("member token was removed by RemoveUser(admin)")
	}

	manager.Clear()
	if _, ok := manager.Get(member.Token); ok {
		t.Fatal("member token still exists after Clear")
	}
}
