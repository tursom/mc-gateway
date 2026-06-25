import { el } from "./dom.js";
import { languageStorageKey, state } from "./state.js";

const defaultLanguage = "en";

const translations = {
  en: {
    activeConnections: "Active",
    activeUser: "Active",
    actions: "Actions",
    actor: "Actor",
    adminSubtitle: "Admin",
    audit: "Audit",
    cancel: "Cancel",
    createAdmin: "Create admin",
    dataShards: "Data shards",
    delete: "Delete",
    deleteDefaultRouteConfirm: "Delete default route?",
    deleteUserConfirm: "Delete user {username}?",
    dialErrors: "Dial errors",
    disabled: "Disabled",
    edit: "Edit",
    enabled: "Enabled",
    failed: "Failed",
    host: "Host",
    initialAdmin: "Initial admin",
    language: "Language",
    languageChinese: "中文",
    languageEnglish: "English",
    login: "Login",
    logout: "Logout",
    message: "Message",
    metrics: "Metrics",
    misses: "Misses",
    newRoute: "New route",
    newUser: "New user",
    noHits: "No hits",
    note: "Note",
    parityShards: "Parity shards",
    password: "Password",
    path: "Path",
    port: "Port",
    protocols: "Protocols",
    restart: "Restart",
    restartRequired: "restart required",
    result: "Result",
    role: "Role",
    roleAdmin: "Admin",
    roleGuest: "Guest",
    roleMember: "Member",
    route: "Route",
    routeHits: "Route hits",
    routeSearch: "Search host, upstream, note",
    routes: "Routes",
    save: "Save",
    services: "Services",
    setupSubtitle: "Setup",
    success: "Success",
    target: "Target",
    time: "Time",
    total: "Total",
    upstream: "Upstream",
    uptime: "Uptime",
    user: "User",
    username: "Username",
    users: "Users",
  },
  zh: {
    activeConnections: "活跃连接",
    activeUser: "启用",
    actions: "操作",
    actor: "操作者",
    adminSubtitle: "管理后台",
    audit: "审计",
    cancel: "取消",
    createAdmin: "创建管理员",
    dataShards: "数据分片",
    delete: "删除",
    deleteDefaultRouteConfirm: "确认删除默认路由？",
    deleteUserConfirm: "确认删除用户 {username}？",
    dialErrors: "连接上游失败",
    disabled: "禁用",
    edit: "编辑",
    enabled: "启用",
    failed: "失败",
    host: "主机",
    initialAdmin: "初始化管理员",
    language: "语言",
    languageChinese: "中文",
    languageEnglish: "English",
    login: "登录",
    logout: "退出登录",
    message: "消息",
    metrics: "指标",
    misses: "未命中",
    newRoute: "新建路由",
    newUser: "新建用户",
    noHits: "暂无命中",
    note: "备注",
    parityShards: "校验分片",
    password: "密码",
    path: "路径",
    port: "端口",
    protocols: "协议",
    restart: "重启",
    restartRequired: "需要重启",
    result: "结果",
    role: "角色",
    roleAdmin: "管理员",
    roleGuest: "访客",
    roleMember: "成员",
    route: "路由",
    routeHits: "路由命中",
    routeSearch: "搜索主机、上游、备注",
    routes: "路由",
    save: "保存",
    services: "服务",
    setupSubtitle: "初始化",
    success: "成功",
    target: "目标",
    time: "时间",
    total: "总数",
    upstream: "上游",
    uptime: "运行时间",
    user: "用户",
    username: "用户名",
    users: "用户",
  },
} as const;

type TranslationKey = keyof typeof translations.en;
type Language = keyof typeof translations;

