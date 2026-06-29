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
const { state } = stateModule;
const { renderPlugins, renderPluginDetail } = pluginsModule;

state.user = { username: "admin", role: "admin", disabled: false };
state.pluginFeatures = {
  schema_version: "mc-gateway.plugin/v1",
  api_version: "plugin-api/v1",
	  runtime_types: [
	    { type: "go-plugin", implemented: true, maturity: "implemented", data_plane: true, requires_restart: false },
	    { type: "wasm", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, unsupported_reason: "wazero runtime is implemented for low-risk validation extension points but disabled unless future runtime gates enable wasm" },
	  ],
	  service_modes: [
	    { mode: "in-process", implemented: true, maturity: "implemented", data_plane: true, requires_restart: false },
	    { mode: "sandbox-process", implemented: true, maturity: "partial", data_plane: true, requires_restart: true, unsupported_reason: "sandbox-process service mode is implemented but disabled unless future runtime gates enable sandbox_process" },
	  ],
  runtime_adapters: [],
  extension_points: [
    { key: "admin.auth.provider/v1", type: "provider", implemented: false, maturity: "reserved", data_plane: false, requires_restart: false, unsupported_reason: "reserved auth provider" },
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
	    unsupported_reason: "sandbox-process service mode is disabled by future runtime gate",
    crash_policy: { backoff_seconds: 30, max_crashes: 1, window_seconds: 300 },
    live_migration: "drain-only",
  },
  service_modes: state.pluginFeatures.service_modes,
  runtime_types: state.pluginFeatures.runtime_types,
  runtime_adapters: [],
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
  extension_points: ["upstream.connect/v1"],
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
  active_proxy_connections: 0,
  proxy_connections: [],
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
  "Plugin Service",
  "Desired maturity",
  "future desired only",
  "Current data plane remains in-process",
  "Build-Time Instrumentation",
  "admin.auth.provider/v1",
]) {
  assert.match(serviceHTML, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `service panel should include ${expected}`);
}

for (const expected of [
  "Runtime maturity",
  "Artifact",
  "implemented",
  "Governance",
  "Fixture gate",
  "Rollout",
  "Artifact package",
  "Secrets",
  "current/previous summaries only",
  "Value visibility",
  "reload required",
  "Snapshots",
  "dry-run and governance rechecked",
  "config-only and full desired rollback",
  "sensitive values redacted",
  "container",
]) {
  assert.match(detailHTML, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), `detail panel should include ${expected}`);
}

assert.doesNotMatch(serviceHTML, /sandbox-process[^<]*(active|current data plane)/i, "future runtime must not read as the active data plane");

console.log("plugin UI acceptance passed");
