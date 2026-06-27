// cmd/gateway/admin_users.go 初始化管理用户仓库，并在没有账号时创建首次初始化用户。

package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/tursom/mc-gateway/internal/adminuser"
	"golang.org/x/crypto/bcrypt"
)

const (
	adminRoleAdmin  = adminuser.RoleAdmin
	adminRoleMember = adminuser.RoleMember
	adminRoleGuest  = adminuser.RoleGuest
)

func ensureInitialAdminFromEnv(ctx context.Context, db *sql.DB, password string) error {
	if strings.TrimSpace(password) == "" {
		return nil
	}
	empty, err := usersTableEmpty(ctx, db)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	return createUser(ctx, "system", "admin", adminRoleAdmin, password, false)
}

func usersTableEmpty(ctx context.Context, db *sql.DB) (bool, error) {
	return adminuser.NewRepository(db).TableEmpty(ctx)
}

func createInitialAdmin(ctx context.Context, username, password string) error {
	empty, err := usersTableEmpty(ctx, adminDB)
	if err != nil {
		return err
	}
	if !empty {
		return errors.New("initial admin has already been created")
	}
	return createUser(ctx, "setup", username, adminRoleAdmin, password, false)
}

func createUser(ctx context.Context, actor, username, role, password string, disabled bool) error {
	username = strings.TrimSpace(username)
	if err := adminuser.ValidateUsername(username); err != nil {
		return err
	}
	if err := adminuser.ValidateRole(role); err != nil {
		return err
	}
	if err := adminuser.ValidatePassword(password); err != nil {
		return err
	}

	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	_ = actor
	return adminuser.NewRepository(adminDB).Create(ctx, username, role, hash, disabled)
}

func authenticateUser(ctx context.Context, username, password string) (adminuser.User, error) {
	user, hash, err := getUserWithHash(ctx, username)
	if err != nil {
		return adminuser.User{}, err
	}
	if user.Disabled {
		return adminuser.User{}, errors.New("user is disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return adminuser.User{}, errors.New("invalid username or password")
	}
	return user, nil
}

func getUserWithHash(ctx context.Context, username string) (adminuser.User, string, error) {
	return adminuser.NewRepository(adminDB).GetWithHash(ctx, username)
}

func listUsers(ctx context.Context) ([]adminuser.User, error) {
	return adminuser.NewRepository(adminDB).List(ctx)
}

func patchUser(ctx context.Context, actor, username string, role *string, disabled *bool, password *string) error {
	username = strings.TrimSpace(username)
	if err := adminuser.ValidateUsername(username); err != nil {
		return err
	}

	var passwordHashProvider func() (string, error)
	if password != nil {
		passwordValue := *password
		passwordHashProvider = func() (string, error) {
			if err := adminuser.ValidatePassword(passwordValue); err != nil {
				return "", err
			}
			return hashPassword(passwordValue)
		}
	}

	result, err := adminuser.NewRepository(adminDB).Patch(ctx, username, adminuser.Patch{
		Role:                 role,
		Disabled:             disabled,
		PasswordHashProvider: passwordHashProvider,
	})
	if err != nil {
		return err
	}
	if result.InvalidateSessions {
		removeSessionsForUser(username)
	}
	_ = actor
	return nil
}

func deleteUser(ctx context.Context, username string) error {
	username = strings.TrimSpace(username)
	if err := adminuser.ValidateUsername(username); err != nil {
		return err
	}

	if err := adminuser.NewRepository(adminDB).Delete(ctx, username); err != nil {
		return err
	}
	removeSessionsForUser(username)
	return nil
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}
