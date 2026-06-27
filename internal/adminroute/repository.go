// internal/adminroute/repository.go 持久化 Minecraft 主机路由记录，并为在线网关返回有序快照。

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
	db *sql.DB
	// now 可在测试中注入固定时间，避免断言依赖真实时钟。
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
	// 只读取启用路由，结果直接用于连接热路径的内存快照。
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
		// 管理端搜索同时覆盖 host、upstream 和 note，便于按服务名或备注定位路由。
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
	// 写入前统一校验，避免无效 host/upstream 进入 SQLite 后再被热路径读取。
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

	// host 是主键；重复保存时只更新可变字段并保留 created_at。
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
	// 删除同样校验 host，防止管理端路径参数中的非法值直接进入 SQL。
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
