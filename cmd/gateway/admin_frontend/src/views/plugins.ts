import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { badge, el, escapeAttr, escapeHTML, getFormInput } from "../dom.js";
import { isAdmin } from "../session.js";
import { state } from "../state.js";
import type { PluginArtifact, PluginBuild, PluginDryRunResult, PluginInstrumentation, PluginOperations, PluginProxyConnection, PluginSecret, PluginServiceStatus, PluginSnapshot, PluginView } from "../types.js";

interface PluginsResponse {
  plugins?: PluginView[];
}

interface ArtifactsResponse {
  artifacts?: PluginArtifact[];
}

interface BuildsResponse {
  builds?: PluginBuild[];
}

interface PluginServiceResponse {
  plugin_service?: PluginServiceStatus;
}

interface InstrumentationResponse {
  instrumentation?: PluginInstrumentation[];
}

interface PluginResponse {
  plugin?: PluginView;
}

interface DryRunResponse {
  result?: PluginDryRunResult;
}

interface SnapshotDiffResponse {
  diff?: {
    redacted_diff_json?: string;
    sensitive_paths?: string[];
    restart_required?: boolean;
  };
}

interface OperationsResponse {
  operations?: PluginOperations;
  candidates?: Record<string, unknown>[];
  diagnostic?: Record<string, unknown>;
  summary?: Record<string, unknown>;
}

export async function loadPlugins(): Promise<void> {
  try {
    const [data, artifacts, builds, service, instrumentation] = await Promise.all([
      api<PluginsResponse>("/plugins"),
      api<ArtifactsResponse>("/plugin-artifacts"),
      api<BuildsResponse>("/plugin-builds"),
      api<PluginServiceResponse>("/plugin-service"),
      api<InstrumentationResponse>("/plugin-instrumentation"),
    ]);
    state.plugins = data.plugins || [];
    state.pluginArtifacts = artifacts.artifacts || [];
    state.pluginBuilds = builds.builds || [];
    state.pluginService = service.plugin_service || null;
    state.pluginInstrumentation = instrumentation.instrumentation || [];
    const firstPlugin = state.plugins[0];
    if (!state.selectedPluginID && firstPlugin) {
      state.selectedPluginID = firstPlugin.id;
    }
    renderPlugins();
    if (state.selectedPluginID) {
      await loadPluginDetail(state.selectedPluginID);
    } else {
      renderPluginDetail(null);
    }
  } catch (err) {
    showAlert((err as Error).message);
  }
}

export function renderPlugins(): void {
  const managed = new Set(state.plugins.map((plugin) => plugin.id));
  const unmanagedArtifacts = state.pluginArtifacts.filter((artifact) => !managed.has(artifact.plugin_id));
  renderPluginServicePanel();
  el("pluginsBody").innerHTML = state.plugins.map((plugin) => `
    <tr class="${plugin.id === state.selectedPluginID ? "selected" : ""}">
      <td><button class="link-button" type="button" data-plugin-detail="${escapeAttr(plugin.id)}">${escapeHTML(plugin.id)}</button></td>
      <td>${escapeHTML(plugin.name || "")}</td>
      <td>${escapeHTML(plugin.version || "")}</td>
      <td>${escapeHTML(plugin.artifact_type || "")}</td>
      <td>${escapeHTML(plugin.runtime_type || "")}</td>
      <td>${escapeHTML(plugin.desired_state)}</td>
      <td>${badge(plugin.runtime_state, plugin.runtime_state !== "enabled")}</td>
      <td>${escapeHTML(shortID(plugin.desired_artifact_id))}</td>
      <td>${escapeHTML((plugin.extension_points || []).join(", "))}</td>
      <td>${escapeHTML(String(plugin.priority))}</td>
      <td>${badge(plugin.restart_required ? "yes" : "no", !plugin.restart_required)}</td>
      <td>${badge(plugin.health || "", plugin.health !== "healthy")}</td>
    </tr>
  `).concat(unmanagedArtifacts.map((artifact) => `
    <tr class="${artifact.id === state.selectedArtifactID ? "selected" : ""}">
      <td><button class="link-button" type="button" data-artifact-detail="${escapeAttr(artifact.id)}">${escapeHTML(artifact.plugin_id)}</button></td>
      <td>${escapeHTML(artifact.file_name || "")}</td>
      <td>${escapeHTML(artifact.version || "")}</td>
      <td>${escapeHTML(artifact.artifact_type)}</td>
      <td>${escapeHTML(artifact.runtime_type || "")}</td>
      <td>${escapeHTML(artifact.status)}</td>
      <td>${badge("not managed", true)}</td>
      <td>${escapeHTML(shortID(artifact.id))}</td>
      <td>${escapeHTML(jsonList(artifact.extension_points_json))}</td>
      <td></td>
      <td>${badge("no", false)}</td>
      <td>${badge(artifact.error ? "error" : "pending", Boolean(artifact.error))}</td>
    </tr>
  `)).join("");
  document.querySelectorAll<HTMLButtonElement>("[data-plugin-detail]").forEach((button) => {
    button.addEventListener("click", () => {
      const id = button.dataset.pluginDetail;
      if (id) {
        state.selectedPluginID = id;
        state.selectedArtifactID = "";
        loadPluginDetail(id);
      }
    });
  });
  document.querySelectorAll<HTMLButtonElement>("[data-artifact-detail]").forEach((button) => {
    button.addEventListener("click", () => {
      state.selectedArtifactID = button.dataset.artifactDetail || "";
      state.selectedPluginID = "";
      renderPlugins();
      renderPluginDetail(null);
    });
  });
}

