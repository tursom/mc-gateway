// internal/adminaudit/audit.go 持久化 Admin API 变更产生的审计事件，并向管理界面提供查询。

package adminaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const DefaultListLimit = 200

type Record struct {
	ID           int64  `json:"id"`
	Actor        string `json:"actor"`
	SourceIP     string `json:"source_ip"`
	Action       string `json:"action"`
	TargetType   string `json:"target_type"`
	TargetID     string `json:"target_id"`
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	MetadataJSON string `json:"metadata_json"`
	CreatedAt    int64  `json:"created_at"`
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
	return r.RecordWithMetadata(ctx, actor, sourceIP, action, targetType, targetID, success, message, nil)
}

func (r Repository) RecordWithMetadata(ctx context.Context, actor, sourceIP, action, targetType, targetID string, success bool, message string, metadata any) error {
	if r.db == nil {
		return nil
	}
	metadataJSON := "{}"
	if metadata != nil {
		data, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		metadataJSON = string(data)
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO audit_logs(actor, source_ip, action, target_type, target_id, success, message, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actor, sourceIP, action, targetType, targetID, boolToInt(success), message, metadataJSON, r.now().Unix())
	return err
}

func (r Repository) List(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, actor, source_ip, action, target_type, target_id, success, message, metadata_json, created_at
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
		if err := rows.Scan(&item.ID, &item.Actor, &item.SourceIP, &item.Action, &item.TargetType, &item.TargetID, &success, &item.Message, &item.MetadataJSON, &item.CreatedAt); err != nil {
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
