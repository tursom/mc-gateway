import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { el, escapeHTML, stat } from "../dom.js";
import { t } from "../i18n.js";
import type { Metrics } from "../types.js";

export async function loadMetrics(): Promise<void> {
  try {
    const data = await api<Metrics>("/metrics");
    el("metricsGrid").innerHTML = [
      stat(t("total"), data.total_connections),
      stat(t("activeConnections"), data.active_connections),
      stat("TCP", data.tcp_connections),
      stat("WebSocket", data.websocket_connections),
      stat(t("misses"), data.route_misses),
      stat(t("dialErrors"), data.upstream_dial_errors),
    ].join("");
    const hits = data.route_hits || {};
    el("routeHits").innerHTML = Object.keys(hits).length
      ? Object.entries(hits).map(([host, count]) => `<span class="chip">${escapeHTML(host)}: ${count}</span>`).join("")
      : `<span class="chip">${escapeHTML(t("noHits"))}</span>`;
  } catch (err) {
    showAlert((err as Error).message);
  }
}
