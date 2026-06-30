// cmd/gateway/admin_frontend/src/i18n.ts 保存嵌入式管理端翻译字典，并提供语言切换辅助方法。

import { el } from "./dom.js";
import { languageStorageKey, state } from "./state.js";

const defaultLanguage = "en";

const translations = {
  en: {
    activeConnections: "Active",
    activeUser: "Active",
    action: "Action",
    actions: "Actions",
    actor: "Actor",
    active: "Active",
    adminSubtitle: "Admin",
    artifact: "Artifact",
    audit: "Audit",
    cancel: "Cancel",
    createAdmin: "Create admin",
    dataShards: "Data shards",
    delete: "Delete",
    deleteDefaultRouteConfirm: "Delete default route?",
    deleteUserConfirm: "Delete user {username}?",
    desired: "Desired",
    desiredArtifact: "Desired artifact",
    documentTitle: "mc-gateway admin",
    dialErrors: "Dial errors",
    disabled: "Disabled",
    edit: "Edit",
    enabled: "Enabled",
    extensions: "Extensions",
    failed: "Failed",
    health: "Health",
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
    name: "Name",
    newRoute: "New route",
    newUser: "New user",
    noHits: "No hits",
    note: "Note",
    parityShards: "Parity shards",
    password: "Password",
    path: "Path",
    port: "Port",
    protocols: "Protocols",
    pluginID: "Plugin ID",
    plugins: "Plugins",
    priority: "Priority",
    refresh: "Refresh",
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
    runtime: "Runtime",
    save: "Save",
    services: "Services",
    setupSubtitle: "Setup",
    success: "Success",
    target: "Target",
    time: "Time",
    total: "Total",
    upstream: "Upstream",
    uploadPlugin: "Upload plugin",
    uptime: "Uptime",
    user: "User",
    username: "Username",
    users: "Users",
    version: "Version",
  },
  zh: {
    activeConnections: "活跃连接",
    activeUser: "启用",
    action: "动作",
    actions: "操作",
    actor: "操作者",
    active: "当前",
    adminSubtitle: "管理后台",
    artifact: "Artifact",
    audit: "审计",
    cancel: "取消",
    createAdmin: "创建管理员",
    dataShards: "数据分片",
    delete: "删除",
    deleteDefaultRouteConfirm: "确认删除默认路由？",
    deleteUserConfirm: "确认删除用户 {username}？",
    desired: "期望",
    desiredArtifact: "期望 Artifact",
    documentTitle: "mc-gateway 管理后台",
    dialErrors: "连接上游失败",
    disabled: "禁用",
    edit: "编辑",
    enabled: "启用",
    extensions: "扩展点",
    failed: "失败",
    health: "健康",
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
    name: "名称",
    newRoute: "新建路由",
    newUser: "新建用户",
    noHits: "暂无命中",
    note: "备注",
    parityShards: "校验分片",
    password: "密码",
    path: "路径",
    port: "端口",
    protocols: "协议",
    pluginID: "插件 ID",
    plugins: "插件",
    priority: "优先级",
    refresh: "刷新",
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
    runtime: "运行时",
    save: "保存",
    services: "服务",
    setupSubtitle: "初始化",
    success: "成功",
    target: "目标",
    time: "时间",
    total: "总数",
    upstream: "上游",
    uploadPlugin: "上传插件",
    uptime: "运行时间",
    user: "用户",
    username: "用户名",
    users: "用户",
    version: "版本",
  },
} as const;

type TranslationKey = keyof typeof translations.en;
type Language = keyof typeof translations;

