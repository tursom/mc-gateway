package adminuser

import (
	"reflect"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	valid := []string{"admin", "member-1", "guest.example"}
	for _, username := range valid {
		t.Run(username, func(t *testing.T) {
			if err := ValidateUsername(username); err != nil {
				t.Fatalf("ValidateUsername(%q) error = %v", username, err)
			}
		})
	}

	invalid := []string{"", "admin user", "admin/user", "admin\tuser", "admin\nuser"}
	for _, username := range invalid {
		t.Run("invalid "+username, func(t *testing.T) {
			if err := ValidateUsername(username); err == nil {
				t.Fatalf("ValidateUsername(%q) error = nil, want error", username)
			}
		})
	}
}

func TestValidateRole(t *testing.T) {
	for _, role := range []string{RoleAdmin, RoleMember, RoleGuest} {
		t.Run(role, func(t *testing.T) {
			if err := ValidateRole(role); err != nil {
				t.Fatalf("ValidateRole(%q) error = %v", role, err)
			}
		})
	}
	if err := ValidateRole("owner"); err == nil {
		t.Fatal("ValidateRole(owner) error = nil, want error")
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("secret"); err != nil {
		t.Fatalf("ValidatePassword(secret) error = %v", err)
	}
	for _, password := range []string{"", " ", "\t"} {
		t.Run("empty", func(t *testing.T) {
			if err := ValidatePassword(password); err == nil {
				t.Fatal("ValidatePassword() error = nil, want error")
			}
		})
	}
}

func TestRoleRankAndHasRole(t *testing.T) {
	tests := []struct {
		role string
		want int
	}{
		{RoleAdmin, 3},
		{RoleMember, 2},
		{RoleGuest, 1},
		{"unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			if got := RoleRank(tt.role); got != tt.want {
				t.Fatalf("RoleRank(%q) = %d, want %d", tt.role, got, tt.want)
			}
		})
	}

	if !HasRole(RoleAdmin, RoleGuest) {
		t.Fatal("admin should satisfy guest")
	}
	if HasRole(RoleGuest, RoleMember) {
		t.Fatal("guest should not satisfy member")
	}
}

func TestPermissions(t *testing.T) {
	wantGuest := map[string]bool{
		"read_routes":     true,
		"write_routes":    false,
		"read_status":     false,
		"read_plugins":    false,
		"manage_plugins":  false,
		"manage_users":    false,
		"manage_services": false,
	}
	if got := Permissions(RoleGuest); !reflect.DeepEqual(got, wantGuest) {
		t.Fatalf("Permissions(guest) = %#v, want %#v", got, wantGuest)
	}

	wantAdmin := map[string]bool{
		"read_routes":     true,
		"write_routes":    true,
		"read_status":     true,
		"read_plugins":    true,
		"manage_plugins":  true,
		"manage_users":    true,
		"manage_services": true,
	}
	if got := Permissions(RoleAdmin); !reflect.DeepEqual(got, wantAdmin) {
		t.Fatalf("Permissions(admin) = %#v, want %#v", got, wantAdmin)
	}
}
