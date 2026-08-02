// cmd/gateway/admin_frontend/src/views/plugins.ts 渲染插件清单、插件详情、配置/密钥/治理动作和运维工具。

import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { badge, el, escapeAttr, escapeHTML, getFormInput, getFormSelect } from "../dom.js";
import { formatValue, localizeMessage, requiredText, ui, yesNo } from "../i18n.js";
import { isAdmin } from "../session.js";
import { state } from "../state.js";
import type { PluginArtifact, PluginBuild, PluginConnectionSession, PluginDryRunResult, PluginExtensionPointFeature, PluginFeatureFacts, PluginHostRuntimeSummary, PluginInstrumentation, PluginNodeRuntimeState, PluginNodeState, PluginOperations, PluginRuntimeAdapterStatus, PluginRuntimeFeature, PluginSecret, PluginServiceModeFeature, PluginServiceStatus, PluginSnapshot, PluginView, RepositoryUpdateReport } from "../types.js";

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

interface PluginFeaturesResponse {
  plugin_features?: PluginFeatureFacts;
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

interface RepositoryUpdatesResponse {
  updates?: RepositoryUpdateReport;
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
    // 插件页首屏依赖插件记录、制品、构建、插件服务模式和观测数据；
    // 并行请求可以减少进入页面时的等待时间。
    const [data, artifacts, builds, features, service, instrumentation] = await Promise.all([
      api<PluginsResponse>("/plugins"),
      api<ArtifactsResponse>("/plugin-artifacts"),
      api<BuildsResponse>("/plugin-builds"),
      api<PluginFeaturesResponse>("/plugin-features"),
      api<PluginServiceResponse>("/plugin-service"),
      api<InstrumentationResponse>("/plugin-instrumentation"),
    ]);
    state.plugins = data.plugins || [];
    state.pluginArtifacts = artifacts.artifacts || [];
    state.pluginBuilds = builds.builds || [];
    state.pluginFeatures = features.plugin_features || null;
    state.pluginService = service.plugin_service || null;
    state.pluginInstrumentation = instrumentation.instrumentation || [];
    const firstPlugin = state.plugins[0];
    if (!state.selectedPluginID && firstPlugin) {
      // 初次进入时默认选中第一个已纳管插件；未纳管制品会在列表中单独展示。
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
  // 未纳管制品还没有 plugins 表记录，但仍要展示，方便管理员创建期望状态。
  const unmanagedArtifacts = state.pluginArtifacts.filter((artifact) => !managed.has(artifact.plugin_id));
  renderPluginServicePanel();
  el("pluginsBody").innerHTML = state.plugins.map((plugin) => `
    <tr class="${plugin.id === state.selectedPluginID ? "selected" : ""}">
      <td><button class="link-button" type="button" data-plugin-detail="${escapeAttr(plugin.id)}">${escapeHTML(plugin.id)}</button></td>
      <td>${escapeHTML(plugin.name || "")}</td>
      <td>${escapeHTML(plugin.version || "")}</td>
      <td>${escapeHTML(formatValue(plugin.artifact_type || ""))}</td>
      <td>${escapeHTML(plugin.runtime_type || "")}</td>
      <td>${escapeHTML(formatValue(plugin.desired_state))}</td>
      <td>${badge(formatValue(plugin.runtime_state), plugin.runtime_state !== "enabled")}</td>
      <td>${escapeHTML(shortID(plugin.desired_artifact_id))}</td>
      <td>${escapeHTML((plugin.extension_points || []).join(", "))}</td>
      <td>${escapeHTML(String(plugin.priority))}</td>
      <td>${badge(yesNo(Boolean(plugin.restart_required)), !plugin.restart_required)}</td>
      <td>${badge(formatValue(plugin.health || ""), plugin.health !== "healthy")}</td>
    </tr>
  `).concat(unmanagedArtifacts.map((artifact) => `
    <tr class="${artifact.id === state.selectedArtifactID ? "selected" : ""}">
      <td><button class="link-button" type="button" data-artifact-detail="${escapeAttr(artifact.id)}">${escapeHTML(artifact.plugin_id)}</button></td>
      <td>${escapeHTML(artifact.file_name || "")}</td>
      <td>${escapeHTML(artifact.version || "")}</td>
      <td>${escapeHTML(formatValue(artifact.artifact_type))}</td>
      <td>${escapeHTML(artifact.runtime_type || "")}</td>
      <td>${escapeHTML(formatValue(artifact.status))}</td>
      <td>${badge(ui("not managed"), true)}</td>
      <td>${escapeHTML(shortID(artifact.id))}</td>
      <td>${escapeHTML(jsonList(artifact.extension_points_json))}</td>
      <td></td>
      <td>${badge(yesNo(false), false)}</td>
      <td>${badge(formatValue(artifact.error ? "error" : "pending"), Boolean(artifact.error))}</td>
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
      // 详情接口返回完整插件视图，用它回填列表中的摘要记录。
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
    // 没有选中纳管插件时展示制品库存和源码构建入口。
    detail.innerHTML = uploadInventoryDetail();
    bindInventoryEvents();
    return;
  }
  const canWrite = isAdmin();
  // 插件详情拆成多个小面板，避免配置、治理、构建和运维信息混成一个长表格。
  detail.innerHTML = `
    <div class="detail-header">
      <div>
        <h2>${escapeHTML(plugin.name || plugin.id)}</h2>
        <p>${escapeHTML(plugin.id)} · ${escapeHTML(plugin.version || "")}</p>
      </div>
      <div class="row-actions">${canWrite ? pluginActionButtons(plugin) : ""}</div>
    </div>
    <div class="status-grid dense">
      ${detailStat(ui("Runtime"), formatValue(plugin.runtime_state))}
      ${detailStat(ui("Desired"), formatValue(plugin.desired_state))}
      ${detailStat(ui("Active"), shortID(plugin.active_artifact_id))}
      ${detailStat(ui("Desired artifact"), shortID(plugin.desired_artifact_id))}
      ${detailStat(ui("Loaded"), shortID(plugin.loaded_artifact_id))}
      ${detailStat(ui("Restart"), requiredText(Boolean(plugin.restart_required)))}
      ${detailStat(ui("Health"), formatValue(plugin.health || ""))}
      ${detailStat(ui("Active sessions"), plugin.active_connection_sessions || 0)}
    </div>
    ${plugin.last_error ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(plugin.last_error))}</div>` : ""}
    <div class="plugin-layout">
      <section class="panel">
        <h3>${escapeHTML(ui("Manifest"))}</h3>
        <dl class="kv">
          <dt>${escapeHTML(ui("Artifact"))}</dt><dd>${artifactMaturityBadge(plugin.artifact_type)} ${escapeHTML(formatValue(plugin.artifact_type || ""))}</dd>
          <dt>${escapeHTML(ui("Runtime type"))}</dt><dd>${escapeHTML(plugin.runtime_type || "")}</dd>
          <dt>${escapeHTML(ui("Runtime maturity"))}</dt><dd>${runtimeMaturityBadge(plugin.runtime_type)}</dd>
          <dt>${escapeHTML(ui("ABI"))}</dt><dd>${escapeHTML(runtimeABI(plugin))}</dd>
          <dt>${escapeHTML(ui("Limits"))}</dt><dd>${escapeHTML(runtimeLimits(plugin))}</dd>
          <dt>${escapeHTML(ui("Supported extensions"))}</dt><dd>${escapeHTML(runtimeSupportedExtensions(plugin))}</dd>
          <dt>${escapeHTML(ui("Last error"))}</dt><dd>${escapeHTML(localizeMessage(runtimeLastError(plugin)))}</dd>
          <dt>${escapeHTML(ui("Extensions"))}</dt><dd>${escapeHTML((plugin.extension_points || []).join(", "))}</dd>
          <dt>${escapeHTML(ui("Scope"))}</dt><dd>${escapeHTML(formatJSON(plugin.scope))}</dd>
          <dt>${escapeHTML(ui("Rollout"))}</dt><dd>${escapeHTML(formatJSON(plugin.rollout))}</dd>
          <dt>${escapeHTML(ui("Minecraft"))}</dt><dd>${escapeHTML(formatJSON(plugin.minecraft))}</dd>
        </dl>
      </section>
      ${sandboxRuntimeDetailPanel(plugin)}
      <section class="panel">
        <h3>${escapeHTML(ui("Governance"))}</h3>
        ${governancePanel(plugin, canWrite)}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Rollout"))}</h3>
        ${rolloutPanel(plugin)}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Config"))}</h3>
        ${configMaturityPanel()}
        <textarea id="pluginConfigEditor" ${canWrite ? "" : "readonly"}>${escapeHTML(prettyJSON(plugin.config_json || "{}"))}</textarea>
        <div class="row-actions">${canWrite ? `
          <button type="button" id="pluginDryRunBtn">${escapeHTML(ui("Dry run"))}</button>
          <button type="button" id="pluginSaveConfigBtn">${escapeHTML(ui("Save config"))}</button>
        ` : ""}</div>
        <pre id="pluginDryRunResult" class="log-output"></pre>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Secrets"))}</h3>
        ${secretMaturityPanel(plugin)}
        <div class="chips">${(plugin.secrets || []).map(secretChip).join("") || `<span class="chip">${escapeHTML(ui("No secrets"))}</span>`}</div>
        ${canWrite ? `
          <form id="pluginSecretForm" class="inline-form">
            <input name="name" placeholder="${escapeAttr(ui("Secret name"))}" required>
            <input name="value" type="password" placeholder="${escapeAttr(ui("value"))}" required>
            <label class="inline"><input name="reload_required" type="checkbox"> ${escapeHTML(ui("Reload required"))}</label>
            <label class="inline"><input name="hot_reload" type="checkbox" checked> ${escapeHTML(ui("Hot reload"))}</label>
            <button type="submit">${escapeHTML(ui("Update secret"))}</button>
          </form>
        ` : ""}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Artifact"))}</h3>
        ${artifactList(plugin.artifacts || [], plugin)}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Repository"))}</h3>
        <form id="pluginRepositoryUpdatesForm" class="inline-form">
          <label>${escapeHTML(ui("Type"))}
            <select name="repository_type">
              <option value="file">${escapeHTML(ui("file"))}</option>
              <option value="url">${escapeHTML(ui("url"))}</option>
              <option value="official">${escapeHTML(ui("official"))}</option>
              <option value="internal">${escapeHTML(ui("internal"))}</option>
            </select>
          </label>
          <input name="index_path" placeholder="${escapeAttr(ui("Path"))}" required>
          <input name="artifact_id" placeholder="${escapeAttr(ui("Candidate ID"))}">
          <input name="version" placeholder="${escapeAttr(ui("version"))}">
          <button class="secondary" type="submit">${escapeHTML(ui("Check updates"))}</button>
        </form>
        <pre id="pluginRepositoryUpdatesOutput" class="log-output"></pre>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Builds"))}</h3>
        ${buildList(plugin.builds || [], plugin, canWrite)}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Snapshots"))}</h3>
        ${rollbackMaturityPanel()}
        ${snapshotList(plugin.snapshots || [], canWrite)}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Dispatch plan"))}</h3>
        ${canWrite ? `
          <div class="row-actions">
            <button class="secondary" type="button" id="pluginDispatchRefreshRoutesBtn">${escapeHTML(ui("Refresh routes"))}</button>
            <button class="secondary" type="button" id="pluginDispatchReplaySubscribersBtn">${escapeHTML(ui("Replay dead letters"))}</button>
            <button class="secondary" type="button" id="pluginDispatchDropSubscribersBtn">${escapeHTML(ui("Drop dead letters"))}</button>
          </div>
        ` : ""}
        <pre id="pluginDispatchOutput" class="log-output">${escapeHTML(formatJSON(plugin.dispatch_summary || []))}</pre>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Extension status"))}</h3>
        <pre class="log-output">${escapeHTML(formatJSON(plugin.extension_status || {}))}</pre>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Operations"))}</h3>
        <div class="row-actions">
          <button class="secondary" type="button" id="pluginOperationsLoadBtn">${escapeHTML(ui("Refresh"))}</button>
          <button class="secondary" type="button" id="pluginOperationsGCDryRunBtn">${escapeHTML(ui("GC dry-run"))}</button>
          <button class="secondary" type="button" id="pluginDiagnosticBtn">${escapeHTML(ui("Diagnostic"))}</button>
        </div>
        <pre id="pluginOperationsOutput" class="log-output"></pre>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Active connection sessions"))}</h3>
        ${connectionSessionList(plugin.connection_sessions || [])}
      </section>
    </div>
  `;
  bindPluginDetailEvents(plugin);
}

export function bindPluginEvents(): void {
  // 顶层插件页事件只绑定一次；详情区会在每次重绘后重新绑定动态按钮。
  el<HTMLInputElement>("pluginUploadInput").addEventListener("change", uploadPluginPackage);
  el<HTMLButtonElement>("refreshPluginsBtn").addEventListener("click", loadPlugins);
}

function renderPluginServicePanel(): void {
  const container = document.getElementById("pluginServicePanel");
  if (!container) {
    return;
  }
  const service = state.pluginService?.service;
  const modes = pluginServiceModes();
  const canWrite = isAdmin();
  const desiredMode = service ? serviceModeFeature(service.desired_mode, modes) : null;
  const activeMode = service ? serviceModeFeature(service.active_mode, modes) : null;
  // 插件服务模式决定当前数据面适配器；未来模式只允许保存为期望状态。
  container.innerHTML = `
    <section class="panel">
      <div class="detail-header compact">
        <div>
          <h3>${escapeHTML(ui("Plugin Service"))}</h3>
          <p>${service ? escapeHTML(pluginServiceSummary(service)) : escapeHTML(ui("not loaded"))}</p>
        </div>
        ${service ? pluginServiceStateBadge(service) : ""}
      </div>
      ${service ? `
        <div class="status-grid dense">
          ${detailStat(ui("Desired mode"), service.desired_mode)}
          ${detailStat(ui("Active mode"), service.active_mode)}
          ${detailStat(ui("Effective data plane"), pluginServiceEffectiveDataPlane(service))}
          ${detailStat(ui("Adapter"), service.implemented_adapter ? ui("implemented") : ui("not implemented"))}
          ${detailStat(ui("Desired maturity"), formatValue(service.desired_maturity || ""))}
          ${detailStat(ui("Desired support"), serviceModeSupportSummary(desiredMode))}
          ${detailStat(ui("Active support"), serviceModeSupportSummary(activeMode))}
          ${detailStat(ui("Migration"), service.live_migration || "drain-only")}
          ${detailStat(ui("Crash policy"), crashPolicySummary(service))}
          ${detailStat(ui("Restart"), requiredText(service.restart_required))}
        </div>
        ${pluginServiceStateAlerts(service, desiredMode)}
        ${service.unsupported_reason ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(service.unsupported_reason))}</div>` : ""}
        ${service.last_error && service.last_error !== service.unsupported_reason ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(service.last_error))}</div>` : ""}
        ${sandboxEnvironmentPanel(state.pluginService?.sandbox_environment)}
        ${serviceModeAvailability(modes)}
        ${extensionPointAvailability(pluginExtensionPoints())}
        ${runtimeAdapterAvailability(state.pluginService?.runtime_adapters || [])}
        ${pluginHostList(state.pluginService?.hosts || [])}
        ${pluginNodeList(state.pluginService?.nodes || [])}
        ${canWrite ? `
          <form id="pluginServiceForm" class="inline-form">
            <select name="desired_mode">
              ${modes.map((mode) => `<option value="${escapeAttr(mode.mode)}" ${mode.mode === service.desired_mode ? "selected" : ""}>${escapeHTML(serviceModeOptionLabel(mode))}</option>`).join("")}
            </select>
            <input name="crash_backoff_seconds" type="number" min="1" max="3600" step="1" value="${escapeAttr(String(service.crash_policy?.backoff_seconds || 30))}">
            <input name="crash_max_crashes" type="number" min="1" max="100" step="1" value="${escapeAttr(String(service.crash_policy?.max_crashes || 1))}">
            <input name="crash_window_seconds" type="number" min="1" max="86400" step="1" value="${escapeAttr(String(service.crash_policy?.window_seconds || 300))}">
            <button type="submit">${escapeHTML(ui("Set desired"))}</button>
          </form>
        ` : ""}
      ` : ""}
    </section>
    <section class="panel">
      <h3>${escapeHTML(ui("Build-Time Instrumentation"))}</h3>
      ${instrumentationList(state.pluginInstrumentation)}
    </section>
  `;
  const form = document.getElementById("pluginServiceForm");
  if (form instanceof HTMLFormElement) {
    form.addEventListener("submit", updatePluginServiceMode);
  }
}

function pluginNodeList(nodes: PluginNodeState[]): string {
  if (!nodes.length) {
    return "";
  }
  return `
    <div class="status-grid dense">
      ${nodes.map((node) => detailStat(
        node.node_id,
        `${formatValue(node.status || "unknown")} · ${node.data_plane_mode || node.service_mode || ""}${node.stale ? ` · ${ui("stale")}` : ""}`,
      )).join("")}
    </div>
  `;
}

function pluginServiceModes(): PluginServiceModeFeature[] {
  const modes = state.pluginFeatures?.service_modes || state.pluginService?.service_modes || [];
  if (modes.length) {
    return modes;
  }
  return [
    { mode: "in-process", implemented: true, maturity: "implemented", data_plane: true, requires_restart: false },
    { mode: "go-plugin-process", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, unsupported_reason: "go-plugin-process supports upstream.connect/v2 takeover with persisted crash policy and per-node crash isolation; fd-live migration, sandbox enforcement, full isolation, and non-Linux process-table orphan discovery are not implemented" },
    { mode: "sandbox-process", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, reason_code: "sandbox_data_plane_partial", unsupported_reason: "sandbox-process data-plane is partial; selected request/response extension points and upstream.connect/v2 takeover use the sandbox stream relay with drain-only lifecycle; broader isolation and governance hardening remain incomplete" },
  ];
}

function pluginExtensionPoints(): PluginExtensionPointFeature[] {
  const points = state.pluginFeatures?.extension_points || [];
  if (points.length) {
    return points;
  }
	  return [
	    { key: "admin.auth.provider/v1", type: "provider", implemented: false, maturity: "reserved", data_plane: false, requires_restart: false, unsupported_reason: "admin.auth.provider/v1 is reserved; local admin break-glass remains the implemented authentication path" },
	    { key: "ingress.service/v1", type: "service", implemented: false, maturity: "reserved", data_plane: false, requires_restart: false, unsupported_reason: "ingress.service/v1 is reserved; schema and governance checks exist but gateway-managed listener data-plane is not enabled" },
	  ];
}

function serviceModeFeature(modeName: string, modes: PluginServiceModeFeature[]): PluginServiceModeFeature | null {
  return modes.find((mode) => mode.mode === modeName) || null;
}

function pluginServiceSummary(service: NonNullable<PluginServiceStatus["service"]>): string {
  const dataPlane = pluginServiceEffectiveDataPlane(service);
  return `${ui("Effective data plane")} ${dataPlane} · ${ui("Active mode")} ${service.active_mode} · ${ui("Desired mode")} ${service.desired_mode}`;
}

function pluginServiceStateBadge(service: NonNullable<PluginServiceStatus["service"]>): string {
  if (service.unsupported_reason) {
    return badge(ui("reserved"), true);
  }
  if (service.data_plane_mode && service.data_plane_mode !== service.desired_mode) {
    return badge(ui("desired pending"), true);
  }
  if (service.restart_required) {
    return badge(ui("restart required"), true);
  }
  return badge(ui("applied"), false);
}

function crashPolicySummary(service: NonNullable<PluginServiceStatus["service"]>): string {
  return `${service.crash_policy?.max_crashes || 1} / ${service.crash_policy?.window_seconds || 300}s · ${ui("Backoff")} ${service.crash_policy?.backoff_seconds || 30}s`;
}

function pluginServiceEffectiveDataPlane(service: NonNullable<PluginServiceStatus["service"]>): string {
  const dataPlane = service.data_plane_mode || service.active_mode || "unknown";
  const env = state.pluginService?.sandbox_environment;
  if (dataPlane === "sandbox-process" && env && (!env.gate_enabled || !env.self_check_ok || !env.data_plane_eligible)) {
    return ui("blocked");
  }
  return dataPlane;
}

function serviceModeSupportSummary(mode: PluginServiceModeFeature | null): string {
  if (!mode) {
    return ui("unknown");
  }
  if (!mode.implemented || !mode.data_plane) {
    return `${formatValue(mode.maturity)} · ${ui("future desired only")} · ${ui("no current data plane")}`;
  }
  const dataPlane = mode.data_plane ? ui("data plane") : ui("no data plane");
  const restart = mode.requires_restart ? ui("restart required") : ui("hot");
  return `${formatValue(mode.maturity)} · ${dataPlane} · ${restart}`;
}

function pluginServiceStateAlerts(service: NonNullable<PluginServiceStatus["service"]>, desiredMode: PluginServiceModeFeature | null): string {
  const alerts: string[] = [];
  const rawDataPlane = service.data_plane_mode || service.active_mode;
  const dataPlane = pluginServiceEffectiveDataPlane(service);
  if (rawDataPlane === "sandbox-process" && dataPlane === ui("blocked")) {
    alerts.push(ui("Sandbox data plane is blocked by the runtime gate or environment self-check."));
  }
  if (dataPlane && dataPlane !== service.desired_mode) {
    alerts.push(`${ui("Current data plane remains")} ${dataPlane}; ${ui("desired mode is")} ${service.desired_mode}.`);
    alerts.push(ui("future desired only until the service mode is applied; current data plane is unchanged."));
  }
  if (desiredMode && (!desiredMode.implemented || !desiredMode.data_plane) && desiredMode.unsupported_reason) {
    alerts.push(`${ui("Future desired mode only; current data plane is unchanged.")} ${localizeMessage(desiredMode.unsupported_reason)}`);
  }
  return alerts.map((item) => `<div class="alert inline-alert">${escapeHTML(item)}</div>`).join("");
}

function sandboxEnvironmentPanel(env: PluginServiceStatus["sandbox_environment"]): string {
  if (!env) {
    return "";
  }
  return `
    <section class="subsection">
      <h4>${escapeHTML(ui("Sandbox environment"))}</h4>
      <div class="status-grid dense">
        ${detailStat(ui("Runtime gate"), yesNo(Boolean(env.gate_enabled)))}
        ${detailStat(ui("Self-check"), yesNo(Boolean(env.self_check_ok)))}
        ${detailStat(ui("Data-plane eligible"), yesNo(Boolean(env.data_plane_eligible)))}
        ${detailStat(ui("Profile"), env.policy_profile || "")}
        ${detailStat(ui("Reason code"), env.reason_code || "")}
      </div>
      ${env.reason ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(env.reason))}</div>` : ""}
      ${sandboxEnforcementFactsTable(env.enforcement_facts || [])}
    </section>
  `;
}

function sandboxEnforcementFactsTable(facts: NonNullable<PluginServiceStatus["sandbox_environment"]>["enforcement_facts"]): string {
  if (!facts || facts.length === 0) {
    return "";
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Category"))}</th><th>${escapeHTML(ui("Key"))}</th><th>${escapeHTML(ui("Required"))}</th><th>${escapeHTML(ui("Enforced"))}</th><th>${escapeHTML(ui("Method"))}</th><th>${escapeHTML(ui("Reason"))}</th></tr></thead>
    <tbody>${facts.map((fact) => `
      <tr>
        <td>${escapeHTML(formatValue(fact.category))}</td>
        <td>${escapeHTML(fact.key)}</td>
        <td>${badge(yesNo(fact.required), !fact.required)}</td>
        <td>${badge(yesNo(fact.enforced), !fact.enforced)}</td>
        <td>${escapeHTML(fact.method || "")}</td>
        <td>${escapeHTML(localizeMessage(fact.unsupported_reason || ""))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function serviceModeOptionLabel(mode: PluginServiceModeFeature): string {
  const restart = mode.requires_restart ? `, ${ui("restart")}` : "";
  const dataPlane = mode.implemented && mode.data_plane ? ui("data plane") : ui("future desired only");
  return `${mode.mode} (${formatValue(mode.maturity)}, ${dataPlane}${restart})`;
}

function serviceModeAvailability(modes: PluginServiceModeFeature[]): string {
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Mode"))}</th><th>${escapeHTML(ui("Maturity"))}</th><th>${escapeHTML(ui("Data plane"))}</th><th>${escapeHTML(ui("Restart"))}</th><th>${escapeHTML(ui("Reason"))}</th></tr></thead>
    <tbody>${modes.map((mode) => `
      <tr>
        <td>${escapeHTML(mode.mode)}</td>
        <td>${badge(formatValue(mode.maturity), !mode.implemented)}</td>
        <td>${badge(mode.implemented && mode.data_plane ? yesNo(true) : ui("future desired only"), !mode.data_plane)}</td>
        <td>${escapeHTML(requiredText(mode.requires_restart))}</td>
        <td>${escapeHTML(localizeMessage(mode.unsupported_reason || ""))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function runtimeAdapterAvailability(adapters: PluginRuntimeAdapterStatus[]): string {
  if (!adapters.length) {
    return "";
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Service"))}</th><th>${escapeHTML(ui("Runtime"))}</th><th>${escapeHTML(ui("Adapter"))}</th><th>${escapeHTML(ui("Maturity"))}</th><th>${escapeHTML(ui("Lifecycle"))}</th><th>${escapeHTML(ui("Data plane"))}</th><th>${escapeHTML(ui("Control"))}</th><th>${escapeHTML(ui("Reason"))}</th></tr></thead>
    <tbody>${adapters.map((adapter) => `
      <tr>
        <td>${escapeHTML(adapter.service_mode)}</td>
        <td>${escapeHTML(adapter.runtime_type)}</td>
        <td>${escapeHTML(adapter.adapter)}</td>
        <td>${badge(formatValue(adapter.maturity || "unknown"), adapter.maturity !== "implemented")}</td>
        <td>${badge(yesNo(adapter.lifecycle), !adapter.lifecycle)}</td>
        <td>${badge(yesNo(adapter.data_plane), !adapter.data_plane)}</td>
        <td>${escapeHTML(adapter.host_protocol ? `${adapter.host_protocol} · ${adapter.control_channel || ""}` : "")}</td>
        <td>${escapeHTML(localizeMessage(adapter.unsupported_reason || ""))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function extensionPointAvailability(points: PluginExtensionPointFeature[]): string {
  if (!points.length) {
    return "";
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Extension"))}</th><th>${escapeHTML(ui("Type"))}</th><th>${escapeHTML(ui("Maturity"))}</th><th>${escapeHTML(ui("Data plane"))}</th><th>${escapeHTML(ui("Restart"))}</th><th>${escapeHTML(ui("Reason"))}</th></tr></thead>
    <tbody>${points.map((point) => `
      <tr>
        <td>${escapeHTML(point.key)}</td>
        <td>${escapeHTML(formatValue(point.type))}</td>
        <td>${badge(formatValue(point.maturity), !point.implemented || point.maturity !== "implemented")}</td>
        <td>${badge(yesNo(point.data_plane), !point.data_plane)}</td>
        <td>${escapeHTML(requiredText(point.requires_restart))}</td>
        <td>${escapeHTML(localizeMessage(point.unsupported_reason || ""))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function pluginHostList(hosts: PluginHostRuntimeSummary[]): string {
  if (!hosts.length) {
    return "";
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Plugin"))}</th><th>PID</th><th>${escapeHTML(ui("State"))}</th><th>${escapeHTML(ui("Drain"))}</th><th>${escapeHTML(ui("Crashes"))}</th><th>${escapeHTML(ui("Backoff"))}</th><th>${escapeHTML(ui("Started"))}</th><th>${escapeHTML(ui("Error"))}</th></tr></thead>
    <tbody>${hosts.map((host) => `
      <tr>
        <td>${escapeHTML(host.plugin_id)}</td>
        <td>${escapeHTML(String(host.pid || ""))}</td>
        <td>${badge(formatValue(host.isolated ? "isolated" : host.state || "unknown"), Boolean(host.isolated || host.crash_loop || host.last_error))}</td>
        <td>${escapeHTML(host.drain_mode || "")}</td>
        <td>${escapeHTML(String(host.crash_count || 0))}</td>
        <td>${escapeHTML(formatPluginTimestamp(host.backoff_until))}</td>
        <td>${escapeHTML(formatPluginTimestamp(host.started_at))}</td>
        <td>${escapeHTML(localizeMessage(host.last_error || ""))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function formatPluginTimestamp(value?: number): string {
  if (!value) {
    return "";
  }
  return new Date(value * 1000).toLocaleString();
}

function runtimeFeature(runtimeType?: string): PluginRuntimeFeature | null {
  if (!runtimeType) {
    return null;
  }
  return (state.pluginFeatures?.runtime_types || state.pluginService?.runtime_types || []).find((feature) => feature.type === runtimeType) || null;
}

function runtimeMaturityBadge(runtimeType?: string): string {
  const feature = runtimeFeature(runtimeType);
  if (!feature) {
    return badge(runtimeType ? ui("stub") : ui("unknown"), true);
  }
  return badge(formatValue(feature.maturity), !feature.implemented || !feature.data_plane);
}

function artifactMaturityBadge(artifactType?: string): string {
  switch (artifactType) {
    case "binary":
      return badge(ui("implemented"), false);
    case "source":
      return badge(ui("partial"), true);
    default:
      return badge(ui("stub"), true);
  }
}

function buildMaturityBadge(builderType?: string): string {
  switch (builderType) {
    case "local-process":
      return badge(ui("partial"), true);
    case "container":
      return badge(ui("implemented"), false);
    default:
      return badge(ui("reserved"), true);
  }
}

function governanceMaturityBadge(plugin: PluginView): string {
	if (plugin.governance?.decision) {
		return badge(ui("implemented"), false);
	}
	if (plugin.governance_error) {
    return badge(ui("partial"), true);
  }
	return badge(ui("partial"), true);
}

function configMaturityPanel(): string {
  return `<dl class="kv compact">
    <dt>${escapeHTML(ui("Maturity"))}</dt><dd>${badge(ui("implemented"), false)}</dd>
    <dt>${escapeHTML(ui("Dry-run"))}</dt><dd>${escapeHTML(ui("JSON · schema · secret refs · ReloadConfig"))}</dd>
    <dt>${escapeHTML(ui("Failure state"))}</dt><dd>${escapeHTML(ui("active and desired generation unchanged"))}</dd>
  </dl>`;
}

function secretMaturityPanel(plugin: PluginView): string {
  const secrets = plugin.secrets || [];
  const reloadRequired = secrets.filter((secret) => secret.reload_required).length;
  const hotReload = secrets.filter((secret) => secret.hot_reload).length;
  return `<dl class="kv compact">
    <dt>${escapeHTML(ui("Maturity"))}</dt><dd>${badge(ui("implemented"), false)}</dd>
    <dt>${escapeHTML(ui("Versions"))}</dt><dd>${escapeHTML(ui("current/previous summaries only"))}</dd>
    <dt>${escapeHTML(ui("Reload policy"))}</dt><dd>${escapeHTML(`${hotReload} ${ui("hot reload")} · ${reloadRequired} ${ui("reload required")}`)}</dd>
    <dt>${escapeHTML(ui("Value visibility"))}</dt><dd>${escapeHTML(ui("redacted in API, audit, operations and diagnostics"))}</dd>
  </dl>`;
}

function rollbackMaturityPanel(): string {
  return `<dl class="kv compact">
    <dt>${escapeHTML(ui("Maturity"))}</dt><dd>${badge(ui("implemented"), false)}</dd>
    <dt>${escapeHTML(ui("Gates"))}</dt><dd>${escapeHTML(ui("dry-run and governance rechecked"))}</dd>
    <dt>${escapeHTML(ui("Modes"))}</dt><dd>${escapeHTML(ui("config-only and full desired rollback"))}</dd>
    <dt>${escapeHTML(ui("Diff"))}</dt><dd>${escapeHTML(ui("sensitive values redacted"))}</dd>
  </dl>`;
}

function rolloutPanel(plugin: PluginView): string {
	if (plugin.rollout_error) {
		return `<div class="alert inline-alert">${escapeHTML(localizeMessage(plugin.rollout_error))}</div>`;
	}
  const rollout = plugin.rollout_status;
  if (!rollout) {
    return `<div class="empty">${escapeHTML(ui("No rollout state"))}</div>`;
  }
  const nodes = rollout.node_runtime_states || plugin.node_runtime_states || [];
  return `
    <div class="status-grid dense">
      ${detailStat(ui("Status"), formatValue(rollout.partial_failure ? "partial failure" : rollout.ok ? "ok" : "pending"))}
      ${detailStat(ui("Nodes"), `${rollout.nodes_ready}/${rollout.nodes_total}`)}
      ${detailStat(ui("Failed"), rollout.nodes_failed)}
      ${detailStat(ui("Stale"), rollout.nodes_stale)}
      ${detailStat(ui("Artifact package"), formatValue(rollout.artifact_distribution_status || (rollout.artifact_distribution ? "available" : "not configured")))}
      ${detailStat(ui("Cross-node apply"), formatValue(rollout.cross_node_apply ? "enabled" : "manual"))}
    </div>
    ${rollout.artifact_distribution_error ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(rollout.artifact_distribution_error))}</div>` : ""}
    ${nodes.length ? `<div class="list compact-list">${nodes.map(nodeRuntimeItem).join("")}</div>` : `<div class="empty">${escapeHTML(ui("No node runtime state"))}</div>`}
  `;
}

function nodeRuntimeItem(node: PluginNodeRuntimeState): string {
  const state = `${formatValue(node.runtime_state || "unknown")} · ${shortID(node.artifact_id || "")}${node.stale ? ` · ${ui("stale")}` : ""}`;
  return `
    <div class="list-item">
      <div>
        <strong>${escapeHTML(node.node_id)}</strong>
        <span>${escapeHTML(state)}</span>
      </div>
      ${badge(formatValue(node.error ? "failed" : node.enabled ? "enabled" : node.loaded ? "loaded" : "idle"), Boolean(node.error || node.stale))}
    </div>
  `;
}

async function updatePluginServiceMode(event: Event): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  try {
    // 服务模式变更可能需要后端迁移或重启，因此保存后立即刷新插件页状态。
    await api("/plugin-service", {
      method: "PUT",
      body: {
        desired_mode: getFormInput(form, "desired_mode"),
        crash_policy: {
          backoff_seconds: Number(getFormInput(form, "crash_backoff_seconds") || 30),
          max_crashes: Number(getFormInput(form, "crash_max_crashes") || 1),
          window_seconds: Number(getFormInput(form, "crash_window_seconds") || 300),
        },
      },
    });
    await loadPlugins();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function instrumentationList(records: PluginInstrumentation[]): string {
  if (!records.length) {
    return `<p class="muted">${escapeHTML(ui("No instrumentation metadata"))}</p>`;
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Name"))}</th><th>${escapeHTML(ui("Profile"))}</th><th>${escapeHTML(ui("Status"))}</th><th>${escapeHTML(ui("Diff"))}</th><th>${escapeHTML(ui("Binding"))}</th><th>${escapeHTML(ui("Conformance"))}</th><th>${escapeHTML(ui("Benchmark"))}</th><th>${escapeHTML(ui("Smoke"))}</th><th>${escapeHTML(ui("Rollback"))}</th></tr></thead>
    <tbody>${records.map((record) => `
      <tr>
        <td>${escapeHTML(record.name)} ${escapeHTML(record.version || "")}</td>
        <td>${escapeHTML(record.profile || "")}</td>
        <td>${badge(formatValue(record.status || "available"), record.status === "blocked")}</td>
        <td>${escapeHTML(shortID(record.generated_diff_hash || ""))}</td>
        <td>${instrumentationBinding(record)}</td>
        <td>${instrumentationEvidenceBadge(record.conformance_json)}</td>
        <td>${instrumentationEvidenceBadge(record.benchmark_json)}</td>
        <td>${instrumentationEvidenceBadge(record.smoke_json)}</td>
        <td>${escapeHTML(record.runbook_rollback || "")}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function instrumentationBinding(record: PluginInstrumentation): string {
  if (!record.gateway_binary_sha256 || !record.ci_artifact_sha256) {
    return badge(ui("missing"), true);
  }
  return escapeHTML(`gw ${shortID(record.gateway_binary_sha256)} · ci ${shortID(record.ci_artifact_sha256)}`);
}

function instrumentationEvidenceBadge(raw?: string): string {
  if (!raw) {
    return badge(ui("missing"), true);
  }
  try {
    const value = JSON.parse(raw) as { ok?: unknown };
    if (value && typeof value === "object" && "ok" in value) {
      const passed = value.ok === true;
      return badge(formatValue(passed ? "ok" : "failed"), !passed);
    }
  } catch {
    return badge(ui("missing"), true);
  }
  return badge(ui("missing"), true);
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
    // 浏览器只负责上传文件；manifest 校验、哈希和制品类型判断由后端完成。
    await api("/plugin-artifacts", { method: "POST", formData });
    input.value = "";
    await loadPlugins();
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function bindPluginDetailEvents(plugin: PluginView): void {
  // 详情区每次重绘都会替换 DOM，因此按钮事件必须在重绘后重新绑定。
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
  document.getElementById("pluginDispatchRefreshRoutesBtn")?.addEventListener("click", () => runDispatchAction("refresh-routes"));
  document.getElementById("pluginDispatchReplaySubscribersBtn")?.addEventListener("click", () => runDispatchAction("replay-subscribers"));
  document.getElementById("pluginDispatchDropSubscribersBtn")?.addEventListener("click", () => runDispatchAction("drop-subscriber-dead-letter"));
  document.getElementById("pluginOperationsLoadBtn")?.addEventListener("click", () => loadPluginOperations(plugin));
  document.getElementById("pluginOperationsGCDryRunBtn")?.addEventListener("click", () => dryRunOperationsGC(plugin));
  document.getElementById("pluginDiagnosticBtn")?.addEventListener("click", () => loadDiagnosticPackage(plugin));
  const repositoryUpdatesForm = document.getElementById("pluginRepositoryUpdatesForm");
  if (repositoryUpdatesForm instanceof HTMLFormElement) {
    repositoryUpdatesForm.addEventListener("submit", (event) => checkRepositoryUpdates(event, plugin));
  }
}

async function runDispatchAction(action: string): Promise<void> {
  try {
    const data = await api<unknown>("/plugins/dispatch-plan", {
      method: "POST",
      body: { action },
    });
    el("pluginDispatchOutput").textContent = formatJSON(data || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function dryRunConfig(plugin: PluginView): Promise<void> {
  try {
    // dry-run 不保存配置，只返回脱敏后的校验结果、diff 和是否需要重启。
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
    // 配置保存写入期望状态；后端会根据当前制品和运行态判断是否可热加载。
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

async function checkRepositoryUpdates(event: SubmitEvent, plugin: PluginView): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  try {
    const data = await api<RepositoryUpdatesResponse>("/plugin-repositories/imports", {
      method: "POST",
      body: {
        action: "updates",
        repository_type: getFormSelect(form, "repository_type").value,
        index_path: getFormInput(form, "index_path").value,
        artifact_id: getFormInput(form, "artifact_id").value,
        plugin_id: plugin.id,
        version: getFormInput(form, "version").value,
      },
    });
    el("pluginRepositoryUpdatesOutput").textContent = formatJSON(data.updates || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function saveSecret(event: SubmitEvent, plugin: PluginView): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  try {
    // 密钥值不回显，保存后通过重新加载详情刷新版本号和 reload 标记。
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
    // enable/disable/load/delete 等动作都走统一动作接口，后端负责审计和操作日志。
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
    // 从未纳管制品创建 disabled 期望状态，管理员随后可以编辑配置再启用。
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
    // 制品回滚只改期望制品；后端仍会执行治理检查和配置 dry-run。
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
    // 构建动作可能耗时，当前界面以刷新详情的方式展示最新构建状态。
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
    // 配置快照回滚可只恢复配置，也可连同 artifact/desired state/priority 一起恢复。
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
    // diff 已由后端脱敏，前端只负责展示结果给管理员确认。
    const data = await api<SnapshotDiffResponse>(`/plugins/${encodeURIComponent(pluginID)}/config/snapshots/${snapshotID}/diff`);
    el("pluginDryRunResult").textContent = formatJSON(data.diff || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function createGovernanceReview(plugin: PluginView): Promise<void> {
  try {
    // 评审记录绑定当前 desired artifact 和配置哈希，用于后续启用或回滚门禁。
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
  const reason = window.prompt(ui("Reason"));
  if (!reason) {
    return;
  }
  try {
    // override 是带 TTL 的临时治理豁免，必须记录人工原因。
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
    // preflight 由插件或宿主返回检查项，结果会持久化到治理面板。
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
    // self-test 用于验证制品自身能力，不直接修改 desired state。
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
  const diff = Number(window.prompt(ui("Baseline diff, e.g. 0.25"), "0.25"));
  if (!Number.isFinite(diff)) {
    return;
  }
  try {
    // 手动录入基准差异用于治理门禁判断，避免高风险性能回退直接启用。
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
  const advisoryID = window.prompt(ui("Advisory ID"), `local-${shortID(artifact.sha256)}`);
  if (!advisoryID) {
    return;
  }
  try {
    // 撤销公告会让命中的制品进入隔离/阻断路径，详情刷新后展示最新治理状态。
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
    // 运维快照包含事件、日志、trace、任务、外部依赖和 GC 候选项，按需刷新即可。
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations`);
    el("pluginOperationsOutput").textContent = formatJSON(data.operations || {});
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function dryRunOperationsGC(plugin: PluginView): Promise<void> {
  try {
    // GC dry-run 不删除文件，只展示哪些运行态数据会被保护或清理。
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations/gc`);
    el("pluginOperationsOutput").textContent = formatJSON(data);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function loadDiagnosticPackage(plugin: PluginView): Promise<void> {
  try {
    // 诊断包由后端生成并脱敏，前端以 JSON 文本形式展示给管理员。
    const data = await api<OperationsResponse>(`/plugins/${encodeURIComponent(plugin.id)}/operations/diagnostic`);
    el("pluginOperationsOutput").textContent = formatJSON(data);
    showAlert("");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

function uploadInventoryDetail(): string {
  // 库存视图聚合未纳管制品和构建记录，支撑上传、构建、纳管的完整流程。
  const artifact = selectedArtifact();
  if (!artifact) {
    return `
      <div class="plugin-layout">
        <section class="panel">
          <h3>${escapeHTML(ui("Uploaded artifacts"))}</h3>
          ${artifactInventoryList(state.pluginArtifacts)}
        </section>
        <section class="panel">
          <h3>${escapeHTML(ui("Builds"))}</h3>
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
        <h3>${escapeHTML(ui("Artifact"))}</h3>
        <dl class="kv">
          <dt>${escapeHTML(ui("Status"))}</dt><dd>${escapeHTML(formatValue(artifact.status))}</dd>
          <dt>${escapeHTML(ui("Maturity"))}</dt><dd>${artifactMaturityBadge(artifact.artifact_type)}</dd>
          <dt>${escapeHTML(ui("Runtime"))}</dt><dd>${escapeHTML(artifact.runtime_type || "")}</dd>
          <dt>${escapeHTML(ui("Runtime maturity"))}</dt><dd>${runtimeMaturityBadge(artifact.runtime_type)}</dd>
          <dt>${escapeHTML(ui("Go/API"))}</dt><dd>${escapeHTML(`${artifact.go_version || ""} ${artifact.api_version || ""}`)}</dd>
          <dt>SHA256</dt><dd>${escapeHTML(artifact.sha256)}</dd>
          <dt>${escapeHTML(ui("Extensions"))}</dt><dd>${escapeHTML(jsonList(artifact.extension_points_json))}</dd>
          <dt>${escapeHTML(ui("Error"))}</dt><dd>${escapeHTML(localizeMessage(artifact.error || ""))}</dd>
        </dl>
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Create desired state"))}</h3>
        ${canWrite ? `
          <form id="artifactDesiredForm" class="inline-form">
            <label>${escapeHTML(ui("Priority"))}<input name="priority" type="number" value="100"></label>
            <textarea id="artifactConfigEditor">{}</textarea>
            <button type="submit">${escapeHTML(ui("Create disabled plugin"))}</button>
          </form>
        ` : `<div class="empty">${escapeHTML(ui("Only binary artifacts can be used as plugin desired state."))}</div>`}
      </section>
      <section class="panel">
        <h3>${escapeHTML(ui("Builds"))}</h3>
        ${buildInventoryList(state.pluginBuilds.filter((build) => build.plugin_id === artifact.plugin_id))}
      </section>
    </div>
  `;
}

function bindInventoryEvents(): void {
  // 库存视图也是动态渲染，制品详情和纳管表单事件需要在渲染后绑定。
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
    return `<div class="empty">${escapeHTML(ui("No artifacts"))}</div>`;
  }
  return `<div class="mini-list">${artifacts.map((artifact) => `
    <div class="mini-row">
      <span>${escapeHTML(artifact.plugin_id)}</span>
      <span>${escapeHTML(artifact.version)} · ${escapeHTML(formatValue(artifact.artifact_type))} · ${artifactMaturityBadge(artifact.artifact_type)} · ${escapeHTML(formatValue(artifact.status))}</span>
      <span>${escapeHTML(shortID(artifact.id))}</span>
      <button class="secondary" type="button" data-artifact-detail="${escapeAttr(artifact.id)}">${escapeHTML(ui("Open"))}</button>
    </div>
  `).join("")}</div>`;
}

function buildInventoryList(builds: PluginBuild[]): string {
  if (builds.length === 0) {
    return `<div class="empty">${escapeHTML(ui("No builds"))}</div>`;
  }
  return `<div class="mini-list">${builds.map((build) => `
    <div class="mini-row">
      <span>${escapeHTML(build.plugin_id)} #${escapeHTML(build.id)}</span>
      <span>${badge(formatValue(build.status), build.status !== "succeeded")}</span>
      <span>${buildMaturityBadge(build.builder_type)}</span>
      <span>${escapeHTML(buildBuilderLabel(build))}</span>
      <span>${escapeHTML(shortID(build.artifact_id || build.source_id))}</span>
      <span>${escapeHTML(buildPolicyLabel(build))}</span>
      <span>${escapeHTML(build.log_summary || localizeMessage(build.error || ""))}</span>
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
    <button type="button" data-plugin-action="load">${escapeHTML(ui("Load"))}</button>
    <button type="button" data-plugin-action="enable">${escapeHTML(ui("Enable"))}</button>
    <button class="secondary" type="button" data-plugin-action="disable">${escapeHTML(ui("Disable"))}</button>
    <button class="danger" type="button" data-plugin-action="delete">${escapeHTML(ui("Delete"))}</button>
  `;
}

function governancePanel(plugin: PluginView, canWrite: boolean): string {
  if (plugin.governance_error) {
    return `<div class="alert inline-alert">${escapeHTML(localizeMessage(plugin.governance_error))}</div>`;
  }
  const governance = plugin.governance;
  const decision = governance?.decision;
  const policy = governance?.policy;
  const issues = decision?.issues || [];
  return `
    <dl class="kv">
      <dt>${escapeHTML(ui("Profile"))}</dt><dd>${escapeHTML(decision?.profile || "")}</dd>
      <dt>${escapeHTML(ui("Risk"))}</dt><dd>${escapeHTML(formatValue(decision?.risk_level || ""))}</dd>
      <dt>${escapeHTML(ui("Policy"))}</dt><dd>${escapeHTML(shortID(decision?.policy_hash || ""))}</dd>
      <dt>${escapeHTML(ui("Fixture gate"))}</dt><dd>${badge(requiredText(Boolean(policy?.require_conformance_fixture)), Boolean(policy?.require_conformance_fixture))}</dd>
      <dt>${escapeHTML(ui("Maturity"))}</dt><dd>${governanceMaturityBadge(plugin)}</dd>
      <dt>${escapeHTML(ui("Decision"))}</dt><dd>${badge(formatValue(decision?.ok ? "allowed" : "blocked"), !decision?.ok)}</dd>
      <dt>${escapeHTML(ui("Review"))}</dt><dd>${badge(requiredText(Boolean(decision?.review_required)), Boolean(decision?.review_required))}</dd>
      <dt>${escapeHTML(ui("Override"))}</dt><dd>${badge(formatValue(decision?.warning_override_used ? "used" : "not used"), Boolean(decision?.warning_override_used))}</dd>
    </dl>
    ${issues.length ? `<div class="mini-list">${issues.map((issue) => `
      <div class="mini-row">
        <span>${badge(formatValue(issue.severity), issue.severity !== "info")}</span>
        <span>${escapeHTML(issue.code)}</span>
        <span>${escapeHTML(localizeMessage(issue.message))}</span>
      </div>
    `).join("")}</div>` : `<div class="empty">${escapeHTML(ui("No governance issues"))}</div>`}
    ${canWrite ? `
      <div class="row-actions">
        <button class="secondary" type="button" id="pluginGovernanceReviewBtn">${escapeHTML(ui("Review"))}</button>
        <button class="secondary" type="button" id="pluginGovernanceOverrideBtn">${escapeHTML(ui("Override"))}</button>
        <button class="secondary" type="button" id="pluginGovernancePreflightBtn">${escapeHTML(ui("Preflight"))}</button>
        <button class="secondary" type="button" id="pluginGovernanceSelfTestBtn">${escapeHTML(ui("Self-test"))}</button>
        <button class="secondary" type="button" id="pluginGovernanceBenchmarkBtn">${escapeHTML(ui("Benchmark"))}</button>
        <button class="danger" type="button" id="pluginGovernanceAdvisoryBtn">${escapeHTML(ui("Revoke artifact"))}</button>
      </div>
    ` : ""}
    <pre class="log-output">${escapeHTML(formatJSON({
      policy: governance?.policy,
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
    return `<div class="empty">${escapeHTML(ui("No artifacts"))}</div>`;
  }
  return `<div class="mini-list">${artifacts.map((artifact) => `
    <div class="mini-row">
      <span>${escapeHTML(shortID(artifact.id))}</span>
      <span>${escapeHTML(artifact.version)} · ${escapeHTML(formatValue(artifact.artifact_type))} · ${artifactMaturityBadge(artifact.artifact_type)} · ${escapeHTML(formatValue(artifact.status))}</span>
      <span>${escapeHTML(artifact.go_version || "")}</span>
      ${isAdmin() && artifact.artifact_type === "binary" && artifact.id !== plugin.desired_artifact_id ? `<button class="secondary" type="button" data-artifact-rollback="${escapeAttr(artifact.id)}">${escapeHTML(ui("Rollback"))}</button>` : ""}
    </div>
  `).join("")}</div>`;
}

function buildList(builds: PluginBuild[], plugin: PluginView, canWrite: boolean): string {
  if (builds.length === 0) {
    return `<div class="empty">${escapeHTML(ui("No builds"))}</div>`;
  }
  return `<div class="mini-list">${builds.map((build) => `
    <div class="mini-row">
      <span>#${escapeHTML(build.id)}</span>
      <span>${badge(formatValue(build.status), build.status !== "succeeded")}</span>
      <span>${buildMaturityBadge(build.builder_type)}</span>
      <span>${escapeHTML(buildBuilderLabel(build))}</span>
      <span>${escapeHTML(shortID(build.artifact_id || build.source_id))}</span>
      <span>${escapeHTML(buildProvenanceLabel(build))}</span>
      <span>${escapeHTML(buildPolicyLabel(build))}</span>
      <span>${escapeHTML(build.log_summary || localizeMessage(build.error || ""))}</span>
      ${canWrite ? buildActions(build, plugin) : ""}
    </div>
  `).join("")}</div>`;
}

function buildBuilderLabel(build: PluginBuild): string {
  const image = build.builder_image ? ` ${build.builder_image}` : "";
  const version = build.builder_version ? ` ${build.builder_version}` : "";
  return `${build.builder_type}${image}${version}`;
}

function buildProvenanceLabel(build: PluginBuild): string {
  const parts = [
    build.go_version || "",
    build.abi_fingerprint ? `${ui("abi")} ${shortID(build.abi_fingerprint)}` : "",
    build.artifact_sha256 ? `${ui("artifact")} ${shortID(build.artifact_sha256)}` : "",
  ].filter(Boolean);
  return parts.join(" · ");
}

function buildPolicyLabel(build: PluginBuild): string {
  const policies = [
    build.go_proxy ? "GOPROXY" : "",
    build.go_no_sumdb ? "GONOSUMDB" : "",
    build.go_private ? "GOPRIVATE" : "",
    build.vendor_required ? ui("vendor") : "",
  ].filter(Boolean);
  return policies.length ? `${ui("policy")} ${policies.join(", ")}` : "";
}

function buildActions(build: PluginBuild, plugin: PluginView): string {
  if (build.status === "queued") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="run">${escapeHTML(ui("Run"))}</button>`;
  }
  if (build.status === "running") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="cancel">${escapeHTML(ui("Cancel"))}</button>`;
  }
  if (build.status === "failed" || build.status === "canceled") {
    return `<button class="secondary" type="button" data-build-id="${escapeAttr(build.id)}" data-build-action="retry">${escapeHTML(ui("Retry"))}</button>`;
  }
  if (build.status === "succeeded" && build.artifact_id && build.artifact_id !== plugin.desired_artifact_id) {
    return `<button class="secondary" type="button" data-artifact-rollback="${escapeAttr(build.artifact_id)}">${escapeHTML(ui("Use artifact"))}</button>`;
  }
  return "";
}

function snapshotList(snapshots: PluginSnapshot[], canWrite: boolean): string {
  if (snapshots.length === 0) {
    return `<div class="empty">${escapeHTML(ui("No snapshots"))}</div>`;
  }
  return `<div class="mini-list">${snapshots.map((snapshot) => `
    <div class="mini-row">
      <span>#${escapeHTML(snapshot.id)}</span>
      <span>${escapeHTML(ui("Generation"))} ${escapeHTML(snapshot.desired_generation)} · ${escapeHTML(formatValue(snapshot.desired_state))} · ${escapeHTML(shortID(snapshot.artifact_id))}</span>
      <button class="secondary" type="button" data-snapshot-diff="${escapeAttr(snapshot.id)}">${escapeHTML(ui("Diff"))}</button>
      ${canWrite ? `
        <button class="secondary" type="button" data-snapshot-rollback="${escapeAttr(snapshot.id)}">${escapeHTML(ui("Config rollback"))}</button>
        <button class="secondary" type="button" data-snapshot-full-rollback="${escapeAttr(snapshot.id)}">${escapeHTML(ui("Full rollback"))}</button>
      ` : ""}
    </div>
  `).join("")}</div>`;
}

function connectionSessionList(sessions: PluginConnectionSession[]): string {
  if (sessions.length === 0) {
    return `<div class="empty">${escapeHTML(ui("No active connection sessions"))}</div>`;
  }
  return `<div class="mini-list">${sessions.map((conn) => `
    <div class="mini-row">
      <span>#${escapeHTML(conn.id)}</span>
      <span>${escapeHTML(shortID(conn.artifact_id))} · ${escapeHTML(conn.handler_id)}</span>
      <span>${escapeHTML(conn.duration_ms)}ms</span>
      <span>${badge(formatValue(conn.draining ? "draining" : "active"), conn.draining)}</span>
    </div>
  `).join("")}</div>`;
}

function secretChip(secret: PluginSecret): string {
  const reload = secret.reload_required ? ui("reload required") : ui("hot reload");
  return `<span class="chip">${escapeHTML(secret.name)} v${escapeHTML(secret.current_version)} · ${escapeHTML(ui("prev"))} ${escapeHTML(secret.previous_version)} · ${escapeHTML(reload)}</span>`;
}

function runtimeABI(plugin: PluginView): string {
  const summary = plugin.runtime_summary || {};
  const manifestRuntime = runtimeManifest(plugin);
  return String(summary.runtime_abi || summary.host_abi || manifestRuntime.abi || "");
}

function runtimeLimits(plugin: PluginView): string {
  const summary = plugin.runtime_summary || {};
  const limits = objectValue(summary.limits) || objectValue(plugin.manifest?.runtime_limits);
  const timeout = summary.handler_timeout_ms || limits?.handler_timeout_ms;
  const memory = summary.memory_bytes || limits?.memory_bytes;
  if (timeout || memory) {
    return [`${timeout || ""}ms`, memory ? `${memory} bytes` : ""].filter(Boolean).join(" / ");
  }
  return limits ? formatJSON(limits) : "";
}

function runtimeSupportedExtensions(plugin: PluginView): string {
  const summary = plugin.runtime_summary || {};
  const supported = arrayValue(summary.supported_extensions);
  if (supported.length > 0) {
    return supported.map(String).join(", ");
  }
  return (plugin.extension_points || []).join(", ");
}

function runtimeLastError(plugin: PluginView): string {
  const summary = plugin.runtime_summary || {};
  return String(summary.last_error || plugin.last_error || "");
}

function runtimeManifest(plugin: PluginView): Record<string, unknown> {
  return objectValue(plugin.manifest?.runtime) || {};
}

function sandboxRuntimeDetailPanel(plugin: PluginView): string {
  const summary = plugin.runtime_summary || {};
  const runtimeType = String(summary.runtime_type || plugin.runtime_type || "");
  if (runtimeType !== "sandbox-process" && !summary.runtime_instance_id && !summary.control_socket) {
    return "";
  }
  return `
    <section class="panel">
      <h3>${escapeHTML(ui("Sandbox runtime"))}</h3>
      <div class="status-grid dense">
        ${detailStat(ui("Runtime type"), runtimeType)}
        ${detailStat(ui("Runtime instance"), summary.runtime_instance_id || "")}
        ${detailStat("PID", summary.pid || "")}
        ${detailStat(ui("Control socket"), summary.control_socket || "")}
        ${detailStat("cgroup", summary.cgroup || "")}
        ${detailStat(ui("Network namespace"), summary.network_namespace || "")}
        ${detailStat(ui("Active calls"), summary.active_calls || 0)}
        ${detailStat(ui("Active streams"), summary.active_streams || 0)}
        ${detailStat(ui("Reason code"), summary.reason_code || "")}
      </div>
      ${summary.last_error ? `<div class="alert inline-alert">${escapeHTML(localizeMessage(String(summary.last_error)))}</div>` : ""}
      <dl class="kv compact">
        <dt>${escapeHTML(ui("Namespace"))}</dt><dd>${badge(yesNo(Boolean(summary.namespace_enforced)), !summary.namespace_enforced)}</dd>
        <dt>${escapeHTML(ui("Filesystem"))}</dt><dd>${badge(yesNo(Boolean(summary.filesystem_enforced)), !summary.filesystem_enforced)}</dd>
        <dt>${escapeHTML(ui("Network"))}</dt><dd>${badge(yesNo(Boolean(summary.network_enforced)), !summary.network_enforced)}</dd>
        <dt>${escapeHTML(ui("Environment"))}</dt><dd>${badge(yesNo(Boolean(summary.env_enforced)), !summary.env_enforced)}</dd>
        <dt>${escapeHTML(ui("CPU/memory"))}</dt><dd>${badge(yesNo(Boolean(summary.cpu_memory_enforced)), !summary.cpu_memory_enforced)}</dd>
        <dt>${escapeHTML(ui("Process"))}</dt><dd>${badge(yesNo(Boolean(summary.process_enforced)), !summary.process_enforced)}</dd>
        <dt>${escapeHTML(ui("Cleanup"))}</dt><dd>${badge(yesNo(Boolean(summary.cleanup_enforced)), !summary.cleanup_enforced)}</dd>
        <dt>${escapeHTML(ui("Secret RPC"))}</dt><dd>${badge(yesNo(Boolean(summary.secret_rpc)), !summary.secret_rpc)}</dd>
      </dl>
      ${sandboxSummaryFactsTable(summary)}
    </section>
  `;
}

function sandboxSummaryFactsTable(summary: Record<string, unknown>): string {
  const facts = arrayValue(summary.enforcement_facts)
    .map(objectValue)
    .filter((fact): fact is Record<string, unknown> => Boolean(fact));
  if (facts.length === 0) {
    return "";
  }
  return `<table class="mini-table">
    <thead><tr><th>${escapeHTML(ui("Category"))}</th><th>${escapeHTML(ui("Key"))}</th><th>${escapeHTML(ui("Required"))}</th><th>${escapeHTML(ui("Enforced"))}</th><th>${escapeHTML(ui("Method"))}</th><th>${escapeHTML(ui("Reason"))}</th></tr></thead>
    <tbody>${facts.map((fact) => `
      <tr>
        <td>${escapeHTML(formatValue(String(fact.category || "")))}</td>
        <td>${escapeHTML(String(fact.key || ""))}</td>
        <td>${badge(yesNo(Boolean(fact.required)), !fact.required)}</td>
        <td>${badge(yesNo(Boolean(fact.enforced)), !fact.enforced)}</td>
        <td>${escapeHTML(String(fact.method || ""))}</td>
        <td>${escapeHTML(localizeMessage(String(fact.unsupported_reason || "")))}</td>
      </tr>
    `).join("")}</tbody>
  </table>`;
}

function objectValue(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

function arrayValue(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
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
