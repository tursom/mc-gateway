// internal/adminaudit/audit.go 持久化 Admin API 变更产生的审计事件，并向管理界面提供查询。

package adminaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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
		data, err := json.Marshal(redactAuditValue(metadata, ""))
		if err != nil {
			return err
		}
		metadataJSON = string(data)
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO audit_logs(actor, source_ip, action, target_type, target_id, success, message, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actor, sourceIP, action, targetType, targetID, boolToInt(success), redactAuditText(message), metadataJSON, r.now().Unix())
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

func redactAuditValue(value any, key string) any {
	if isAuditSensitiveName(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, child := range typed {
			out[childKey] = redactAuditValue(child, childKey)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for childKey, child := range typed {
			if isAuditSensitiveName(childKey) {
				out[childKey] = "[REDACTED]"
			} else {
				out[childKey] = redactAuditText(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactAuditValue(child, key)
		}
		return out
	case string:
		return redactAuditText(typed)
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return value
		}
		return redactAuditValue(decoded, key)
	}
}

func redactAuditText(text string) string {
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, marker := range []string{"secret", "token", "password", "credential", "authorization", "session", "packet"} {
		if strings.Contains(lower, marker) {
			return "[REDACTED]"
		}
	}
	return text
}

func isAuditSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"secret", "token", "password", "credential", "authorization"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
