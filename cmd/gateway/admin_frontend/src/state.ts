import type { RouteRecord, ServiceRecord, User } from "./types.js";

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
}

export const state: AppState = {
  apiBase: "/admin/api",
  language: "",
  token: sessionStorage.getItem(tokenStorageKey) || "",
  user: null,
  routes: [],
  services: [],
  users: [],
};

export function setToken(token: string): void {
  state.token = token;
  if (token) {
    sessionStorage.setItem(tokenStorageKey, token);
  } else {
    sessionStorage.removeItem(tokenStorageKey);
  }
}
