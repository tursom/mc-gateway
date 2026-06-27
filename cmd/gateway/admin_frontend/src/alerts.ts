// cmd/gateway/admin_frontend/src/alerts.ts 集中处理告警展示，让异步界面流程可以一致地清空或显示错误。

import { el } from "./dom.js";
import { localizeMessage } from "./i18n.js";

export function showAlert(message: unknown): void {
  const box = el("alert");
  box.textContent = localizeMessage(message);
  box.classList.toggle("hidden", !message);
}