const localizedMessages: Partial<Record<Language, Record<string, string>>> = {
  zh: {
    "admin API prefix cannot be under static asset path": "管理 API 前缀不能位于静态资源路径下",
    "admin API prefix cannot equal admin page path": "管理 API 前缀不能等于管理页面路径",
    "cannot delete the last enabled admin": "不能删除最后一个启用的管理员",
    "cannot remove the last enabled admin": "不能移除最后一个启用的管理员",
    "created initial admin": "已创建初始管理员",
    "host is required": "主机不能为空",
    "host must not contain /": "主机不能包含 /",
    "host must not contain whitespace": "主机不能包含空白字符",
    "initial admin has already been created": "初始管理员已创建",
    "invalid JSON": "JSON 无效",
    "invalid path segment": "路径片段无效",
    "invalid username or password": "用户名或密码无效",
    "login required": "需要登录",
    "login success": "登录成功",
    "logout success": "退出登录成功",
    "method not allowed": "方法不允许",
    "not found": "未找到",
    "password hash is required": "密码哈希不能为空",
    "password is required": "密码不能为空",
    "permission denied": "没有权限",
    "port must be an integer from 1 to 65535": "端口必须是 1 到 65535 之间的整数",
    "restart is not implemented": "暂未实现重启",
    "route deleted": "路由已删除",
    "route saved": "路由已保存",
    "service restart is not implemented; restart the gateway process": "暂未实现服务重启；请重启网关进程",
    "service saved": "服务已保存",
    "tcp_admin cannot be disabled": "tcp_admin 不能被禁用",
    "upstream host is required": "上游主机不能为空",
    "upstream is required": "上游不能为空",
    "upstream must be host:port": "上游必须是 host:port 格式",
    "upstream port must be an integer from 1 to 65535": "上游端口必须是 1 到 65535 之间的整数",
    "user created": "用户已创建",
    "user deleted": "用户已删除",
    "user is disabled": "用户已禁用",
    "user not found": "用户不存在",
    "user updated": "用户已更新",
    "username is required": "用户名不能为空",
    "username must not contain whitespace or /": "用户名不能包含空白字符或 /",
  },
};

const localizedPrefixes: Partial<Record<Language, Array<[string, string]>>> = {
  zh: [
    ["invalid JSON: ", "JSON 无效："],
    ["invalid role ", "无效角色 "],
    ["unknown service ", "未知服务 "],
  ],
};

export const auditActions: Partial<Record<Language, Record<string, string>>> = {
  zh: {
    login: "登录",
    logout: "退出登录",
    route_delete: "删除路由",
    route_upsert: "保存路由",
    service_restart: "重启服务",
    service_update: "更新服务",
    setup: "初始化",
    user_create: "创建用户",
    user_delete: "删除用户",
    user_patch: "更新用户",
  },
};

export const targetTypes: Partial<Record<Language, Record<string, string>>> = {
  zh: {
    route: "路由",
    service: "服务",
    session: "会话",
    user: "用户",
  },
};

export function initializeLanguage(): void {
  state.language = resolveInitialLanguage();
  applyLanguage();
}

export function changeLanguage(next: string, rerender: () => void): void {
  if (!isLanguage(next) || next === state.language) {
    return;
  }
  state.language = next;
  localStorage.setItem(languageStorageKey, next);
  applyLanguage();
  rerender();
}

export function applyLanguage(): void {
  document.documentElement.lang = state.language === "zh" ? "zh-CN" : "en";
  el<HTMLSelectElement>("languageSelect").value = state.language;
  document.querySelectorAll<HTMLElement>("[data-i18n]").forEach((node) => {
    node.textContent = t(node.dataset.i18n as TranslationKey);
  });
  document.querySelectorAll<HTMLInputElement>("[data-i18n-placeholder]").forEach((node) => {
    node.placeholder = t(node.dataset.i18nPlaceholder as TranslationKey);
  });
  if (!el("setupView").classList.contains("hidden")) {
    setSubtitle("setupSubtitle");
  } else if (!el("loginView").classList.contains("hidden")) {
    setSubtitle("login");
  } else {
    setSubtitle("adminSubtitle");
  }
}

export function setSubtitle(key: TranslationKey): void {
  el("subtitle").textContent = t(key);
}

export function t(key: TranslationKey, values: Record<string, string> = {}): string {
  const lang = isLanguage(state.language) ? state.language : defaultLanguage;
  let text: string = translations[lang][key] || translations[defaultLanguage][key] || key;
  for (const [name, value] of Object.entries(values)) {
    text = text.replaceAll(`{${name}}`, value);
  }
  return text;
}

export function localizeMessage(message: unknown): string {
  if (!message) {
    return "";
  }
  const lang = isLanguage(state.language) ? state.language : defaultLanguage;
  const text = String(message);
  const messages = localizedMessages[lang] || {};
  if (messages[text]) {
    return messages[text];
  }
  const prefixes = localizedPrefixes[lang] || [];
  for (const [source, replacement] of prefixes) {
    if (text.startsWith(source)) {
      return replacement + text.slice(source.length);
    }
  }
  return text;
}

export function formatAuditAction(action: string): string {
  return auditActions[state.language as Language]?.[action] || action;
}

export function formatTargetType(targetType: string): string {
  return targetTypes[state.language as Language]?.[targetType] || targetType;
}

function resolveInitialLanguage(): Language {
  const stored = localStorage.getItem(languageStorageKey);
  if (isLanguage(stored)) {
    return stored;
  }
  const browserLanguage = (navigator.language || "").toLowerCase();
  return browserLanguage.startsWith("zh") ? "zh" : defaultLanguage;
}

function isLanguage(value: unknown): value is Language {
  return typeof value === "string" && value in translations;
}
