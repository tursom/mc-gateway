package adminuser

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Repository struct {
	db  *sql.DB
	now func() time.Time
}

type Patch struct {
	Role                 *string
	Disabled             *bool
	PasswordHashProvider func() (string, error)
}

type PatchResult struct {
	InvalidateSessions bool
}

func NewRepository(db *sql.DB) Repository {
	return Repository{
		db:  db,
		now: time.Now,
	}
}

func NewRepositoryWithClock(db *sql.DB, now func() time.Time) Repository {
	repo := NewRepository(db)
	if now != nil {
		repo.now = now
	}
	return repo
}

func (r Repository) TableEmpty(ctx context.Context) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

func (r Repository) Create(ctx context.Context, username, role, passwordHash string, disabled bool) error {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return err
	}
	if err := ValidateRole(role); err != nil {
		return err
	}
	if strings.TrimSpace(passwordHash) == "" {
		return errors.New("password hash is required")
	}

	now := r.now().Unix()
	_, err := r.db.ExecContext(ctx, `
INSERT INTO users(username, role, password_hash, disabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		username, role, passwordHash, boolToInt(disabled), now, now)
	return err
}

func (r Repository) GetWithHash(ctx context.Context, username string) (User, string, error) {
	user, hash, err := r.getWithHash(ctx, r.db, username, "invalid username or password")
	return user, hash, err
}

func (r Repository) List(ctx context.Context) ([]User, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT username, role, disabled, created_at, updated_at
FROM users
ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		var disabled int
		if err := rows.Scan(&user.Username, &user.Role, &disabled, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, err
		}
		user.Disabled = disabled != 0
		users = append(users, user)
	}
	return users, rows.Err()
}

func (r Repository) Patch(ctx context.Context, username string, patch Patch) (PatchResult, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return PatchResult{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PatchResult{}, err
	}
	defer tx.Rollback()

	current, _, err := r.getWithHash(ctx, tx, username, "user not found")
	if err != nil {
		return PatchResult{}, err
	}

	nextRole := current.Role
	if patch.Role != nil {
		if err := ValidateRole(*patch.Role); err != nil {
			return PatchResult{}, err
		}
		nextRole = *patch.Role
	}

	nextDisabled := current.Disabled
	if patch.Disabled != nil {
		nextDisabled = *patch.Disabled
	}

	if current.Role == RoleAdmin && (nextRole != RoleAdmin || nextDisabled) {
		count, err := r.enabledAdminCount(ctx, tx, username)
		if err != nil {
			return PatchResult{}, err
		}
		if count == 0 {
			return PatchResult{}, errors.New("cannot remove the last enabled admin")
		}
	}

	sets := []string{"role = ?", "disabled = ?", "updated_at = ?"}
	args := []any{nextRole, boolToInt(nextDisabled), r.now().Unix()}
	if patch.PasswordHashProvider != nil {
		passwordHash, err := patch.PasswordHashProvider()
		if err != nil {
			return PatchResult{}, err
		}
		if strings.TrimSpace(passwordHash) == "" {
			return PatchResult{}, errors.New("password hash is required")
		}
		sets = append(sets, "password_hash = ?")
		args = append(args, passwordHash)
	}
	args = append(args, username)

	if _, err := tx.ExecContext(ctx, "UPDATE users SET "+strings.Join(sets, ", ")+" WHERE username = ?", args...); err != nil {
		return PatchResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PatchResult{}, err
	}

	return PatchResult{
		InvalidateSessions: nextDisabled || nextRole != current.Role || patch.PasswordHashProvider != nil,
	}, nil
}

func (r Repository) Delete(ctx context.Context, username string) error {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	current, _, err := r.getWithHash(ctx, tx, username, "user not found")
	if err != nil {
		return err
	}
	if current.Role == RoleAdmin {
		count, err := r.enabledAdminCount(ctx, tx, username)
		if err != nil {
			return err
		}
		if count == 0 {
			return errors.New("cannot delete the last enabled admin")
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username); err != nil {
		return err
	}
	return tx.Commit()
}

type userQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r Repository) getWithHash(ctx context.Context, q userQueryer, username, notFoundMessage string) (User, string, error) {
	var user User
	var hash string
	var disabled int
	err := q.QueryRowContext(ctx, `
SELECT username, role, password_hash, disabled, created_at, updated_at
FROM users
WHERE username = ?`, username).
		Scan(&user.Username, &user.Role, &hash, &disabled, &user.CreatedAt, &user.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", errors.New(notFoundMessage)
	}
	if err != nil {
		return User{}, "", err
	}
	user.Disabled = disabled != 0
	return user, hash, nil
}

func (r Repository) enabledAdminCount(ctx context.Context, tx *sql.Tx, excludingUsername string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM users
WHERE role = ? AND disabled = 0 AND username <> ?`, RoleAdmin, excludingUsername).Scan(&count)
	return count, err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
