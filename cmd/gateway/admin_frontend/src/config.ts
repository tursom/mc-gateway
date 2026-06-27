// cmd/gateway/admin_frontend/src/config.ts 读取嵌入式管理端 HTML 壳注入的运行时配置。

import type { RuntimeConfig } from "./types.js";

declare global {
  interface Window {
    MCGatewayAdmin?: Partial<RuntimeConfig>;
  }
}

export function runtimeConfig(): RuntimeConfig {
  return {
    apiPrefix: window.MCGatewayAdmin?.apiPrefix || "/admin/api",
  };
}
