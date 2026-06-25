import { api } from "../api.js";
import { showAlert } from "../alerts.js";
import { badge, el, escapeAttr, escapeHTML, getFormInput, getFormSelect } from "../dom.js";
import { t } from "../i18n.js";
import { formatRole } from "../session.js";
import { state } from "../state.js";
import type { User } from "../types.js";

interface UsersResponse {
  users?: User[];
}

export async function loadUsers(): Promise<void> {
  try {
    const data = await api<UsersResponse>("/users");
    state.users = data.users || [];
    renderUsers();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

export function renderUsers(): void {
  el("usersBody").innerHTML = state.users.map((user) => `
    <tr>
      <td>${escapeHTML(user.username)}</td>
      <td>${escapeHTML(formatRole(user.role))}</td>
      <td>${badge(user.disabled ? t("disabled") : t("activeUser"), user.disabled)}</td>
      <td class="actions">
        <div class="row-actions">
          <button class="secondary" type="button" data-edit-user="${escapeAttr(user.username)}">${escapeHTML(t("edit"))}</button>
          <button class="danger" type="button" data-delete-user="${escapeAttr(user.username)}">${escapeHTML(t("delete"))}</button>
        </div>
      </td>
    </tr>
  `).join("");

  document.querySelectorAll<HTMLButtonElement>("[data-edit-user]").forEach((button) => {
    button.addEventListener("click", () => {
      const user = state.users.find((item) => item.username === button.dataset.editUser);
      openUserDialog(user || null);
    });
  });
  document.querySelectorAll<HTMLButtonElement>("[data-delete-user]").forEach((button) => {
    button.addEventListener("click", () => {
      const username = button.dataset.deleteUser;
      if (username) {
        removeUser(username);
      }
    });
  });
}

export function openUserDialog(user: User | null = null): void {
  const form = el<HTMLFormElement>("userForm");
  form.reset();
  form.dataset.originalUsername = user ? user.username : "";
  getFormInput(form, "username").disabled = Boolean(user);
  getFormInput(form, "password").required = !user;
  if (user) {
    getFormInput(form, "username").value = user.username;
    getFormSelect(form, "role").value = user.role;
    getFormInput(form, "disabled").checked = user.disabled;
  } else {
    getFormSelect(form, "role").value = "member";
  }
  el<HTMLDialogElement>("userDialog").showModal();
}

export async function saveUser(event: SubmitEvent): Promise<void> {
  event.preventDefault();
  const form = event.currentTarget as HTMLFormElement;
  const username = form.dataset.originalUsername || getFormInput(form, "username").value;
  const body: Record<string, unknown> = {
    role: getFormSelect(form, "role").value,
    disabled: getFormInput(form, "disabled").checked,
  };
  if (getFormInput(form, "password").value) {
    body.password = getFormInput(form, "password").value;
  }

  try {
    if (form.dataset.originalUsername) {
      await api(`/users/${encodeURIComponent(username)}`, { method: "PATCH", body });
    } else {
      body.username = username;
      await api("/users", { method: "POST", body });
    }
    el<HTMLDialogElement>("userDialog").close();
    await loadUsers();
  } catch (err) {
    showAlert((err as Error).message);
  }
}

async function removeUser(username: string): Promise<void> {
  if (!confirm(t("deleteUserConfirm", { username }))) {
    return;
  }
  try {
    await api(`/users/${encodeURIComponent(username)}`, { method: "DELETE" });
    await loadUsers();
  } catch (err) {
    showAlert((err as Error).message);
  }
}
