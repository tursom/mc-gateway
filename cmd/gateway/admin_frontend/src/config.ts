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
