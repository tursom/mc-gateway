package adminservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

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

func (r Repository) EnsureDefaults(ctx context.Context, tcpAdminPort int) error {
	now := r.now().Unix()
	for _, service := range DefaultRecords(tcpAdminPort) {
		options, err := json.Marshal(service.Options)
		if err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `
INSERT INTO services(name, enabled, port, options_json, restart_required, created_at, updated_at)
VALUES (?, ?, ?, ?, 0, ?, ?)
ON CONFLICT(name) DO NOTHING`,
			service.Name, boolToInt(service.Enabled), service.Port, string(options), now, now); err != nil {
			return err
		}
	}

	return nil
}

func (r Repository) List(ctx context.Context) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT name, enabled, port, options_json, restart_required, created_at, updated_at, updated_by
FROM services
ORDER BY CASE name
    WHEN 'tcp_admin' THEN 0
    WHEN 'kcp' THEN 1
    WHEN 'quic' THEN 2
    WHEN 'websocket' THEN 3
    ELSE 4
END, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var services []Record
	for rows.Next() {
		var service Record
		var enabled, restartRequired int
		var optionsJSON string
		if err := rows.Scan(&service.Name, &enabled, &service.Port, &optionsJSON, &restartRequired, &service.CreatedAt, &service.UpdatedAt, &service.UpdatedBy); err != nil {
			return nil, err
		}
		service.Enabled = enabled != 0
		service.RestartRequired = restartRequired != 0
		service.Options = DecodeOptions(optionsJSON)
		services = append(services, service)
	}
	return services, rows.Err()
}

func (r Repository) Update(ctx context.Context, actor, name string, enabled bool, port int, options map[string]any) error {
	if err := ValidateUpdate(name, enabled, port); err != nil {
		return err
	}

	optionsJSON, err := json.Marshal(NormalizeOptions(name, options))
	if err != nil {
		return err
	}

	_, err = r.db.ExecContext(ctx, `
UPDATE services
SET enabled = ?, port = ?, options_json = ?, restart_required = 1, updated_at = ?, updated_by = ?
WHERE name = ?`,
		boolToInt(enabled), port, string(optionsJSON), r.now().Unix(), actor, name)
	return err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
