// cmd/gateway/admin_audit.go 把 HTTP 请求上下文转换为持久化审计记录，用于追踪管理端变更。

package main

import (
	"context"

	"github.com/tursom/mc-gateway/internal/adminaudit"
)

func recordAudit(ctx context.Context, actor, sourceIP, action, targetType, targetID string, success bool, message string) {
	_ = adminaudit.NewRepository(adminDB).Record(ctx, actor, sourceIP, action, targetType, targetID, success, message)
}

func recordAuditMetadata(ctx context.Context, actor, sourceIP, action, targetType, targetID string, success bool, message string, metadata any) {
	_ = adminaudit.NewRepository(adminDB).RecordWithMetadata(ctx, actor, sourceIP, action, targetType, targetID, success, message, metadata)
}

func listAuditLogs(ctx context.Context) ([]adminaudit.Record, error) {
	return adminaudit.NewRepository(adminDB).List(ctx, adminaudit.DefaultListLimit)
}
