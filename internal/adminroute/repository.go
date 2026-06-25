package adminroute

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type Record struct {
	Host      string `json:"host"`
	Upstream  string `json:"upstream"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

type Repository struct {
	db  *sql.DB
	now func() time.Time
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

func (r Repository) EnabledMap(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT host, upstream FROM routes WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	routes := make(map[string]string)
	for rows.Next() {
		var host, upstream string
		if err := rows.Scan(&host, &upstream); err != nil {
			return nil, err
		}
		routes[host] = upstream
	}
	return routes, rows.Err()
}

func (r Repository) List(ctx context.Context, query string) ([]Record, error) {
	sqlQuery := `
SELECT host, upstream, enabled, note, created_at, updated_at, updated_by
FROM routes`
	var args []any
	if query = strings.TrimSpace(query); query != "" {
		sqlQuery += ` WHERE host LIKE ? OR upstream LIKE ? OR note LIKE ?`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	sqlQuery += ` ORDER BY host`

	rows, err := r.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []Record
	for rows.Next() {
		var route Record
		var enabled int
		if err := rows.Scan(&route.Host, &route.Upstream, &enabled, &route.Note, &route.CreatedAt, &route.UpdatedAt, &route.UpdatedBy); err != nil {
			return nil, err
		}
		route.Enabled = enabled != 0
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (r Repository) Upsert(ctx context.Context, actor, host, upstream string, enabled bool, note string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateUpstream(upstream); err != nil {
		return err
	}

	now := r.now().Unix()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO routes(host, upstream, enabled, note, created_at, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(host) DO UPDATE SET
    upstream = excluded.upstream,
    enabled = excluded.enabled,
    note = excluded.note,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by`,
		host, upstream, boolToInt(enabled), note, now, now, actor); err != nil {
		return err
	}
	return tx.Commit()
}

func (r Repository) Delete(ctx context.Context, host string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM routes WHERE host = ?`, host); err != nil {
		return err
	}
	return tx.Commit()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