export async function loadPluginDetail(pluginID: string): Promise<void> {
  try {
    const data = await api<PluginResponse>(`/plugins/${encodeURIComponent(pluginID)}`);
    if (data.plugin) {
      state.plugins = state.plugins.map((plugin) => plugin.id === data.plugin?.id ? data.plugin : plugin);
      if (!state.plugins.some((plugin) => plugin.id === data.plugin?.id)) {
        state.plugins.push(data.plugin);
      }
      state.selectedPluginID = data.plugin.id;
      renderPlugins();
      renderPluginDetail(data.plugin);
    }
  } catch (err) {
    showAlert((err as Error).message);
  }
}

export function renderPluginDetail(plugin: PluginView | null = selectedPlugin()): void {
  const detail = el("pluginDetail");
  if (!plugin) {
    detail.innerHTML = uploadInventoryDetail();
    bindInventoryEvents();
    return;
  }
  const canWrite = isAdmin();
  detail.innerHTML = `
    <div class="detail-header">
      <div>
        <h2>${escapeHTML(plugin.name || plugin.id)}</h2>
        <p>${escapeHTML(plugin.id)} · ${escapeHTML(plugin.version || "")}</p>
      </div>
      <div class="row-actions">${canWrite ? pluginActionButtons(plugin) : ""}</div>
    </div>
    <div class="status-grid dense">
      ${detailStat("Runtime", plugin.runtime_state)}
      ${detailStat("Desired", plugin.desired_state)}
      ${detailStat("Active", shortID(plugin.active_artifact_id))}
      ${detailStat("Desired artifact", shortID(plugin.desired_artifact_id))}
      ${detailStat("Loaded", shortID(plugin.loaded_artifact_id))}
      ${detailStat("Restart", plugin.restart_required ? "required" : "not required")}
      ${detailStat("Health", plugin.health || "")}
      ${detailStat("Active proxy", plugin.active_proxy_connections || 0)}
    </div>
    ${plugin.last_error ? `<div class="alert inline-alert">${escapeHTML(plugin.last_error)}</div>` : ""}
    <div class="plugin-layout">
      <section class="panel">
        <h3>Manifest</h3>
        <dl class="kv">
          <dt>Runtime</dt><dd>${escapeHTML(plugin.runtime_type || "")}</dd>
          <dt>Extensions</dt><dd>${escapeHTML((plugin.extension_points || []).join(", "))}</dd>
          <dt>Scope</dt><dd>${escapeHTML(formatJSON(plugin.scope))}</dd>
          <dt>Rollout</dt><dd>${escapeHTML(formatJSON(plugin.rollout))}</dd>
          <dt>Minecraft</dt><dd>${escapeHTML(formatJSON(plugin.minecraft))}</dd>
        </dl>
      </section>
      <section class="panel">
        <h3>Governance</h3>
        ${governancePanel(plugin, canWrite)}
      </section>
      <section class="panel">
        <h3>Config</h3>
        <textarea id="pluginConfigEditor" ${canWrite ? "" : "readonly"}>${escapeHTML(prettyJSON(plugin.config_json || "{}"))}</textarea>
        <div class="row-actions">${canWrite ? `
          <button type="button" id="pluginDryRunBtn">Dry run</button>
          <button type="button" id="pluginSaveConfigBtn">Save config</button>
        ` : ""}</div>
        <pre id="pluginDryRunResult" class="log-output"></pre>
      </section>
      <section class="panel">
        <h3>Secrets</h3>
        <div class="chips">${(plugin.secrets || []).map(secretChip).join("") || `<span class="chip">No secrets</span>`}</div>
        ${canWrite ? `
          <form id="pluginSecretForm" class="inline-form">
            <input name="name" placeholder="secret name" required>
            <input name="value" type="password" placeholder="value" required>
            <label class="inline"><input name="reload_required" type="checkbox"> Reload required</label>
            <label class="inline"><input name="hot_reload" type="checkbox" checked> Hot reload</label>
            <button type="submit">Update secret</button>
          </form>
        ` : ""}
      </section>
      <section class="panel">
        <h3>Artifacts</h3>
        ${artifactList(plugin.artifacts || [], plugin)}
      </section>
      <section class="panel">
        <h3>Builds</h3>
        ${buildList(plugin.builds || [], plugin, canWrite)}
      </section>
      <section class="panel">
        <h3>Snapshots</h3>
        ${snapshotList(plugin.snapshots || [], canWrite)}
      </section>
      <section class="panel">
        <h3>Dispatch plan</h3>
        <pre class="log-output">${escapeHTML(formatJSON(plugin.dispatch_summary || []))}</pre>
      </section>
      <section class="panel">
        <h3>Extension status</h3>
        <pre class="log-output">${escapeHTML(formatJSON(plugin.extension_status || {}))}</pre>
      </section>
      <section class="panel">
        <h3>Operations</h3>
        <div class="row-actions">
          <button class="secondary" type="button" id="pluginOperationsLoadBtn">Refresh</button>
          <button class="secondary" type="button" id="pluginOperationsGCDryRunBtn">GC dry-run</button>
          <button class="secondary" type="button" id="pluginDiagnosticBtn">Diagnostic</button>
        </div>
        <pre id="pluginOperationsOutput" class="log-output"></pre>
      </section>
      <section class="panel">
        <h3>Active proxy connections</h3>
        ${proxyConnectionList(plugin.proxy_connections || [])}
      </section>
    </div>
  `;
  bindPluginDetailEvents(plugin);
}

