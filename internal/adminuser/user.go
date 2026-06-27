// internal/adminuser/user.go 定义管理用户角色、校验规则、密码哈希和对外用户视图。

package adminuser

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// 角色按权限从高到低排列：admin 管理所有资源，member 管理路由和查看运行态，
	// guest 只保留基础只读能力。
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
	// 用户名会出现在 URL path 和审计记录中，因此禁止空白和斜杠。
	if strings.ContainsAny(username, " \t\r\n/") {
		return errors.New("username must not contain whitespace or /")
	}
	return nil
}

func ValidateRole(role string) error {
	// 所有角色必须在这里登记，避免数据库里出现管理端无法解释的角色。
	switch role {
	case RoleAdmin, RoleMember, RoleGuest:
		return nil
	default:
		return fmt.Errorf("invalid role %q", role)
	}
}

func ValidatePassword(password string) error {
	// 当前只做非空校验；更复杂的密码策略应放在产品策略确定后再补。
	if strings.TrimSpace(password) == "" {
		return errors.New("password is required")
	}
	return nil
}

func RoleRank(role string) int {
	// rank 让权限判断保持单调：高角色天然包含低角色能力。
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

// Permissions 返回前端可直接消费的权限位；后端仍以角色校验为准。
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
