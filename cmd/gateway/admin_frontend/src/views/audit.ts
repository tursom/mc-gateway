// cmd/gateway/admin_frontend/src/views/audit.ts 渲染管理员用于复核运行态变更的审计日志。

import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { badge, el, escapeHTML } from "../dom.js";
import { formatAuditAction, formatTargetType, localizeMessage, t } from "../i18n.js";
import type { AuditLog } from "../types.js";

interface AuditResponse {
  audit_logs?: AuditLog[];
}

export async function loadAudit(): Promise<void> {
  try {
    const data = await api<AuditResponse>("/audit-logs");
    el("auditBody").innerHTML = (data.audit_logs || []).map((item) => `
      <tr>
        <td>${new Date(item.created_at * 1000).toLocaleString()}</td>
        <td>${escapeHTML(item.actor)}</td>
        <td>${escapeHTML(formatAuditAction(item.action))}</td>
        <td>${escapeHTML(formatTargetType(item.target_type))}:${escapeHTML(item.target_id)}</td>
        <td>${badge(item.success ? t("success") : t("failed"), !item.success)}</td>
        <td>${escapeHTML(localizeMessage(item.message || ""))}</td>
      </tr>
    `).join("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}
