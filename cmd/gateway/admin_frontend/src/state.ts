import type { PluginArtifact, PluginBuild, PluginView, RouteRecord, ServiceRecord, User } from "./types.js";

export const tokenStorageKey = "mcGatewayAdminToken";
export const languageStorageKey = "mcGatewayAdminLanguage";

export interface AppState {
  apiBase: string;
  language: string;
  token: string;
  user: User | null;
  routes: RouteRecord[];
  services: ServiceRecord[];
  users: User[];
  plugins: PluginView[];
  pluginArtifacts: PluginArtifact[];
  pluginBuilds: PluginBuild[];
  selectedPluginID: string;
  selectedArtifactID: string;
}

export const state: AppState = {
  apiBase: "/admin/api",
  language: "",
  token: sessionStorage.getItem(tokenStorageKey) || "",
  user: null,
  routes: [],
  services: [],
  users: [],
  plugins: [],
  pluginArtifacts: [],
  pluginBuilds: [],
  selectedPluginID: "",
  selectedArtifactID: "",
};

export function setToken(token: string): void {
  state.token = token;
  if (token) {
    sessionStorage.setItem(tokenStorageKey, token);
  } else {
    sessionStorage.removeItem(tokenStorageKey);
  }
}
