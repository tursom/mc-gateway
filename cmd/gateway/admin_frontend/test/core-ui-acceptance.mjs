import assert from "node:assert/strict";
import { cp, mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const root = process.cwd();
const builtJS = path.join(root, "cmd/gateway/admin_static/js");
const tmp = path.join(root, ".tmp/admin-core-ui-acceptance");
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
globalThis.window = { setTimeout, clearTimeout, MCGatewayAdmin: { apiPrefix: "/ops/api" } };
globalThis.document = {
  getElementById: element,
  querySelectorAll: () => [],
};

const stateModule = await import(pathToFileURL(path.join(tmpJS, "state.js")));
const { api } = await import(pathToFileURL(path.join(tmpJS, "api.js")));
const session = await import(pathToFileURL(path.join(tmpJS, "session.js")));
const dom = await import(pathToFileURL(path.join(tmpJS, "dom.js")));
const { runtimeConfig } = await import(pathToFileURL(path.join(tmpJS, "config.js")));
const { state, setToken, tokenStorageKey } = stateModule;

assert.deepEqual(runtimeConfig(), { apiPrefix: "/ops/api" });
window.MCGatewayAdmin = undefined;
assert.deepEqual(runtimeConfig(), { apiPrefix: "/admin/api" });

setToken("session-token");
assert.equal(state.token, "session-token");
assert.equal(sessionStorage.getItem(tokenStorageKey), "session-token");
setToken("");
assert.equal(state.token, "");
assert.equal(sessionStorage.getItem(tokenStorageKey), "");

state.language = "en";
state.user = { username: "admin", role: "admin", disabled: false };
assert.equal(session.isAdmin(), true);
assert.equal(session.isMember(), true);
assert.equal(session.formatRole("admin"), "Admin");
session.renderSessionUser();
assert.equal(element("sessionUser").textContent, "admin (Admin)");
state.user = { username: "guest", role: "guest", disabled: false };
assert.equal(session.isAdmin(), false);
assert.equal(session.isMember(), false);
assert.equal(session.formatRole("custom"), "custom");

assert.equal(dom.escapeHTML(`<script a="b">'&`), "&lt;script a=&quot;b&quot;&gt;&#39;&amp;");
assert.equal(dom.escapeAttr("`quoted`"), "&#96;quoted&#96;");
assert.match(dom.stat("<label>", "<&>"), /&lt;label&gt;.*&lt;&amp;&gt;/);
assert.equal(dom.badge("<active>"), '<span class="badge ">&lt;active&gt;</span>');
assert.equal(dom.badge("off", true), '<span class="badge off">off</span>');

state.apiBase = "/ops/api";
setToken("api-token");
const calls = [];
globalThis.fetch = async (url, options) => {
  calls.push({ url, options });
  return {
    ok: true,
    statusText: "OK",
    async text() {
      return '{"ok":true}';
    },
  };
};
assert.deepEqual(await api("/routes", { method: "POST", body: { host: "play.example" } }), { ok: true });
assert.equal(calls.length, 1);
assert.equal(calls[0].url, "/ops/api/routes");
assert.equal(calls[0].options.method, "POST");
assert.equal(calls[0].options.headers.Authorization, "Bearer api-token");
assert.equal(calls[0].options.headers["Content-Type"], "application/json");
assert.equal(calls[0].options.body, '{"host":"play.example"}');

globalThis.fetch = async () => ({
  ok: false,
  statusText: "Forbidden",
  async text() {
    return '{"error":"permission denied"}';
  },
});
await assert.rejects(() => api("/users"), /permission denied/);

globalThis.fetch = async () => ({
  ok: false,
  statusText: "Bad Gateway",
  async text() {
    return "";
  },
});
await assert.rejects(() => api("/status"), /Bad Gateway/);

console.log("core admin UI acceptance passed");
