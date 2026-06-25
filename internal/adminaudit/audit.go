package adminaudit

import (
	"context"
	"database/sql"
	"time"
)

const DefaultListLimit = 200

type Record struct {
	ID         int64  `json:"id"`
	Actor      string `json:"actor"`
	SourceIP   string `json:"source_ip"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	CreatedAt  int64  `json:"created_at"`
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

func (r Repository) Record(ctx context.Context, actor, sourceIP, action, targetType, targetID string, success bool, message string) error {
	if r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO audit_logs(actor, source_ip, action, target_type, target_id, success, message, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		actor, sourceIP, action, targetType, targetID, boolToInt(success), message, r.now().Unix())
	return err
}

func (r Repository) List(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, actor, source_ip, action, target_type, target_id, success, message, created_at
FROM audit_logs
ORDER BY id DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []Record
	for rows.Next() {
		var item Record
		var success int
		if err := rows.Scan(&item.ID, &item.Actor, &item.SourceIP, &item.Action, &item.TargetType, &item.TargetID, &success, &item.Message, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Success = success != 0
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