const localizedMessages: Partial<Record<Language, Record<string, string>>> = {
  zh: {
    "admin API prefix cannot be under static asset path": "管理 API 前缀不能位于静态资源路径下",
    "admin API prefix cannot equal admin page path": "管理 API 前缀不能等于管理页面路径",
    "admin.auth.provider/v1 is reserved; local admin break-glass remains the implemented authentication path": "admin.auth.provider/v1 已预留；本地管理破窗账号仍是当前已实现的认证路径",
    "cannot delete the last enabled admin": "不能删除最后一个启用的管理员",
    "cannot remove the last enabled admin": "不能移除最后一个启用的管理员",
    "created initial admin": "已创建初始管理员",
    "gateway-managed listener lifecycle is implemented but disabled unless future runtime gates enable ingress": "网关托管监听生命周期已实现，但在未来运行时开关启用 ingress 前保持禁用",
    "go-plugin-process supports upstream.connect/v1 dialer mode and protocol-proxy drain-only with persisted crash policy and per-node crash isolation; fd-live migration, sandbox enforcement, full isolation, and non-Linux process-table orphan discovery are not implemented": "go-plugin-process 支持 upstream.connect/v1 拨号模式和 protocol-proxy 排空模式，包含持久化崩溃策略与单节点崩溃隔离；暂未实现 fd 热迁移、沙箱强制、完整隔离和非 Linux 进程表孤儿发现",
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
    "reserved auth provider": "预留认证提供方",
    "restart is not implemented": "暂未实现重启",
    "route deleted": "路由已删除",
    "route saved": "路由已保存",
    "ingress.service/v1 is reserved; schema and governance checks exist but gateway-managed listener data-plane is not enabled": "ingress.service/v1 已预留；schema 和治理检查存在，但网关托管监听数据面尚未启用",
    "sandbox-process enforcement is implemented but disabled unless future runtime gates enable sandbox_process": "sandbox-process 强制隔离已实现，但在未来运行时开关启用 sandbox_process 前保持禁用",
    "sandbox-process runtime requires sandbox-process service mode": "sandbox-process 运行时需要 sandbox-process 服务模式",
    "sandbox-process runtime is reserved; current gateway releases do not expose a sandbox data-plane": "sandbox-process 运行时已预留；当前网关版本不开放 sandbox 数据面",
    "sandbox-process service mode is disabled by future runtime gate": "sandbox-process 服务模式被未来运行时开关禁用",
    "sandbox-process service mode is implemented but disabled unless future runtime gates enable sandbox_process": "sandbox-process 服务模式已实现，但在未来运行时开关启用 sandbox_process 前保持禁用",
    "sandbox-process service mode is reserved; current data-plane modes are in-process and go-plugin-process": "sandbox-process 服务模式已预留；当前数据面模式是 in-process 和 go-plugin-process",
    "sandbox-process service mode supports sandbox-process and wasm runtimes": "sandbox-process 服务模式支持 sandbox-process 与 wasm 运行时",
    "sandbox-process wasm adapter is reserved; use in-process wasm for the current low-risk WASM data-plane": "sandbox-process wasm 适配器已预留；当前低风险 WASM 数据面使用 in-process wasm",
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
    "wasm runtime only supports low-risk extension points; protocol-proxy, network, file, and high-risk extension points are not supported": "wasm 运行时仅支持低风险扩展点；不支持 protocol-proxy、网络、文件和高风险扩展点",
    "username is required": "用户名不能为空",
    "username must not contain whitespace or /": "用户名不能包含空白字符或 /",
  },
};