export function bindPluginEvents(): void {
  el<HTMLInputElement>("pluginUploadInput").addEventListener("change", uploadPluginPackage);
  el<HTMLButtonElement>("refreshPluginsBtn").addEventListener("click", loadPlugins);
}

function renderPluginServicePanel(): void {
  const container = document.getElementById("pluginServicePanel");
  if (!container) {
    return;
  }
  const service = state.pluginService?.service;
  const canWrite = isAdmin();
  container.innerHTML = `
    <section class="panel">
      <div class="detail-header compact">
        <div>
          <h3>Plugin Service</h3>
          <p>${service ? `active ${escapeHTML(service.active_mode)} · desired ${escapeHTML(service.desired_mode)}` : "not loaded"}</p>
        </div>
        ${service ? badge(service.restart_required ? "restart required" : "applied", service.restart_required) : ""}
      </div>
      ${service ? `
        <div class="status-grid dense">
          ${detailStat("Desired mode", service.desired_mode)}
          ${detailStat("Active mode", service.active_mode)}
          ${detailStat("Migration", service.live_migration || "drain-only")}
          ${detailStat("Restart", service.restart_required ? "required" : "not required")}
        </div>
        ${service.last_error ? `<div class="alert inline-alert">${escapeHTML(service.last_error)}</div>` : ""}
        ${canWrite ? `
          <form id="pluginServiceForm" class="inline-form">
            <select name="desired_mode">
              ${["in-process", "go-plugin-process", "sandbox-process"].map((mode) => `<option value="${mode}" ${mode === service.desired_mode ? "selected" : ""}>${mode}</option>`).join("")}
            </select>
            <button type="submit">Set desired</button>
          </form>
        ` : ""}
      ` : ""}
    </section>
    <section class="panel">
      <h3>Build-Time Instrumentation</h3>
      ${instrumentationList(state.pluginInstrumentation)}
    </section>
  `;
  const form = document.getElementById("pluginServiceForm");
  if (form instanceof HTMLFormElement) {
    form.addEventListener("submit", updatePluginServiceMode);
  }
}

