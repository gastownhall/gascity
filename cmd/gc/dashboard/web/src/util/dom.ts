// Tiny DOM helpers. The SPA intentionally avoids a framework, so
// these primitives replace what React/Vue would otherwise provide.
// Every render function builds a DocumentFragment imperatively and
// swaps it into a container in one operation.

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string | number | boolean | undefined> = {},
  children: (Node | string | null | undefined)[] = [],
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === false) continue;
    if (value === true) {
      node.setAttribute(key, "");
    } else {
      node.setAttribute(key, String(value));
    }
  }
  for (const child of children) {
    if (child == null) continue;
    node.append(typeof child === "string" ? document.createTextNode(child) : child);
  }
  return node;
}

export function clear(node: Element): void {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export function render(container: Element, ...nodes: (Node | string)[]): void {
  clear(container);
  for (const n of nodes) {
    container.append(typeof n === "string" ? document.createTextNode(n) : n);
  }
}

export function byId<T extends Element = HTMLElement>(id: string): T | null {
  return document.getElementById(id) as T | null;
}
