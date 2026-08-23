import assert from "node:assert/strict";
import { cp, mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const root = process.cwd();
const builtJS = path.join(root, "cmd/gateway/admin_static/js");
const tmp = path.join(root, ".tmp/admin-management-ui-acceptance");
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

const { state, setToken } = await import(pathToFileURL(path.join(tmpJS, "state.js")));
const routes = await import(pathToFileURL(path.join(tmpJS, "views/routes.js")));
const services = await import(pathToFileURL(path.join(tmpJS, "views/services.js")));
const users = await import(pathToFileURL(path.join(tmpJS, "views/users.js")));
const { loadStatus } = await import(pathToFileURL(path.join(tmpJS, "views/status.js")));
const { loadAudit } = await import(pathToFileURL(path.join(tmpJS, "views/audit.js")));
const { loadMetrics } = await import(pathToFileURL(path.join(tmpJS, "views/metrics.js")));

state.apiBase = "/admin/api";
state.language = "en";
state.user = { username: "admin", role: "admin", disabled: false };
setToken("management-token");
element("routeSearch").value = "survival & pvp";

const payloads = new Map([
  ["/admin/api/routes?q=survival%20%26%20pvp", {
    routes: [{ host: "<route.example>", upstream: "127.0.0.1:25565", enabled: true, note: "<script>alert(1)</script>" }],
  }],
  ["/admin/api/services", {
    services: [
      { name: "tcp_admin", enabled: true, port: 25565, running: true, restart_required: false, options: {} },
      { name: "kcp", enabled: true, port: 25566, running: false, restart_required: true, options: { data_shards: 12, parity_shards: 4 } },
      { name: "quic", enabled: false, port: 25567, running: false, restart_required: false, options: { application_protocols: ["minecraft", "custom"] } },
      { name: "websocket", enabled: true, port: 25568, running: true, restart_required: false, options: { path: "\" onfocus=\"alert(1)" } },
    ],
  }],
  ["/admin/api/users", {
    users: [
      { username: "admin", role: "admin", disabled: false },
      { username: "<operator>", role: "member", disabled: true },
    ],
  }],
  ["/admin/api/status", { pid: 42, uptime_seconds: 90, db_path: "<gateway.sqlite3>", tcp_admin_port: 25565 }],
  ["/admin/api/audit-logs", {
    audit_logs: [{ created_at: 1, actor: "<admin>", action: "route_update", target_type: "route", target_id: "<route.example>", success: false, message: "permission denied <token>" }],
  }],
  ["/admin/api/metrics", {
    total_connections: 10,
    active_connections: 2,
    tcp_connections: 7,
    websocket_connections: 3,
    route_misses: 1,
    upstream_dial_errors: 2,
    route_hits: { "<route.example>": 9 },
  }],
]);
const calls = [];
globalThis.fetch = async (url, options) => {
  calls.push({ url, options });
  assert.equal(options.headers.Authorization, "Bearer management-token");
  const payload = payloads.get(url);
  assert.ok(payload, `unexpected Admin API request: ${url}`);
  return {
    ok: true,
    statusText: "OK",
    async text() {
      return JSON.stringify(payload);
    },
  };
};

await Promise.all([
  routes.loadRoutes(),
  services.loadServices(),
  users.loadUsers(),
  loadStatus(),
  loadAudit(),
  loadMetrics(),
]);

assert.equal(calls.length, 6);
assert.deepEqual(new Set(calls.map((call) => call.url)), new Set(payloads.keys()));

const routesHTML = element("routesBody").innerHTML;
assert.match(routesHTML, /&lt;route\.example&gt;/);
assert.match(routesHTML, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
assert.match(routesHTML, /data-edit-route=/);
assert.doesNotMatch(routesHTML, /<script>/);

const servicesHTML = element("servicesGrid").innerHTML;
for (const expected of ["TCP/Admin", "KCP", "QUIC", "WebSocket", "restart required", "minecraft,custom", "data_shards"]) {
  assert.match(servicesHTML, new RegExp(expected), `service view should include ${expected}`);
}
assert.match(servicesHTML, /&quot; onfocus=&quot;alert\(1\)/);
assert.match(servicesHTML, /data-service-form=/);

const usersHTML = element("usersBody").innerHTML;
assert.match(usersHTML, /&lt;operator&gt;/);
assert.match(usersHTML, /Member/);
assert.match(usersHTML, /data-delete-user=/);

const statusHTML = element("statusGrid").innerHTML;
assert.match(statusHTML, /42/);
assert.match(statusHTML, /90s/);
assert.match(statusHTML, /&lt;gateway\.sqlite3&gt;/);

const auditHTML = element("auditBody").innerHTML;
assert.match(auditHTML, /&lt;admin&gt;/);
assert.match(auditHTML, /&lt;route\.example&gt;/);
assert.match(auditHTML, /permission denied &lt;token&gt;/);
assert.doesNotMatch(auditHTML, /<token>/);

const metricsHTML = `${element("metricsGrid").innerHTML}${element("routeHits").innerHTML}`;
assert.match(metricsHTML, /Active/);
assert.match(metricsHTML, /&lt;route\.example&gt;: 9/);

state.user = { username: "guest", role: "guest", disabled: false };
routes.renderRoutes();
services.renderServices();
assert.doesNotMatch(element("routesBody").innerHTML, /data-edit-route|data-delete-route/);
assert.doesNotMatch(element("servicesGrid").innerHTML, /<form|data-restart-service/);
assert.match(element("servicesGrid").innerHTML, /Port<\/span><strong>25565/);

console.log("management admin UI acceptance passed");
