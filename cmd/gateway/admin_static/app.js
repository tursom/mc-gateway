const apiBase = document.body.dataset.apiPrefix || "/admin/api";
const state = {
  token: sessionStorage.getItem("mcGatewayAdminToken") || "",
  user: null,
  routes: [],
  services: [],
  users: [],
};

const el = (id) => document.getElementById(id);

function showAlert(message) {
  const box = el("alert");
  box.textContent = message;
  box.classList.toggle("hidden", !message);
}

function setView(name) {
  for (const id of ["setupView", "loginView", "appView"]) {
    el(id).classList.toggle("hidden", id !== name);
  }
}

async function api(path, options = {}) {
  const headers = { "Accept": "application/json" };
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (state.token) {
    headers.Authorization = `Bearer ${state.token}`;
  }

  const res = await fetch(apiBase + path, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });

  let data = {};
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text };
    }
  }
  if (!res.ok) {
    throw new Error(data.error || res.statusText);
  }
  return data;
}

async function boot() {
  bindEvents();
  try {
    const setup = await api("/setup");
    if (setup.required) {
      setView("setupView");
      el("subtitle").textContent = "Setup";
      return;
    }
  } catch (err) {
    showAlert(err.message);
  }

  if (!state.token) {
    setView("loginView");
    el("subtitle").textContent = "Login";
    return;
  }

  try {
    state.user = await api("/me");
    await showApp();
  } catch {
    sessionStorage.removeItem("mcGatewayAdminToken");
    state.token = "";
    setView("loginView");
    el("subtitle").textContent = "Login";
  }
}

function bindEvents() {
  el("setupForm").addEventListener("submit", submitSetup);
  el("loginForm").addEventListener("submit", submitLogin);
  el("logoutBtn").addEventListener("click", logout);
  el("routeSearch").addEventListener("input", debounce(loadRoutes, 180));
  el("newRouteBtn").addEventListener("click", () => openRouteDialog());
  el("newUserBtn").addEventListener("click", () => openUserDialog());
  el("routeForm").addEventListener("submit", saveRoute);
  el("userForm").addEventListener("submit", saveUser);

  for (const button of document.querySelectorAll("[data-close]")) {
    button.addEventListener("click", () => button.closest("dialog").close());
  }
  for (const button of document.querySelectorAll(".tabs button")) {
    button.addEventListener("click", () => selectTab(button.dataset.tab));
  }
}

async function submitSetup(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  try {
    await api("/setup", {
      method: "POST",
      body: {
        username: form.get("username"),
        password: form.get("password"),
      },
    });
    showAlert("");
    setView("loginView");
  } catch (err) {
    showAlert(err.message);
  }
}

async function submitLogin(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  try {
    const data = await api("/auth/login", {
      method: "POST",
      body: {
        username: form.get("username"),
        password: form.get("password"),
      },
    });
    state.token = data.token;
    state.user = data.user;
    sessionStorage.setItem("mcGatewayAdminToken", state.token);
    showAlert("");
    await showApp();
  } catch (err) {
    showAlert(err.message);
  }
}

async function logout() {
  try {
    await api("/auth/logout", { method: "POST", body: {} });
  } catch {
  }
  sessionStorage.removeItem("mcGatewayAdminToken");
  state.token = "";
  state.user = null;
  setView("loginView");
}

async function showApp() {
  setView("appView");
  el("subtitle").textContent = "Admin";
  el("sessionUser").textContent = `${state.user.username} (${state.user.role})`;
  el("logoutBtn").classList.remove("hidden");
  applyRoleVisibility();
  await loadRoutes();
  if (isMember()) {
    await loadStatus();
    await loadServices();
    await loadMetrics();
  }
  if (isAdmin()) {
    await loadUsers();
    await loadAudit();
  }
}

function applyRoleVisibility() {
  const member = isMember();
  const admin = isAdmin();
  el("statusGrid").classList.toggle("hidden", !member);
  el("newRouteBtn").classList.toggle("hidden", !member);
  toggleTab("services", member);
  toggleTab("metrics", member);
  toggleTab("users", admin);
  toggleTab("audit", admin);
  selectTab("routes");
}

