import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { badge, el, escapeAttr, escapeHTML, getFormInput } from "../dom.js";
import { t } from "../i18n.js";
import { isMember } from "../session.js";
import { state } from "../state.js";
import type { RouteRecord } from "../types.js";

interface RoutesResponse {
  routes?: RouteRecord[];
}

export async function loadRoutes(): Promise<void> {
  try {
    const q = encodeURIComponent(el<HTMLInputElement>("routeSearch").value || "");
    const data = await api<RoutesResponse>(q ? `/routes?q=${q}` : "/routes");
    state.routes = data.routes || [];
    renderRoutes();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

export function renderRoutes(): void {
  const canWrite = isMember();
  el("routesBody").innerHTML = state.routes.map((route) => `
    <tr>
      <td>${escapeHTML(route.host)}</td>
      <td>${escapeHTML(route.upstream)}</td>
      <td>${badge(route.enabled ? t("enabled") : t("disabled"), !route.enabled)}</td>
      <td>${escapeHTML(route.note || "")}</td>
      <td class="actions">${canWrite ? routeActions(route) : ""}</td>
    </tr>
  `).join("");

  document.querySelectorAll<HTMLButtonElement>("[data-edit-route]").forEach((button) => {
    button.addEventListener("click", () => {
      const route = state.routes.find((item) => item.host === button.dataset.editRoute);
      openRouteDialog(route || null);
    });
  });
  document.querySelectorAll<HTMLButtonElement>("[data-delete-route]").forEach((button) => {
    button.addEventListener("click", () => {
      const host = button.dataset.deleteRoute;
      if (host) {
        removeRoute(host);
      }
    });
  });
}

export function openRouteDialog(route: RouteRecord | null = null): void {
  const form = el<HTMLFormElement>("routeForm");
  form.reset();
  form.dataset.originalHost = route ? route.host : "";
  getFormInput(form, "host").disabled = Boolean(route);
  if (route) {
    getFormInput(form, "host").value = route.host;
    getFormInput(form, "upstream").value = route.upstream;
    getFormInput(form, "enabled").checked = route.enabled;
    getFormInput(form, "note").value = route.note || "";
  } else {
    getFormInput(form, "enabled").checked = true;
  }
  el<HTMLDialogElement>("routeDialog").showModal();
}

export async function saveRoute(event: SubmitEvent): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  const host = form.dataset.originalHost || getFormInput(form, "host").value;
  try {
    await api(`/routes/${encodeURIComponent(host)}`, {
      method: "PUT",
      body: {
        upstream: getFormInput(form, "upstream").value,
        enabled: getFormInput(form, "enabled").checked,
        note: getFormInput(form, "note").value,
      },
    });
    el<HTMLDialogElement>("routeDialog").close();
    await loadRoutes();
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function removeRoute(host: string): Promise<void> {
  if (host === "default" && !confirm(t("deleteDefaultRouteConfirm"))) {
    return;
  }
  try {
    await api(`/routes/${encodeURIComponent(host)}`, { method: "DELETE" });
    await loadRoutes();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function routeActions(route: RouteRecord): string {
  return `
    <div class="row-actions">
      <button class="secondary" type="button" data-edit-route="${escapeAttr(route.host)}">${escapeHTML(t("edit"))}</button>
      <button class="danger" type="button" data-delete-route="${escapeAttr(route.host)}">${escapeHTML(t("delete"))}</button>
    </div>
  `;
}
