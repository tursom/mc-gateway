import assert from "node:assert/strict";
import { cp, mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const root = process.cwd();
const builtJS = path.join(root, "cmd/gateway/admin_static/js");
const tmp = path.join(root, ".tmp/admin-ui-acceptance");
const tmpJS = path.join(tmp, "js");

await rm(tmp, { force: true, recursive: true });
await mkdir(tmp, { recursive: true });
await cp(builtJS, tmpJS, { recursive: true });
await writeFile(path.join(tmp, "package.json"), JSON.stringify({ type: "module" }));

class HTMLElementStub {}
class HTMLFormElementStub extends HTMLElementStub {}
class HTMLInputElementStub extends HTMLElementStub {}
class HTMLSelectElementStub extends HTMLElementStub {}

const elements = new Map();
function element(id) {
  if (!elements.has(id)) {
    elements.set(id, {
      id,
      innerHTML: "",
      textContent: "",
      value: "",
      checked: false,
      dataset: {},
      elements: { namedItem: () => null },
      classList: { toggle: () => {} },
      addEventListener: () => {},
    });
  }
  return elements.get(id);
}

globalThis.HTMLElement = HTMLElementStub;
globalThis.HTMLFormElement = HTMLFormElementStub;
globalThis.HTMLInputElement = HTMLInputElementStub;
globalThis.HTMLSelectElement = HTMLSelectElementStub;
globalThis.sessionStorage = {
  store: new Map(),
  getItem(key) {
    return this.store.get(key) || "";
  },
  setItem(key, value) {
    this.store.set(key, String(value));
  },
  removeItem(key) {
    this.store.delete(key);
  },
};
globalThis.window = { setTimeout, clearTimeout };
globalThis.document = {
  getElementById: element,
  querySelectorAll: () => [],
};

const stateModule = await import(pathToFileURL(path.join(tmpJS, "state.js")));
const pluginsModule = await import(pathToFileURL(path.join(tmpJS, "views/plugins.js")));
const i18nModule = await import(pathToFileURL(path.join(tmpJS, "i18n.js")));
const { state } = stateModule;
const { renderPlugins, renderPluginDetail } = pluginsModule;
const { t, ui } = i18nModule;

state.user = { username: "admin", role: "admin", disabled: false };
state.language = "zh";
state.pluginFeatures = {
  schema_version: "mc-gateway.plugin/v1",
  api_version: "plugin-api/v1",
  runtime_types: [
    { type: "go-plugin", implemented: true, maturity: "implemented", data_plane: true, requires_restart: false },
    { type: "sandbox-process", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, reason_code: "sandbox_data_plane_partial", unsupported_reason: "sandbox-process data-plane is partial; selected request/response extension points and upstream.connect/v2 takeover use the sandbox stream relay with drain-only lifecycle; broader isolation and governance hardening remain incomplete" },
    { type: "wasm", implemented: true, maturity: "partial", data_plane: true, requires_restart: false, unsupported_reason: "wasm runtime only supports low-risk extension points; upstream.connect/v2, network, file, and other high-risk extension points are not supported" },
  ],
  service_modes: [
    { mode: "in-process", implemented: true, maturity: "implemented", data_plane: true, requires_restart: false },
    { mode: "sandbox-process", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, reason_code: "sandbox_data_plane_partial", unsupported_reason: "sandbox-process data-plane is partial; selected request/response extension points and upstream.connect/v2 takeover use the sandbox stream relay with drain-only lifecycle; broader isolation and governance hardening remain incomplete" },
  ],
  runtime_adapters: [],
  extension_points: [
    { key: "admin.auth.provider/v1", type: "provider", implemented: false, maturity: "reserved", data_plane: false, requires_restart: false, unsupported_reason: "reserved auth provider" },
    { key: "ingress.service/v1", type: "service", implemented: false, maturity: "reserved", data_plane: false, requires_restart: false, unsupported_reason: "ingress.service/v1 is reserved; schema and governance checks exist but gateway-managed listener data-plane is not enabled" },
  ],
};
state.pluginService = {
  service: {
    desired_mode: "sandbox-process",
    active_mode: "in-process",
    data_plane_mode: "in-process",
    implemented_adapter: true,
    desired_maturity: "partial",
    active_maturity: "implemented",
    restart_required: true,
    crash_policy: { backoff_seconds: 30, max_crashes: 1, window_seconds: 300 },
    live_migration: "drain-only",
  },
  service_modes: state.pluginFeatures.service_modes,
  runtime_types: state.pluginFeatures.runtime_types,
  runtime_adapters: [],
  sandbox_environment: {
    gate_enabled: true,
    self_check_ok: true,
    data_plane_eligible: true,
    policy_profile: "prod",
    reason_code: "sandbox_data_plane_partial",
    reason: "sandbox-process data-plane is partial; selected request/response extension points and upstream.connect/v2 takeover use the sandbox stream relay with drain-only lifecycle; broader isolation and governance hardening remain incomplete",
    enforcement_facts: [
      { category: "namespace", key: "network", required: true, enforced: true, method: "netns" },
    ],
  },
  hosts: [],
  nodes: [],
};
state.pluginInstrumentation = [
  {
    id: 1,
    name: "source-build",
    version: "v1",
    profile: "prod",
    generated_diff_hash: "diffhash123456",
    gateway_binary_sha256: "gatewayhash123456",
    ci_artifact_sha256: "artifacthash123456",
    conformance_json: "{\"ok\":true}",
    benchmark_json: "{\"ok\":false}",
    smoke_json: "{\"ok\":true}",
    runbook_rollback: "re-run gates",
    status: "available",
  },
];

const plugin = {
  id: "acceptance-plugin",
  name: "Acceptance Plugin",
  version: "1.0.0",
  artifact_type: "binary",
  runtime_type: "go-plugin",
  desired_state: "enabled",
  runtime_state: "enabled",
  desired_artifact_id: "artifact-desired-123456",
  active_artifact_id: "artifact-active-123456",
  loaded_artifact_id: "artifact-active-123456",
  desired_artifact: { id: "artifact-desired-123456", plugin_id: "acceptance-plugin", version: "1.0.0", file_name: "acceptance.mcgp", sha256: "sha", artifact_type: "binary", runtime_type: "go-plugin", status: "loadable" },
  active_artifact: { id: "artifact-active-123456", plugin_id: "acceptance-plugin", version: "1.0.0", file_name: "acceptance.mcgp", sha256: "sha", artifact_type: "binary", runtime_type: "go-plugin", status: "loaded" },
  loaded_artifact: { id: "artifact-active-123456", plugin_id: "acceptance-plugin", version: "1.0.0", file_name: "acceptance.mcgp", sha256: "sha", artifact_type: "binary", runtime_type: "go-plugin", status: "loaded" },
  extension_points: ["upstream.connect/v2"],
  priority: 10,
  scope: { type: "global" },
  rollout: { mode: "all" },
  restart_required: false,
  health: "healthy",
  runtime_summary: {},
  dispatch_summary: [],
  extension_status: {},
  capabilities_summary: {},
  minecraft: {},
  config_json: "{\"token\":\"plain-secret\",\"host\":\"play.example\"}",
  config_schema: { type: "object" },
  secrets: [{ plugin_id: "acceptance-plugin", name: "api_token", current_version: 2, previous_version: 1, reload_required: true, hot_reload: false }],
  snapshots: [{ id: 7, plugin_id: "acceptance-plugin", artifact_id: "artifact-active-123456", config_json: "{}", desired_state: "enabled", priority: 10, desired_generation: 2 }],
  builds: [{ id: 3, plugin_id: "acceptance-plugin", source_id: "source-123456", artifact_id: "artifact-build-123456", status: "succeeded", builder_type: "container" }],
  artifacts: [
    { id: "artifact-active-123456", plugin_id: "acceptance-plugin", version: "1.0.0", file_name: "acceptance.mcgp", sha256: "sha", artifact_type: "binary", runtime_type: "go-plugin", status: "loaded" },
    { id: "source-123456", plugin_id: "acceptance-plugin", version: "1.0.0", file_name: "acceptance-src.mcgp", sha256: "source", artifact_type: "source", runtime_type: "go-plugin", status: "uploaded" },
  ],
  active_connection_sessions: 0,
  connection_sessions: [],
  governance: {
    decision: { ok: true, action: "enable", profile: "prod", risk_level: "low", policy_hash: "policyhash123456", review_required: false, warning_override_used: false, issues: [] },
    policy: { profile: "prod", require_conformance_fixture: true },
    reviews: [],
    warning_overrides: [],
    preflights: [],
    benchmarks: [],
    advisories: [],
    conflicts: { ok: true, issues: [] },
  },
  rollout_status: {
    plugin_id: "acceptance-plugin",
    desired_state: "enabled",
    desired_artifact_id: "artifact-desired-123456",
    desired_generation: 2,
    ok: true,
    partial_failure: false,
    nodes_total: 1,
    nodes_ready: 1,
    nodes_failed: 0,
    nodes_stale: 0,
    artifact_distribution: true,
    artifact_distribution_mode: "local-content-store",
    artifact_distribution_status: "available",
    artifact_package_sha256: "packagehash123456",
    cross_node_apply: false,
    node_runtime_states: [{ node_id: "node-a", plugin_id: "acceptance-plugin", artifact_id: "artifact-active-123456", desired_state: "enabled", runtime_state: "enabled", desired_generation: 2, applied_generation: 2, enabled: true, loaded: true, health: "healthy" }],
  },
  node_runtime_states: [],
  updated_at: 1,
};

state.plugins = [plugin];
state.pluginArtifacts = plugin.artifacts;
state.pluginBuilds = plugin.builds;
state.selectedPluginID = plugin.id;

renderPlugins();
renderPluginDetail(plugin);

const serviceHTML = element("pluginServicePanel").innerHTML;
const detailHTML = element("pluginDetail").innerHTML;

for (const expected of [
  "插件服务",
  "期望成熟度",
  "当前数据面保持为 in-process",
  "部分实现",
  "数据面",
  "需要重启",
  "构建期埋点",
  "预留认证提供方",
  "admin.auth.provider/v1",
  "ingress.service/v1",
  "Sandbox 环境",
  "运行时开关",
  "环境自检",
  "数据面可用",
  "原因码",
]) {
  assert.match(serviceHTML, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `service panel should include ${expected}`);
}

for (const expected of [
  "运行时成熟度",
  "制品",
  "已实现",
  "源码",
  "部分实现",
  "治理",
  "夹具门禁",
  "发布",
  "制品包",
  "构建",
  "密钥",
  "仅展示当前/上一版本摘要",
  "值可见性",
  "在 API、审计、运维和诊断中脱敏",
  "当前和期望代次保持不变",
  "需要重载",
  "快照",
  "重新执行试运行和治理检查",
  "支持仅配置和完整期望回滚",
  "敏感值已脱敏",
  "container",
]) {
  assert.match(detailHTML, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `detail panel should include ${expected}`);
}

for (const untranslated of [
  "Plugin Service",
  "Desired maturity",
  "Current data plane remains",
  "Runtime maturity",
  "Value visibility",
  "Sandbox environment",
  "Runtime gate",
  "Self-check",
  "Reason code",
]) {
  assert.doesNotMatch(`${serviceHTML}\n${detailHTML}`, new RegExp(untranslated.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `Chinese UI should not include ${untranslated}`);
}

assert.equal(ui("No builds"), "暂无构建");
assert.equal(t("action"), "动作");
assert.doesNotMatch(serviceHTML, /sandbox-process[^<]*(active|current data plane)/i, "pending sandbox mode must not read as the active data plane");

const sandboxPlugin = {
  ...plugin,
  runtime_type: "sandbox-process",
  active_artifact: { ...plugin.active_artifact, runtime_type: "sandbox-process" },
  desired_artifact: { ...plugin.desired_artifact, runtime_type: "sandbox-process" },
  runtime_summary: {
    runtime_type: "sandbox-process",
    runtime_instance_id: "runtime-acceptance",
    pid: 4242,
    control_socket: "unix:///run/control.sock",
    cgroup: "0::/mc-gateway/sandbox",
    network_namespace: "net:[4026532442]",
    active_calls: 2,
    active_streams: 1,
    reason_code: "sandbox_crash_loop",
    last_error: "sandbox crash loop",
    namespace_enforced: true,
    filesystem_enforced: true,
    network_enforced: false,
    env_enforced: true,
    cpu_memory_enforced: true,
    process_enforced: true,
    cleanup_enforced: true,
    secret_rpc: true,
    enforcement_facts: [
      { category: "network", key: "network.none", required: true, enforced: false, method: "netns", unsupported_reason: "netns unavailable" },
    ],
  },
};
renderPluginDetail(sandboxPlugin);
const sandboxDetailHTML = element("pluginDetail").innerHTML;
for (const expected of [
  "Sandbox 运行时",
  "运行时实例",
  "runtime-acceptance",
  "控制套接字",
  "网络命名空间",
  "活跃调用",
  "活跃流",
  "sandbox_crash_loop",
  "密钥 RPC",
]) {
  assert.match(sandboxDetailHTML, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `sandbox detail should include ${expected}`);
}
for (const untranslated of [
  "Sandbox runtime",
  "Runtime instance",
  "Control socket",
  "Network namespace",
  "Active calls",
  "Active streams",
  "Secret RPC",
]) {
  assert.doesNotMatch(sandboxDetailHTML, new RegExp(untranslated.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `Chinese sandbox detail should not include ${untranslated}`);
}

console.log("plugin UI acceptance passed");