function toggleTab(name, visible) {
  document.querySelector(`[data-tab="${name}"]`).classList.toggle("hidden", !visible);
}

function selectTab(name) {
  for (const button of document.querySelectorAll(".tabs button")) {
    button.classList.toggle("active", button.dataset.tab === name);
  }
  for (const panel of document.querySelectorAll(".tab-panel")) {
    panel.classList.add("hidden");
  }
  el(`${name}Tab`).classList.remove("hidden");
}

async function loadStatus() {
  try {
    const status = await api("/status");
    el("statusGrid").innerHTML = [
      stat("PID", status.pid),
      stat("Uptime", `${status.uptime_seconds}s`),
      stat("SQLite", status.db_path),
      stat("TCP/Admin", status.tcp_admin_port),
    ].join("");
  } catch (err) {
    showAlert(err.message);
  }
}

async function loadRoutes() {
  try {
    const q = encodeURIComponent(el("routeSearch").value || "");
    const data = await api(q ? `/routes?q=${q}` : "/routes");
    state.routes = data.routes || [];
    renderRoutes();
  } catch (err) {
    showAlert(err.message);
  }
}

function renderRoutes() {
  const canWrite = isMember();
  el("routesBody").innerHTML = state.routes.map((route) => `
    <tr>
      <td>${escapeHTML(route.host)}</td>
      <td>${escapeHTML(route.upstream)}</td>
      <td>${badge(route.enabled ? "Enabled" : "Disabled", !route.enabled)}</td>
      <td>${escapeHTML(route.note || "")}</td>
      <td class="actions">${canWrite ? routeActions(route) : ""}</td>
    </tr>
  `).join("");

  for (const button of document.querySelectorAll("[data-edit-route]")) {
    button.addEventListener("click", () => {
      const route = state.routes.find((item) => item.host === button.dataset.editRoute);
      openRouteDialog(route);
    });
  }
  for (const button of document.querySelectorAll("[data-delete-route]")) {
    button.addEventListener("click", () => removeRoute(button.dataset.deleteRoute));
  }
}

function routeActions(route) {
  return `
    <div class="row-actions">
      <button class="secondary" type="button" data-edit-route="${escapeAttr(route.host)}">Edit</button>
      <button class="danger" type="button" data-delete-route="${escapeAttr(route.host)}">Delete</button>
    </div>
  `;
}

function openRouteDialog(route = null) {
  const form = el("routeForm");
  form.reset();
  form.dataset.originalHost = route ? route.host : "";
  form.elements.host.disabled = Boolean(route);
  if (route) {
    form.elements.host.value = route.host;
    form.elements.upstream.value = route.upstream;
    form.elements.enabled.checked = route.enabled;
    form.elements.note.value = route.note || "";
  } else {
    form.elements.enabled.checked = true;
  }
  el("routeDialog").showModal();
}

async function saveRoute(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const host = form.dataset.originalHost || form.elements.host.value;
  try {
    await api(`/routes/${encodeURIComponent(host)}`, {
      method: "PUT",
      body: {
        upstream: form.elements.upstream.value,
        enabled: form.elements.enabled.checked,
        note: form.elements.note.value,
      },
    });
    el("routeDialog").close();
    await loadRoutes();
    showAlert("");
  } catch (err) {
    showAlert(err.message);
  }
}

async function removeRoute(host) {
  if (host === "default" && !confirm("Delete default route?")) {
    return;
  }
  try {
    await api(`/routes/${encodeURIComponent(host)}`, { method: "DELETE" });
    await loadRoutes();
  } catch (err) {
    showAlert(err.message);
  }
}

async function loadServices() {
  try {
    const data = await api("/services");
    state.services = data.services || [];
    renderServices();
  } catch (err) {
    showAlert(err.message);
  }
}

