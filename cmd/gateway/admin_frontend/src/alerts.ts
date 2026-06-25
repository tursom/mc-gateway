import { el } from "./dom.js";
import { localizeMessage } from "./i18n.js";

export function showAlert(message: unknown): void {
  const box = el("alert");
  box.textContent = localizeMessage(message);
  box.classList.toggle("hidden", !message);
}