async function updatePluginServiceMode(event: Event): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  try {
    await api("/plugin-service", {
      method: "PUT",
      body: { desired_mode: getFormInput(form, "desired_mode") },
    });
    await loadPlugins();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function instrumentationList(records: PluginInstrumentation[]): string {
  if (!records.length) {
    return `<p class="muted">No instrumentation metadata</p>`;
  }
  return `<table class="mini-table">
    <thead><tr><th>Name</th><th>Profile</th><th>Status</th><th>Diff</th><th>Rollback</th></tr></thead>
    <tbody>${records.map((record) => `
      <tr>
        <td>${escapeHTML(record.name)} ${escapeHTML(record.version || "")}</td>
        <td>${escapeHTML(record.profile || "")}</td>
        <td>${badge(record.status || "available", record.status === "blocked")}</td>
        <td>${escapeHTML(shortID(record.generated_diff_hash || ""))}</td>
        <td>${escapeHTML(record.runbook_rollback || "")}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

async function uploadPluginPackage(event: Event): Promise<void> {
  const input = event.currentTarget as HTMLInputElement;
  const file = input.files?.[0];
  if (!file) {
    return;
  }
  const formData = new FormData();
  formData.set("artifact", file);
  try {
    await api("/plugin-artifacts", { method: "POST", formData });
    input.value = "";
    await loadPlugins();
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function bindPluginDetailEvents(plugin: PluginView): void {
  document.getElementById("pluginDryRunBtn")?.addEventListener("click", () => dryRunConfig(plugin));
  document.getElementById("pluginSaveConfigBtn")?.addEventListener("click", () => saveConfig(plugin));
  const secretForm = document.getElementById("pluginSecretForm");
  if (secretForm instanceof HTMLFormElement) {
    secretForm.addEventListener("submit", (event) => saveSecret(event, plugin));
  }
  document.querySelectorAll<HTMLButtonElement>("[data-plugin-action]").forEach((button) => {
    button.addEventListener("click", () => runPluginAction(plugin.id, button.dataset.pluginAction || ""));
  });
  document.querySelectorAll<HTMLButtonElement>("[data-artifact-rollback]").forEach((button) => {
    button.addEventListener("click", () => rollbackArtifact(plugin.id, button.dataset.artifactRollback || ""));
  });
  document.querySelectorAll<HTMLButtonElement>("[data-build-action]").forEach((button) => {
    button.addEventListener("click", () => runBuildAction(plugin.id, Number(button.dataset.buildId || "0"), button.dataset.buildAction || ""));
  });
  document.querySelectorAll<HTMLButtonElement>("[data-snapshot-rollback]").forEach((button) => {
    button.addEventListener("click", () => rollbackSnapshot(plugin.id, Number(button.dataset.snapshotRollback || "0"), false));
  });
  document.querySelectorAll<HTMLButtonElement>("[data-snapshot-full-rollback]").forEach((button) => {
    button.addEventListener("click", () => rollbackSnapshot(plugin.id, Number(button.dataset.snapshotFullRollback || "0"), true));
  });
  document.querySelectorAll<HTMLButtonElement>("[data-snapshot-diff]").forEach((button) => {
    button.addEventListener("click", () => showSnapshotDiff(plugin.id, Number(button.dataset.snapshotDiff || "0")));
  });
  document.getElementById("pluginGovernanceReviewBtn")?.addEventListener("click", () => createGovernanceReview(plugin));
  document.getElementById("pluginGovernanceOverrideBtn")?.addEventListener("click", () => createGovernanceOverride(plugin));
  document.getElementById("pluginGovernancePreflightBtn")?.addEventListener("click", () => runGovernancePreflight(plugin));
  document.getElementById("pluginGovernanceSelfTestBtn")?.addEventListener("click", () => runGovernanceSelfTest(plugin));
  document.getElementById("pluginGovernanceBenchmarkBtn")?.addEventListener("click", () => recordGovernanceBenchmark(plugin));
  document.getElementById("pluginGovernanceAdvisoryBtn")?.addEventListener("click", () => createArtifactRevokeAdvisory(plugin));
  document.getElementById("pluginOperationsLoadBtn")?.addEventListener("click", () => loadPluginOperations(plugin));
  document.getElementById("pluginOperationsGCDryRunBtn")?.addEventListener("click", () => dryRunOperationsGC(plugin));
  document.getElementById("pluginDiagnosticBtn")?.addEventListener("click", () => loadDiagnosticPackage(plugin));
}

async function dryRunConfig(plugin: PluginView): Promise<void> {
  try {
    const data = await api<DryRunResponse>(`/plugins/${encodeURIComponent(plugin.id)}/config/dry-run`, {
      method: "POST",
      body: {
        artifact_id: plugin.desired_artifact_id,
        config_json: configEditorValue(),
      },
    });
    el("pluginDryRunResult").textContent = formatJSON(data.result || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function saveConfig(plugin: PluginView): Promise<void> {
  try {
    await api(`/plugins/${encodeURIComponent(plugin.id)}/config`, {
      method: "PUT",
      body: {
        artifact_id: plugin.desired_artifact_id,
        desired_state: plugin.desired_state,
        priority: plugin.priority,
        config_json: configEditorValue(),
      },
    });
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function saveSecret(event: SubmitEvent, plugin: PluginView): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  try {
    await api(`/plugins/${encodeURIComponent(plugin.id)}/secrets`, {
      method: "POST",
      body: {
        name: getFormInput(form, "name").value,
        artifact_id: plugin.desired_artifact_id,
        value: getFormInput(form, "value").value,
        reload_required: getFormInput(form, "reload_required").checked,
        hot_reload: getFormInput(form, "hot_reload").checked,
      },
    });
    form.reset();
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function runPluginAction(pluginID: string, action: string): Promise<void> {
  if (!action) {
    return;
  }
  try {
    await api(`/plugins/${encodeURIComponent(pluginID)}/${action}`, { method: "POST", body: {} });
    if (action === "delete") {
      state.selectedPluginID = "";
      await loadPlugins();
    } else {
      await loadPluginDetail(pluginID);
    }
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function createDesiredFromArtifact(artifact: PluginArtifact): Promise<void> {
  try {
    await api(`/plugins/${encodeURIComponent(artifact.plugin_id)}`, {
      method: "PUT",
      body: {
        artifact_id: artifact.id,
        desired_state: "disabled",
        priority: Number(getFormInput(el<HTMLFormElement>("artifactDesiredForm"), "priority").value || "100"),
        config_json: el<HTMLTextAreaElement>("artifactConfigEditor").value || "{}",
      },
    });
    state.selectedArtifactID = "";
    state.selectedPluginID = artifact.plugin_id;
    await loadPlugins();
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function rollbackArtifact(pluginID: string, artifactID: string): Promise<void> {
  try {
    await api(`/plugins/${encodeURIComponent(pluginID)}/rollback/artifact`, {
      method: "POST",
      body: { artifact_id: artifactID },
    });
    await loadPluginDetail(pluginID);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function runBuildAction(pluginID: string, buildID: number, action: string): Promise<void> {
  if (!buildID || !action) {
    return;
  }
  try {
    await api(`/plugin-builds/${buildID}/${encodeURIComponent(action)}`, { method: "POST", body: {} });
    await loadPluginDetail(pluginID);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function rollbackSnapshot(pluginID: string, snapshotID: number, fullDesired: boolean): Promise<void> {
  if (!snapshotID) {
    return;
  }
  try {
    await api(`/plugins/${encodeURIComponent(pluginID)}/rollback/config`, {
      method: "POST",
      body: { snapshot_id: snapshotID, full_desired: fullDesired },
    });
    await loadPluginDetail(pluginID);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function showSnapshotDiff(pluginID: string, snapshotID: number): Promise<void> {
  if (!snapshotID) {
    return;
  }
  try {
    const data = await api<SnapshotDiffResponse>(`/plugins/${encodeURIComponent(pluginID)}/config/snapshots/${snapshotID}/diff`);
    el("pluginDryRunResult").textContent = formatJSON(data.diff || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function createGovernanceReview(plugin: PluginView): Promise<void> {
  try {
    await api(`/plugins/${encodeURIComponent(plugin.id)}/governance/review`, {
      method: "POST",
      body: { artifact_id: plugin.desired_artifact_id, profile: "prod", decision: "approved" },
    });
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function createGovernanceOverride(plugin: PluginView): Promise<void> {
  const reason = window.prompt("Reason");
  if (!reason) {
    return;
  }
  try {
    await api(`/plugins/${encodeURIComponent(plugin.id)}/governance/override`, {
      method: "POST",
      body: { artifact_id: plugin.desired_artifact_id, profile: "prod", action: "enable", reason, ttl_seconds: 3600 },
    });
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function runGovernancePreflight(plugin: PluginView): Promise<void> {
  try {
    const data = await api<Record<string, unknown>>(`/plugins/${encodeURIComponent(plugin.id)}/governance/preflight`, {
      method: "POST",
      body: { artifact_id: plugin.desired_artifact_id, config_json: configEditorValue() },
    });
    el("pluginDryRunResult").textContent = formatJSON(data);
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function runGovernanceSelfTest(plugin: PluginView): Promise<void> {
  try {
    const data = await api<Record<string, unknown>>(`/plugins/${encodeURIComponent(plugin.id)}/governance/self-test`, {
      method: "POST",
      body: { artifact_id: plugin.desired_artifact_id },
    });
    el("pluginDryRunResult").textContent = formatJSON(data);
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function recordGovernanceBenchmark(plugin: PluginView): Promise<void> {
  const diff = Number(window.prompt("Baseline diff, e.g. 0.25", "0.25"));
  if (!Number.isFinite(diff)) {
    return;
  }
  try {
    await api(`/plugins/${encodeURIComponent(plugin.id)}/governance/benchmark`, {
      method: "POST",
      body: {
        artifact_id: plugin.desired_artifact_id,
        profile: "prod",
        benchmark_profile: "manual",
        p95_ms: 0,
        p99_ms: 0,
        error_rate: 0,
        active_proxy_capacity: 0,
        baseline_diff: diff,
      },
    });
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function createArtifactRevokeAdvisory(plugin: PluginView): Promise<void> {
  const artifact = plugin.desired_artifact;
  if (!artifact) {
    return;
  }
  const advisoryID = window.prompt("Advisory ID", `local-${shortID(artifact.sha256)}`);
  if (!advisoryID) {
    return;
  }
  try {
    await api("/plugin-advisories", {
      method: "POST",
      body: {
        advisory_id: advisoryID,
        status: "revoked",
        action: "revoke",
        artifact_sha256: artifact.sha256,
        recommended_action: "rollback or upgrade",
      },
    });
    await loadPluginDetail(plugin.id);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function loadPluginOperations(plugin: PluginView): Promise<void> {
  try {
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations`);
    el("pluginOperationsOutput").textContent = formatJSON(data.operations || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function dryRunOperationsGC(plugin: PluginView): Promise<void> {
  try {
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations/gc`);
    el("pluginOperationsOutput").textContent = formatJSON(data);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function loadDiagnosticPackage(plugin: PluginView): Promise<void> {
  try {
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations/diagnostic`);
    el("pluginOperationsOutput").textContent = formatJSON(data);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function uploadInventoryDetail(): string {
  const artifact = selectedArtifact();
  if (!artifact) {
    return `
      <div class="plugin-layout">
        <section class="panel">
          <h3>Uploaded artifacts</h3>
          ${artifactInventoryList(state.pluginArtifacts)}
        </section>
        <section class="panel">
          <h3>Builds</h3>
          ${buildInventoryList(state.pluginBuilds)}
        </section>
      </div>
    `;
  }
  const canWrite = isAdmin() && artifact.artifact_type === "binary";
  return `
    <div class="detail-header">
      <div>
        <h2>${escapeHTML(artifact.plugin_id)}</h2>
        <p>${escapeHTML(artifact.version)} · ${escapeHTML(artifact.artifact_type)} · ${escapeHTML(shortID(artifact.id))}</p>
      </div>
    </div>
    <div class="plugin-layout">
      <section class="panel">
        <h3>Artifact</h3>
        <dl class="kv">
          <dt>Status</dt><dd>${escapeHTML(artifact.status)}</dd>
          <dt>Runtime</dt><dd>${escapeHTML(artifact.runtime_type || "")}</dd>
          <dt>Go/API</dt><dd>${escapeHTML(`${artifact.go_version || ""} ${artifact.api_version || ""}`)}</dd>
          <dt>SHA256</dt><dd>${escapeHTML(artifact.sha256)}</dd>
          <dt>Extensions</dt><dd>${escapeHTML(jsonList(artifact.extension_points_json))}</dd>
          <dt>Error</dt><dd>${escapeHTML(artifact.error || "")}</dd>
        </dl>
      </section>
      <section class="panel">
        <h3>Create desired state</h3>
        ${canWrite ? `
          <form id="artifactDesiredForm" class="inline-form">
            <label>Priority<input name="priority" type="number" value="100"></label>
            <textarea id="artifactConfigEditor">{}</textarea>
            <button type="submit">Create disabled plugin</button>
          </form>
        ` : `<div class="empty">Only binary artifacts can be used as plugin desired state.</div>`}
      </section>
      <section class="panel">
        <h3>Builds</h3>
        ${buildInventoryList(state.pluginBuilds.filter((build) => build.plugin_id === artifact.plugin_id))}
      </section>
    </div>
  `;
}

function bindInventoryEvents(): void {
  const artifact = selectedArtifact();
  const form = document.getElementById("artifactDesiredForm");
  if (artifact && form instanceof HTMLFormElement) {
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      createDesiredFromArtifact(artifact);
    });
  }
  document.querySelectorAll<HTMLButtonElement>("[data-artifact-detail]").forEach((button) => {
    button.addEventListener("click", () => {
      state.selectedArtifactID = button.dataset.artifactDetail || "";
      renderPlugins();
      renderPluginDetail(null);
    });
  });
}

function artifactInventoryList(artifacts: PluginArtifact[]): string {
  if (artifacts.length === 0) {
    return `<div class="empty">No artifacts</div>`;
  }
  return `<div class="mini-list">${artifacts.map((artifact) => `
    <div class="mini-row">
      <span>${escapeHTML(artifact.plugin_id)}</span>
      <span>${escapeHTML(artifact.version)} · ${escapeHTML(artifact.artifact_type)} · ${escapeHTML(artifact.status)}</span>
      <span>${escapeHTML(shortID(artifact.id))}</span>
      <button class="secondary" type="button" data-artifact-detail="${escapeAttr(artifact.id)}">Open</button>
    </div>
  `).join("")}</div>`;
}

function buildInventoryList(builds: PluginBuild[]): string {
  if (builds.length === 0) {
    return `<div class="empty">No builds</div>`;
  }
  return `<div class="mini-list">${builds.map((build) => `
    <div class="mini-row">
      <span>${escapeHTML(build.plugin_id)} #${escapeHTML(build.id)}</span>
      <span>${badge(build.status, build.status !== "succeeded")}</span>
      <span>${escapeHTML(shortID(build.artifact_id || build.source_id))}</span>
      <span>${escapeHTML(build.log_summary || build.error || "")}</span>
    </div>
  `).join("")}</div>`;
}

function selectedPlugin(): PluginView | null {
  return state.plugins.find((plugin) => plugin.id === state.selectedPluginID) || null;
}

function selectedArtifact(): PluginArtifact | null {
  return state.pluginArtifacts.find((artifact) => artifact.id === state.selectedArtifactID) || null;
}

function pluginActionButtons(plugin: PluginView): string {
  return `
    <button type="button" data-plugin-action="load">Load</button>
    <button type="button" data-plugin-action="enable">Enable</button>
    <button class="secondary" type="button" data-plugin-action="disable">Disable</button>
    <button class="danger" type="button" data-plugin-action="delete">Delete</button>
  `;
}

function governancePanel(plugin: PluginView, canWrite: boolean): string {
  if (plugin.governance_error) {
    return `<div class="alert inline-alert">${escapeHTML(plugin.governance_error)}</div>`;
  }
  const governance = plugin.governance;
  const decision = governance?.decision;
  const issues = decision?.issues || [];
  return `
    <dl class="kv">
      <dt>Profile</dt><dd>${escapeHTML(decision?.profile || "")}</dd>
      <dt>Risk</dt><dd>${escapeHTML(decision?.risk_level || "")}</dd>
      <dt>Policy</dt><dd>${escapeHTML(shortID(decision?.policy_hash || ""))}</dd>
      <dt>Decision</dt><dd>${badge(decision?.ok ? "allowed" : "blocked", !decision?.ok)}</dd>
      <dt>Review</dt><dd>${badge(decision?.review_required ? "required" : "not required", Boolean(decision?.review_required))}</dd>
      <dt>Override</dt><dd>${badge(decision?.warning_override_used ? "used" : "not used", Boolean(decision?.warning_override_used))}</dd>
    </dl>
    ${issues.length ? `<div class="mini-list">${issues.map((issue) => `
      <div class="mini-row">
        <span>${badge(issue.severity, issue.severity !== "info")}</span>
        <span>${escapeHTML(issue.code)}</span>
        <span>${escapeHTML(issue.message)}</span>
      </div>
    `).join("")}</div>` : `<div class="empty">No governance issues</div>`}
    ${canWrite ? `
      <div class="row-actions">
        <button class="secondary" type="button" id="pluginGovernanceReviewBtn">Review</button>
        <button class="secondary" type="button" id="pluginGovernanceOverrideBtn">Override</button>
        <button class="secondary" type="button" id="pluginGovernancePreflightBtn">Preflight</button>
        <button class="secondary" type="button" id="pluginGovernanceSelfTestBtn">Self-test</button>
        <button class="secondary" type="button" id="pluginGovernanceBenchmarkBtn">Benchmark</button>
        <button class="danger" type="button" id="pluginGovernanceAdvisoryBtn">Revoke artifact</button>
      </div>
    ` : ""}
    <pre class="log-output">${escapeHTML(formatJSON({
      conflicts: governance?.conflicts,
      reviews: governance?.reviews || [],
      warning_overrides: governance?.warning_overrides || [],
      preflights: governance?.preflights || [],
      benchmarks: governance?.benchmarks || [],
      advisories: governance?.advisories || [],
    }))}</pre>
  `;
}

function artifactList(artifacts: PluginArtifact[], plugin: PluginView): string {
  if (artifacts.length === 0) {
    return `<div class="empty">No artifacts</div>`;
  }
  return `<div class="mini-list">${artifacts.map((artifact) => `
    <div class="mini-row">
      <span>${escapeHTML(shortID(artifact.id))}</span>
      <span>${escapeHTML(artifact.version)} · ${escapeHTML(artifact.artifact_type)} · ${escapeHTML(artifact.status)}</span>
      <span>${escapeHTML(artifact.go_version || "")}</span>
      ${isAdmin() && artifact.artifact_type === "binary" && artifact.id !== plugin.desired_artifact_id ? `<button class="secondary" type="button" data-artifact-rollback="${escapeAttr(artifact.id)}">Rollback</button>` : ""}
    </div>
  `).join("")}</div>`;
}

function buildList(builds: PluginBuild[], plugin: PluginView, canWrite: boolean): string {
  if (builds.length === 0) {
    return `<div class="empty">No builds</div>`;
  }
  return `<div class="mini-list">${builds.map((build) => `
    <div class="mini-row">
      <span>#${escapeHTML(build.id)}</span>
      <span>${badge(build.status, build.status !== "succeeded")}</span>
      <span>${escapeHTML(shortID(build.artifact_id || build.source_id))}</span>
      <span>${escapeHTML(build.log_summary || build.error || "")}</span>
      ${canWrite ? buildActions(build, plugin) : ""}
    </div>
  `).join("")}</div>`;
}

function buildActions(build: PluginBuild, plugin: PluginView): string {
  if (build.status === "queued") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="run">Run</button>`;
  }
  if (build.status === "running") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="cancel">Cancel</button>`;
  }
  if (build.status === "failed" || build.status === "canceled") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="retry">Retry</button>`;
  }
  if (build.status === "succeeded" && build.artifact_id && build.artifact_id !== plugin.desired_artifact_id) {
    return `<button class="secondary" type="button" data-artifact-rollback="${escapeAttr(build.artifact_id)}">Use artifact</button>`;
  }
  return "";
}

function snapshotList(snapshots: PluginSnapshot[], canWrite: boolean): string {
  if (snapshots.length === 0) {
    return `<div class="empty">No snapshots</div>`;
  }
  return `<div class="mini-list">${snapshots.map((snapshot) => `
    <div class="mini-row">
      <span>#${escapeHTML(snapshot.id)}</span>
      <span>gen ${escapeHTML(snapshot.desired_generation)} · ${escapeHTML(snapshot.desired_state)} · ${escapeHTML(shortID(snapshot.artifact_id))}</span>
      <button class="secondary" type="button" data-snapshot-diff="${escapeAttr(snapshot.id)}">Diff</button>
      ${canWrite ? `
        <button class="secondary" type="button" data-snapshot-rollback="${escapeAttr(snapshot.id)}">Config rollback</button>
        <button class="secondary" type="button" data-snapshot-full-rollback="${escapeAttr(snapshot.id)}">Full rollback</button>
      ` : ""}
    </div>
  `).join("")}</div>`;
}

function proxyConnectionList(connections: PluginProxyConnection[]): string {
  if (connections.length === 0) {
    return `<div class="empty">No active proxy connections</div>`;
  }
  return `<div class="mini-list">${connections.map((conn) => `
    <div class="mini-row">
      <span>#${escapeHTML(conn.id)}</span>
      <span>${escapeHTML(shortID(conn.artifact_id))} · ${escapeHTML(conn.handler_id)}</span>
      <span>${escapeHTML(conn.duration_ms)}ms</span>
      <span>${badge(conn.draining ? "draining" : "active", conn.draining)}</span>
    </div>
  `).join("")}</div>`;
}

function secretChip(secret: PluginSecret): string {
  const reload = secret.reload_required ? "reload required" : "hot reload";
  return `<span class="chip">${escapeHTML(secret.name)} v${escapeHTML(secret.current_version)} · prev ${escapeHTML(secret.previous_version)} · ${escapeHTML(reload)}</span>`;
}

function detailStat(label: string, value: unknown): string {
  return `<div class="stat"><span>${escapeHTML(label)}</span><strong>${escapeHTML(String(value ?? ""))}</strong></div>`;
}

function configEditorValue(): string {
  return el<HTMLTextAreaElement>("pluginConfigEditor").value;
}

function shortID(id?: string): string {
  return id ? id.slice(0, 12) : "";
}

function prettyJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function jsonList(raw?: string): string {
  if (!raw) {
    return "";
  }
  try {
    const value = JSON.parse(raw);
    return Array.isArray(value) ? value.join(", ") : String(value);
  } catch {
    return raw;
  }
}

function formatJSON(value: unknown): string {
  try {
    return JSON.stringify(value ?? {}, null, 2);
  } catch {
    return String(value ?? "");
  }
}
