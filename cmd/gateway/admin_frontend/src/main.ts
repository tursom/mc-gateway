import { api } from "./api.js";
import { showAlert } from "./alerts.js";
import { runtimeConfig } from "./config.js";
import { debounce, el } from "./dom.js";
import { changeLanguage, initializeLanguage, setSubtitle } from "./i18n.js";
import { isAdmin, isMember, renderSessionUser } from "./session.js";
import { setToken, state } from "./state.js";
import type { LoginResponse, SetupStatus, User } from "./types.js";
import { loadAudit } from "./views/audit.js";
import { loadMetrics } from "./views/metrics.js";
import { loadRoutes, openRouteDialog, renderRoutes, saveRoute } from "./views/routes.js";
import { loadServices, renderServices } from "./views/services.js";
import { loadStatus } from "./views/status.js";
import { loadUsers, openUserDialog, renderUsers, saveUser } from "./views/users.js";

async function boot(): Promise<void> {
  state.apiBase = runtimeConfig().apiPrefix;
  initializeLanguage();
  bindEvents();
  try {
    const setup = await api<SetupStatus>("/setup");
    if (setup.required) {
      setView("setupView");
      setSubtitle("setupSubtitle");
      return;
    }
  } catch (err) {
    showAlert((err as Error).message);
  }

  if (!state.token) {
    setView("loginView");
    setSubtitle("login");
    return;
  }

  try {
    state.user = await api<User>("/me");
    await showApp();
  } catch {
    setToken("");
    setView("loginView");
    setSubtitle("login");
  }
}

function bindEvents(): void {
  el<HTMLSelectElement>("languageSelect").addEventListener("change", (event) => {
    changeLanguage((event.currentTarget as HTMLSelectElement).value, rerenderCurrentView);
  });
  el<HTMLFormElement>("setupForm").addEventListener("submit", submitSetup);
  el<HTMLFormElement>("loginForm").addEventListener("submit", submitLogin);
  el("logoutBtn").addEventListener("click", logout);
  el("routeSearch").addEventListener("input", debounce(loadRoutes, 180));
  el("newRouteBtn").addEventListener("click", () => openRouteDialog());
  el("newUserBtn").addEventListener("click", () => openUserDialog());
  el<HTMLFormElement>("routeForm").addEventListener("submit", saveRoute);
  el<HTMLFormElement>("userForm").addEventListener("submit", saveUser);

  document.querySelectorAll<HTMLButtonElement>("[data-close]").forEach((button) => {
    button.addEventListener("click", () => button.closest("dialog")?.close());
  });
  document.querySelectorAll<HTMLButtonElement>(".tabs button").forEach((button) => {
    button.addEventListener("click", () => {
      if (button.dataset.tab) {
        selectTab(button.dataset.tab);
      }
    });
  });
}

async function submitSetup(event: SubmitEvent): Promise<void> {
  event.preventDefault();
  const form = new FormData(event.currentTarget as HTMLFormElement);
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
    setSubtitle("login");
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function submitLogin(event: SubmitEvent): Promise<void> {
  event.preventDefault();
  const form = new FormData(event.currentTarget as HTMLFormElement);
  try {
    const data = await api<LoginResponse>("/auth/login", {
      method: "POST",
      body: {
        username: form.get("username"),
        password: form.get("password"),
      },
    });
    setToken(data.token);
    state.user = data.user;
    showAlert("");
    await showApp();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function logout(): Promise<void> {
  try {
    await api("/auth/logout", { method: "POST", body: {} });
  } catch {
  }
  setToken("");
  state.user = null;
  setView("loginView");
  setSubtitle("login");
  renderSessionUser();
  el("logoutBtn").classList.add("hidden");
}

async function showApp(): Promise<void> {
  setView("appView");
  setSubtitle("adminSubtitle");
  renderSessionUser();
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

function applyRoleVisibility(): void {
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

function toggleTab(name: string, visible: boolean): void {
  document.querySelector(`[data-tab="${name}"]`)?.classList.toggle("hidden", !visible);
}

function selectTab(name: string): void {
  document.querySelectorAll<HTMLButtonElement>(".tabs button").forEach((button) => {
    button.classList.toggle("active", button.dataset.tab === name);
  });
  document.querySelectorAll<HTMLElement>(".tab-panel").forEach((panel) => {
    panel.classList.add("hidden");
  });
  el(`${name}Tab`).classList.remove("hidden");
}

function setView(name: string): void {
  for (const id of ["setupView", "loginView", "appView"]) {
    el(id).classList.toggle("hidden", id !== name);
  }
}

function rerenderCurrentView(): void {
  renderSessionUser();
  renderRoutes();
  renderServices();
  renderUsers();
  if (isMember()) {
    loadStatus();
    loadMetrics();
  }
  if (isAdmin()) {
    loadAudit();
  }
}

boot();
