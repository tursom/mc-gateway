// cmd/gateway/admin_frontend/src/dom.ts 集中 DOM 辅助方法，包括查询、转义、徽标、去抖和表单取值。

export function el<T extends HTMLElement = HTMLElement>(id: string): T {
  const node = document.getElementById(id);
  if (!node) {
    throw new Error(`missing element #${id}`);
  }
  return node as T;
}

export function escapeHTML(value: unknown): string {
  return String(value).replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;",
  }[ch] || ch));
}

export function escapeAttr(value: unknown): string {
  return escapeHTML(value).replace(/`/g, "&#96;");
}

export function stat(label: string, value: unknown): string {
  return `<div class="stat"><span>${escapeHTML(label)}</span><strong>${escapeHTML(String(value ?? ""))}</strong></div>`;
}

export function badge(text: string, off = false): string {
  return `<span class="badge ${off ? "off" : ""}">${escapeHTML(text)}</span>`;
}

export function debounce<T extends (...args: unknown[]) => void>(fn: T, wait: number): T {
  let id = 0;
  return ((...args: Parameters<T>) => {
    clearTimeout(id);
    id = window.setTimeout(() => fn(...args), wait);
  }) as T;
}

export function getFormInput(form: HTMLFormElement, name: string): HTMLInputElement {
  const field = form.elements.namedItem(name);
  if (!(field instanceof HTMLInputElement)) {
    throw new Error(`missing input ${name}`);
  }
  return field;
}

export function getFormSelect(form: HTMLFormElement, name: string): HTMLSelectElement {
  const field = form.elements.namedItem(name);
  if (!(field instanceof HTMLSelectElement)) {
    throw new Error(`missing select ${name}`);
  }
  return field;
}
