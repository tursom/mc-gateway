import { el } from "./dom.js";
import { t } from "./i18n.js";
import { state } from "./state.js";
import type { Role } from "./types.js";

export function isAdmin(): boolean {
  return state.user?.role === "admin";
}

export function isMember(): boolean {
  return state.user?.role === "admin" || state.user?.role === "member";
}

export function renderSessionUser(): void {
  el("sessionUser").textContent = state.user ? `${state.user.username} (${formatRole(state.user.role)})` : "";
}

export function formatRole(role: Role | string): string {
  const keys: Partial<Record<string, "roleAdmin" | "roleMember" | "roleGuest">> = {
    admin: "roleAdmin",
    member: "roleMember",
    guest: "roleGuest",
  };
  const key = keys[role];
  return key ? t(key) : role;
}
