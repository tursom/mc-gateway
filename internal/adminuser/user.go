package adminuser

import (
	"errors"
	"fmt"
	"strings"
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleGuest  = "guest"
)

type User struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func ValidateUsername(username string) error {
	if username == "" {
		return errors.New("username is required")
	}
	if strings.ContainsAny(username, " \t\r\n/") {
		return errors.New("username must not contain whitespace or /")
	}
	return nil
}

func ValidateRole(role string) error {
	switch role {
	case RoleAdmin, RoleMember, RoleGuest:
		return nil
	default:
		return fmt.Errorf("invalid role %q", role)
	}
}

func ValidatePassword(password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("password is required")
	}
	return nil
}

func RoleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleMember:
		return 2
	case RoleGuest:
		return 1
	default:
		return 0
	}
}

func HasRole(actual, required string) bool {
	return RoleRank(actual) >= RoleRank(required)
}

func Permissions(role string) map[string]bool {
	return map[string]bool{
		"read_routes":     HasRole(role, RoleGuest),
		"write_routes":    HasRole(role, RoleMember),
		"read_status":     HasRole(role, RoleMember),
		"read_plugins":    HasRole(role, RoleMember),
		"manage_plugins":  HasRole(role, RoleAdmin),
		"manage_users":    HasRole(role, RoleAdmin),
		"manage_services": HasRole(role, RoleAdmin),
	}
}
