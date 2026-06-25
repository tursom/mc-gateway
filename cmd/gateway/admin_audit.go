package main

import (
	"context"

	"github.com/tursom/mc-gateway/internal/adminaudit"
)

func recordAudit(ctx context.Context, actor, sourceIP, action, targetType, targetID string, success bool, message string) {
	_ = adminaudit.NewRepository(adminDB).Record(ctx, actor, sourceIP, action, targetType, targetID, success, message)
}

func listAuditLogs(ctx context.Context) ([]adminaudit.Record, error) {
	return adminaudit.NewRepository(adminDB).List(ctx, adminaudit.DefaultListLimit)
}
