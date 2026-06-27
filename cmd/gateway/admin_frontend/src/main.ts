// cmd/gateway/admin_frontend/src/main.ts 启动嵌入式管理端，选择初始化/登录/应用视图，并协调按角色加载数据。

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
import { bindPluginEvents, loadPlugins, renderPluginDetail, renderPlugins } from "./views/plugins.js";
import { loadRoutes, openRouteDialog, renderRoutes, saveRoute } from "./views/routes.js";
import { loadServices, renderServices } from "./views/services.js";
import { loadStatus } from "./views/status.js";
import { loadUsers, openUserDialog, renderUsers, saveUser } from "./views/users.js";

async function boot(): Promise<void> {
  // API 前缀由后端嵌入到 HTML 中，前端启动时先读取它，避免部署在子路径时写死地址。
  state.apiBase = runtimeConfig().apiPrefix;
  initializeLanguage();
  bindEvents();
  try {
    const setup = await api<SetupStatus>("/setup");
    if (setup.required) {
      // 没有任何管理账号时只展示初始化界面，不尝试加载其他运行态数据。
      setView("setupView");
      setSubtitle("setupSubtitle");
      return;
    }
  } catch (err) {
    showAlert((err as Error).message);
  }

  if (!state.token) {
    // token 保存在本地状态中；没有 token 时直接进入登录视图。
    setView("loginView");
    setSubtitle("login");
    return;
  }

  try {
    state.user = await api<User>("/me");
    await showApp();
  } catch {
    // token 失效时清空本地状态，避免后续 API 调用持续带着过期凭证。
    setToken("");
    setView("loginView");
    setSubtitle("login");
  }
}

function bindEvents(): void {
  // 所有顶层事件在启动时绑定一次，视图重渲染只更新内容区域。
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
  bindPluginEvents();

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
    // 初始化只创建首个管理员账号，创建成功后仍要求用户走登录流程获取会话 token。
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
    // 登录成功后立即保存 token 和用户信息，再统一进入应用态加载流程。
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
    // 服务端登出失败不阻塞本地清理，避免用户卡在失效会话上。
  }
  setToken("");
  state.user = null;
  setView("loginView");
  setSubtitle("login");
  renderSessionUser();
  el("logoutBtn").classList.add("hidden");
}

async function showApp(): Promise<void> {
  // 路由列表是成员和管理员都可见的基础视图，因此先加载它。
  setView("appView");
  setSubtitle("adminSubtitle");
  renderSessionUser();
  el("logoutBtn").classList.remove("hidden");
  applyRoleVisibility();
  await loadRoutes();
  if (isMember()) {
    // 成员权限可以查看运行态、服务、指标和插件，但不能管理用户与审计。
    await loadStatus();
    await loadServices();
    await loadMetrics();
    await loadPlugins();
  }
  if (isAdmin()) {
    // 管理员专属数据放在最后加载，减少普通成员的无权限请求。
    await loadUsers();
    await loadAudit();
  }
}

function applyRoleVisibility(): void {
  const member = isMember();
  const admin = isAdmin();
  // 角色控制只隐藏入口；服务端仍会按 token 做权限校验。
  el("statusGrid").classList.toggle("hidden", !member);
  el("newRouteBtn").classList.toggle("hidden", !member);
  toggleTab("services", member);
  toggleTab("plugins", member);
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
  // 切换语言后复用当前内存状态重绘静态文案，再刷新会随语言展示的远端数据。
  renderSessionUser();
  renderRoutes();
  renderServices();
  renderUsers();
  renderPlugins();
  renderPluginDetail();
  if (isMember()) {
    loadStatus();
    loadMetrics();
    loadPlugins();
  }
  if (isAdmin()) {
    loadAudit();
  }
}

boot();