function renderServices() {
  el("servicesGrid").innerHTML = state.services.map((service) => `
    <article class="service">
      <div>
        <span>${escapeHTML(service.name)}</span>
        <strong>${service.enabled ? "Enabled" : "Disabled"}${service.restart_required ? " / restart required" : ""}</strong>
      </div>
      ${isAdmin() ? serviceForm(service) : serviceSummary(service)}
    </article>
  `).join("");

  for (const form of document.querySelectorAll("[data-service-form]")) {
    form.addEventListener("submit", saveService);
  }
  for (const button of document.querySelectorAll("[data-restart-service]")) {
    button.addEventListener("click", () => restartService(button.dataset.restartService));
  }
}

function serviceSummary(service) {
  return `<div><span>Port</span><strong>${service.port}</strong></div>`;
}

function serviceForm(service) {
  const disabled = service.name === "tcp_admin" ? "disabled" : "";
  const optionFields = serviceOptionFields(service);
  return `
    <form data-service-form="${escapeAttr(service.name)}">
      <label class="inline">
        <input name="enabled" type="checkbox" ${service.enabled ? "checked" : ""} ${disabled}>
        Enabled
      </label>
      <label>
        Port
        <input name="port" type="number" min="1" max="65535" value="${service.port}">
      </label>
      ${optionFields}
      <div class="row-actions">
        <button type="submit">Save</button>
        <button class="secondary" type="button" data-restart-service="${escapeAttr(service.name)}">Restart</button>
      </div>
    </form>
  `;
}

function serviceOptionFields(service) {
  const options = service.options || {};
  if (service.name === "kcp") {
    return `
      <label>Data shards<input name="data_shards" type="number" min="1" value="${options.data_shards || 10}"></label>
      <label>Parity shards<input name="parity_shards" type="number" min="1" value="${options.parity_shards || 3}"></label>
    `;
  }
  if (service.name === "quic") {
    const protocols = Array.isArray(options.application_protocols) ? options.application_protocols.join(",") : "";
    return `<label>Protocols<input name="application_protocols" value="${escapeAttr(protocols)}"></label>`;
  }
  if (service.name === "websocket") {
    return `<label>Path<input name="path" value="${escapeAttr(options.path || "/")}"></label>`;
  }
  return "";
}