const uiTranslations: Partial<Record<Language, Record<string, string>>> = {
  zh: {
    "Active": "当前",
    "Active mode": "生效模式",
    "Active proxy": "活跃代理",
    "Active proxy connections": "活跃代理连接",
    "Active support": "生效支持",
    "Adapter": "适配器",
    "Advisory ID": "公告 ID",
    "Artifact": "制品",
    "Artifact package": "制品包",
    "Backoff": "退避",
    "Baseline diff, e.g. 0.25": "基线差异，例如 0.25",
    "Benchmark": "基准",
    "Binding": "绑定",
    "Build-Time Instrumentation": "构建期埋点",
    "Builds": "构建",
    "Cancel": "取消",
    "Candidate ID": "候选 ID",
    "Check updates": "检查更新",
    "Config": "配置",
    "Config rollback": "配置回滚",
    "Conformance": "合规",
    "Control": "控制",
    "Create desired state": "创建期望状态",
    "Create disabled plugin": "创建禁用插件",
    "Crash policy": "崩溃策略",
    "Crashes": "崩溃次数",
    "Cross-node apply": "跨节点应用",
    "Data plane": "数据面",
    "Decision": "决策",
    "Delete": "删除",
    "Desired": "期望",
    "Desired artifact": "期望制品",
    "Desired maturity": "期望成熟度",
    "Desired mode": "期望模式",
    "Desired support": "期望支持",
    "Diagnostic": "诊断",
    "Diff": "差异",
    "Disable": "禁用",
    "Dispatch plan": "分发计划",
    "Drain": "排空",
    "Drop dead letters": "丢弃死信",
    "Dry run": "试运行",
    "Dry-run": "试运行",
    "Effective data plane": "有效数据面",
    "Enable": "启用",
    "Error": "错误",
    "Extension": "扩展",
    "Extension status": "扩展状态",
    "Extensions": "扩展点",
    "Failure state": "失败状态",
    "Failed": "失败",
    "Fixture gate": "夹具门禁",
    "Full rollback": "完整回滚",
    "GC dry-run": "GC 试运行",
    "Gates": "门禁",
    "Generation": "代次",
    "Go/API": "Go/API",
    "Governance": "治理",
    "Health": "健康",
    "Hot reload": "热重载",
    "Lifecycle": "生命周期",
    "Limits": "限制",
    "Load": "加载",
    "Loaded": "已加载",
    "Last error": "最后错误",
    "Manifest": "清单",
    "Maturity": "成熟度",
    "Migration": "迁移",
    "Minecraft": "Minecraft",
    "Mode": "模式",
    "Modes": "模式",
    "Name": "名称",
    "No active proxy connections": "暂无活跃代理连接",
    "No artifacts": "暂无制品",
    "No builds": "暂无构建",
    "No governance issues": "暂无治理问题",
    "No instrumentation metadata": "暂无埋点元数据",
    "No node runtime state": "暂无节点运行态",
    "No rollout state": "暂无发布状态",
    "No secrets": "暂无密钥",
    "No snapshots": "暂无快照",
    "Nodes": "节点",
    "Only binary artifacts can be used as plugin desired state.": "只有二进制制品可用作插件期望状态。",
    "Open": "打开",
    "Operations": "运维",
    "Override": "豁免",
    "Path": "路径",
    "Plugin": "插件",
    "Plugin Service": "插件服务",
    "Policy": "策略",
    "Preflight": "预检",
    "Priority": "优先级",
    "Profile": "配置档",
    "Reason": "原因",
    "Refresh": "刷新",
    "Refresh routes": "刷新路由",
    "Reload policy": "重载策略",
    "Reload required": "需要重载",
    "Replay dead letters": "重放死信",
    "Repository": "仓库",
    "Restart": "重启",
    "Retry": "重试",
    "Review": "评审",
    "Revoke artifact": "撤销制品",
    "Risk": "风险",
    "Rollback": "回滚",
    "Rollout": "发布",
    "Run": "运行",
    "Runtime": "运行时",
    "Runtime maturity": "运行时成熟度",
    "Runtime type": "运行时类型",
    "Save config": "保存配置",
    "Scope": "范围",
    "Secret name": "密钥名称",
    "Secrets": "密钥",
    "Self-test": "自检",
    "Service": "服务",
    "Set desired": "设置期望",
    "Smoke": "冒烟",
    "Snapshots": "快照",
    "Started": "启动时间",
    "State": "状态",
    "Status": "状态",
    "Stale": "过期",
    "Supported extensions": "支持的扩展点",
    "Type": "类型",
    "unknown": "未知",
    "Update secret": "更新密钥",
    "Uploaded artifacts": "已上传制品",
    "Use artifact": "使用制品",
    "Value visibility": "值可见性",
    "Versions": "版本",
    "active": "活跃",
    "active and desired generation unchanged": "当前和期望代次保持不变",
    "allowed": "允许",
    "applied": "已应用",
    "artifact": "制品",
    "available": "可用",
    "abi": "ABI",
    "binary": "二进制",
    "blocked": "已阻断",
    "canceled": "已取消",
    "config-only and full desired rollback": "支持仅配置和完整期望回滚",
    "current/previous summaries only": "仅展示当前/上一版本摘要",
    "data plane": "数据面",
    "desired pending": "期望待应用",
    "desired mode is": "期望模式为",
    "disabled": "已禁用",
    "draining": "排空中",
    "dry-run and governance rechecked": "重新执行试运行和治理检查",
    "enabled": "已启用",
    "error": "错误",
    "failed": "失败",
    "file": "文件",
    "future desired only": "仅未来期望",
    "healthy": "健康",
    "high": "高",
    "hot": "热更新",
    "hot reload": "热重载",
    "idle": "空闲",
    "implemented": "已实现",
    "info": "信息",
    "internal": "内部",
    "isolated": "已隔离",
    "local-content-store": "本地内容存储",
    "loadable": "可加载",
    "loaded": "已加载",
    "low": "低",
    "manual": "手动",
    "medium": "中",
    "missing": "缺失",
    "no": "否",
    "no current data plane": "无当前数据面",
    "no data plane": "无数据面",
    "not configured": "未配置",
    "not implemented": "未实现",
    "not loaded": "未加载",
    "not managed": "未纳管",
    "not required": "不需要",
    "not used": "未使用",
    "official": "官方",
    "ok": "正常",
    "partial": "部分实现",
    "partial failure": "部分失败",
    "pending": "待处理",
    "policy": "策略",
    "prev": "上一版",
    "provider": "提供方",
    "queued": "排队中",
    "redacted in API, audit, operations and diagnostics": "在 API、审计、运维和诊断中脱敏",
    "reload required": "需要重载",
    "required": "需要",
    "reserved": "预留",
    "restart": "重启",
    "restart required": "需要重启",
    "running": "运行中",
    "sensitive values redacted": "敏感值已脱敏",
    "service": "服务",
    "source": "源码",
    "stale": "过期",
    "stub": "占位",
    "succeeded": "成功",
    "uploaded": "已上传",
    "url": "URL",
    "used": "已使用",
    "value": "值",
    "vendor": "vendor",
    "version": "版本",
    "warning": "警告",
    "yes": "是",
    "Current data plane remains": "当前数据面保持为",
    "Future desired mode only; current data plane is unchanged.": "仅作为未来期望模式；当前数据面不变。",
    "future desired only until the service mode is applied; current data plane is unchanged.": "在服务模式应用前仅作为未来期望；当前数据面不变。",
    "JSON · schema · secret refs · ReloadConfig": "JSON · Schema · 密钥引用 · ReloadConfig",
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
  document.title = t("documentTitle");
  el<HTMLSelectElement>("languageSelect").value = state.language;
  document.querySelectorAll<HTMLElement>("[data-i18n]").forEach((node) => {
    node.textContent = t(node.dataset.i18n as TranslationKey);
  });
  document.querySelectorAll<HTMLInputElement>("[data-i18n-placeholder]").forEach((node) => {
    node.placeholder = t(node.dataset.i18nPlaceholder as TranslationKey);
  });
  document.querySelectorAll<HTMLElement>("[data-i18n-aria-label]").forEach((node) => {
    node.setAttribute("aria-label", t(node.dataset.i18nAriaLabel as TranslationKey));
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

export function ui(text: string): string {
  const lang = isLanguage(state.language) ? state.language : defaultLanguage;
  return uiTranslations[lang]?.[text] || text;
}

export function yesNo(value: boolean): string {
  return ui(value ? "yes" : "no");
}

export function requiredText(value: boolean): string {
  return ui(value ? "required" : "not required");
}

export function formatValue(value: unknown): string {
  if (typeof value !== "string") {
    return String(value ?? "");
  }
  return ui(value);
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
  return ui(text);
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
