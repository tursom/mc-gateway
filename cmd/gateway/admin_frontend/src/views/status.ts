// cmd/gateway/admin_frontend/src/views/status.ts 渲染管理面板上的网关健康摘要。

import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { el, stat } from "../dom.js";
import { t } from "../i18n.js";

interface StatusResponse {
  pid?: number;
  uptime_seconds?: number;
  db_path?: string;
  tcp_admin_port?: number;
}

export async function loadStatus(): Promise<void> {
  try {
    const status = await api<StatusResponse>("/status");
    el("statusGrid").innerHTML = [
      stat("PID", status.pid),
      stat(t("uptime"), `${status.uptime_seconds}s`),
      stat("SQLite", status.db_path),
      stat("TCP/Admin", status.tcp_admin_port),
    ].join("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}
