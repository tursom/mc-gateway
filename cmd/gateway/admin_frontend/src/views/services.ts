import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { el, escapeAttr, escapeHTML, getFormInput } from "../dom.js";
import { t } from "../i18n.js";
import { isAdmin } from "../session.js";
import { state } from "../state.js";
import type { ServiceRecord } from "../types.js";

interface ServicesResponse {
  services?: ServiceRecord[];
}

const serviceNames: Record<string, string> = {
  tcp_admin: "TCP/Admin",
  kcp: "KCP",
  quic: "QUIC",
  websocket: "WebSocket",
};

export async function loadServices(): Promise<void> {
  try {
    const data = await api<ServicesResponse>("/services");
    state.services = data.services || [];
    renderServices();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

export function renderServices(): void {
  el("servicesGrid").innerHTML = state.services.map((service) => `
    <article class="service">
      <div>
        <span>${escapeHTML(formatServiceName(service.name))}</span>
        <strong>${serviceStatusText(service)}</strong>
      </div>
      ${isAdmin() ? serviceForm(service) : serviceSummary(service)}
    </article>
  `).join("");

  document.querySelectorAll<HTMLFormElement>("[data-service-form]").forEach((form) => {
    form.addEventListener("submit", saveService);
  });
  document.querySelectorAll<HTMLButtonElement>("[data-restart-service]").forEach((button) => {
    button.addEventListener("click", () => {
      const name = button.dataset.restartService;
      if (name) {
        restartService(name);
      }
    });
  });
}

function serviceSummary(service: ServiceRecord): string {
  return `<div><span>${escapeHTML(t("port"))}</span><strong>${service.port}</strong></div>`;
}

function serviceForm(service: ServiceRecord): string {
  const disabled = service.name === "tcp_admin" ? "disabled" : "";
  const optionFields = serviceOptionFields(service);
  return `
    <form data-service-form="${escapeAttr(service.name)}">
      <label class="inline">
        <input name="enabled" type="checkbox" ${service.enabled ? "checked" : ""} ${disabled}>
        ${escapeHTML(t("enabled"))}
      </label>
      <label>
        ${escapeHTML(t("port"))}
        <input name="port" type="number" min="1" max="65535" value="${service.port}">
      </label>
      ${optionFields}
      <div class="row-actions">
        <button type="submit">${escapeHTML(t("save"))}</button>
        <button class="secondary" type="button" data-restart-service="${escapeAttr(service.name)}">${escapeHTML(t("restart"))}</button>
      </div>
    </form>
  `;
}

function serviceOptionFields(service: ServiceRecord): string {
  const options = service.options || {};
  if (service.name === "kcp") {
    return `
      <label>${escapeHTML(t("dataShards"))}<input name="data_shards" type="number" min="1" value="${numberOption(options, "data_shards", 10)}"></label>
      <label>${escapeHTML(t("parityShards"))}<input name="parity_shards" type="number" min="1" value="${numberOption(options, "parity_shards", 3)}"></label>
    `;
  }
  if (service.name === "quic") {
    const protocols = arrayOption(options, "application_protocols").join(",");
    return `<label>${escapeHTML(t("protocols"))}<input name="application_protocols" value="${escapeAttr(protocols)}"></label>`;
  }
  if (service.name === "websocket") {
    return `<label>${escapeHTML(t("path"))}<input name="path" value="${escapeAttr(stringOption(options, "path", "/"))}"></label>`;
  }
  return "";
}

async function saveService(event: SubmitEvent): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  const name = form.dataset.serviceForm;
  if (!name) {
    return;
  }
  const options: Record<string, unknown> = {};
  if (name === "kcp") {
    options.data_shards = Number(getFormInput(form, "data_shards").value);
    options.parity_shards = Number(getFormInput(form, "parity_shards").value);
  } else if (name === "quic") {
    options.application_protocols = getFormInput(form, "application_protocols").value.split(",").map((item) => item.trim()).filter(Boolean);
  } else if (name === "websocket") {
    options.path = getFormInput(form, "path").value;
  }

  try {
    await api(`/services/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: {
        enabled: name === "tcp_admin" ? true : getFormInput(form, "enabled").checked,
        port: Number(getFormInput(form, "port").value),
        options,
      },
    });
    await loadServices();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function restartService(name: string): Promise<void> {
  try {
    await api(`/services/${encodeURIComponent(name)}/restart`, { method: "POST", body: {} });
    await loadServices();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function formatServiceName(name: string): string {
  return serviceNames[name] || name;
}

function serviceStatusText(service: ServiceRecord): string {
  const status = service.enabled ? t("enabled") : t("disabled");
  return escapeHTML(service.restart_required ? `${status} / ${t("restartRequired")}` : status);
}

function numberOption(options: Record<string, unknown>, key: string, fallback: number): number {
  const value = options[key];
  return typeof value === "number" ? value : fallback;
}

function stringOption(options: Record<string, unknown>, key: string, fallback: string): string {
  const value = options[key];
  return typeof value === "string" ? value : fallback;
}

function arrayOption(options: Record<string, unknown>, key: string): string[] {
  const value = options[key];
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}