async function saveService(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const name = form.dataset.serviceForm;
  const options = {};
  if (name === "kcp") {
    options.data_shards = Number(form.elements.data_shards.value);
    options.parity_shards = Number(form.elements.parity_shards.value);
  } else if (name === "quic") {
    options.application_protocols = form.elements.application_protocols.value.split(",").map((item) => item.trim()).filter(Boolean);
  } else if (name === "websocket") {
    options.path = form.elements.path.value;
  }

  try {
    await api(`/services/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: {
        enabled: name === "tcp_admin" ? true : form.elements.enabled.checked,
        port: Number(form.elements.port.value),
        options,
      },
    });
    await loadServices();
  } catch (err) {
    showAlert(err.message);
  }
}

async function restartService(name) {
  try {
    await api(`/services/${encodeURIComponent(name)}/restart`, { method: "POST", body: {} });
    await loadServices();
  } catch (err) {
    showAlert(err.message);
  }
}

async function loadUsers() {
  try {
    const data = await api("/users");
    state.users = data.users || [];
    renderUsers();
  } catch (err) {
    showAlert(err.message);
  }
}

function renderUsers() {
  el("usersBody").innerHTML = state.users.map((user) => `
    <tr>
      <td>${escapeHTML(user.username)}</td>
      <td>${escapeHTML(user.role)}</td>
      <td>${badge(user.disabled ? "Disabled" : "Active", user.disabled)}</td>
      <td class="actions">
        <div class="row-actions">
          <button class="secondary" type="button" data-edit-user="${escapeAttr(user.username)}">Edit</button>
          <button class="danger" type="button" data-delete-user="${escapeAttr(user.username)}">Delete</button>
        </div>
      </td>
    </tr>
  `).join("");

  for (const button of document.querySelectorAll("[data-edit-user]")) {
    button.addEventListener("click", () => {
      const user = state.users.find((item) => item.username === button.dataset.editUser);
      openUserDialog(user);
    });
  }
  for (const button of document.querySelectorAll("[data-delete-user]")) {
    button.addEventListener("click", () => removeUser(button.dataset.deleteUser));
  }
}

function openUserDialog(user = null) {
  const form = el("userForm");
  form.reset();
  form.dataset.originalUsername = user ? user.username : "";
  form.elements.username.disabled = Boolean(user);
  form.elements.password.required = !user;
  if (user) {
    form.elements.username.value = user.username;
    form.elements.role.value = user.role;
    form.elements.disabled.checked = user.disabled;
  } else {
    form.elements.role.value = "member";
  }
  el("userDialog").showModal();
}

async function saveUser(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const username = form.dataset.originalUsername || form.elements.username.value;
  const body = {
    role: form.elements.role.value,
    disabled: form.elements.disabled.checked,
  };
  if (form.elements.password.value) {
    body.password = form.elements.password.value;
  }

  try {
    if (form.dataset.originalUsername) {
      await api(`/users/${encodeURIComponent(username)}`, { method: "PATCH", body });
    } else {
      body.username = username;
      await api("/users", { method: "POST", body });
    }
    el("userDialog").close();
    await loadUsers();
  } catch (err) {
    showAlert(err.message);
  }
}

async function removeUser(username) {
  if (!confirm(`Delete user ${username}?`)) {
    return;
  }
  try {
    await api(`/users/${encodeURIComponent(username)}`, { method: "DELETE" });
    await loadUsers();
  } catch (err) {
    showAlert(err.message);
  }
}

async function loadMetrics() {
  try {
    const data = await api("/metrics");
    el("metricsGrid").innerHTML = [
      stat("Total", data.total_connections),
      stat("Active", data.active_connections),
      stat("TCP", data.tcp_connections),
      stat("WebSocket", data.websocket_connections),
      stat("Misses", data.route_misses),
      stat("Dial errors", data.upstream_dial_errors),
    ].join("");
    const hits = data.route_hits || {};
    el("routeHits").innerHTML = Object.keys(hits).length
      ? Object.entries(hits).map(([host, count]) => `<span class="chip">${escapeHTML(host)}: ${count}</span>`).join("")
      : `<span class="chip">No hits</span>`;
  } catch (err) {
    showAlert(err.message);
  }
}

async function loadAudit() {
  try {
    const data = await api("/audit-logs");
    el("auditBody").innerHTML = (data.audit_logs || []).map((item) => `
      <tr>
        <td>${new Date(item.created_at * 1000).toLocaleString()}</td>
        <td>${escapeHTML(item.actor)}</td>
        <td>${escapeHTML(item.action)}</td>
        <td>${escapeHTML(item.target_type)}:${escapeHTML(item.target_id)}</td>
        <td>${badge(item.success ? "Success" : "Failed", !item.success)}</td>
        <td>${escapeHTML(item.message || "")}</td>
      </tr>
    `).join("");
  } catch (err) {
    showAlert(err.message);
  }
}

function stat(label, value) {
  return `<div class="stat"><span>${escapeHTML(label)}</span><strong>${escapeHTML(String(value ?? ""))}</strong></div>`;
}

function badge(text, off = false) {
  return `<span class="badge ${off ? "off" : ""}">${escapeHTML(text)}</span>`;
}

function isAdmin() {
  return state.user && state.user.role === "admin";
}

function isMember() {
  return state.user && (state.user.role === "admin" || state.user.role === "member");
}

function debounce(fn, wait) {
  let id = 0;
  return (...args) => {
    clearTimeout(id);
    id = setTimeout(() => fn(...args), wait);
  };
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;",
  }[ch]));
}

function escapeAttr(value) {
  return escapeHTML(value).replace(/`/g, "&#96;");
}

boot();
