import { byId } from "./util/dom";

let pauseCount = 0;

function setPauseCount(next: number): void {
  pauseCount = Math.max(0, next);
  document.body.dataset.pauseRefresh = pauseCount > 0 ? "true" : "false";
}

export function pushPause(): void {
  setPauseCount(pauseCount + 1);
}

export function popPause(): void {
  setPauseCount(pauseCount - 1);
}

export function refreshPaused(): boolean {
  return pauseCount > 0;
}

export function openOutput(title: string, content: string): void {
  const panel = byId("output-panel");
  const titleEl = byId("output-panel-cmd");
  const contentEl = byId("output-panel-content");
  if (!panel || !titleEl || !contentEl) return;
  titleEl.textContent = title;
  contentEl.textContent = content;
  panel.classList.add("open");
}

export function closeOutput(): void {
  byId("output-panel")?.classList.remove("open");
}

export function showToast(type: "success" | "error" | "info", title: string, message: string): void {
  const container = byId("toast-container");
  if (!container) return;
  const toast = document.createElement("div");
  toast.className = `toast toast-${type}`;
  toast.innerHTML = `<strong>${escapeHTML(title)}</strong><div>${escapeHTML(message)}</div>`;
  container.append(toast);
  window.setTimeout(() => {
    toast.remove();
  }, 3200);
}

export function installPanelAffordances(): void {
  document.addEventListener("click", (event) => {
    const target = event.target as HTMLElement | null;
    const collapseBtn = target?.closest(".collapse-btn") as HTMLElement | null;
    if (collapseBtn) {
      const panel = collapseBtn.closest(".panel");
      panel?.classList.toggle("collapsed");
      return;
    }

    const expandBtn = target?.closest(".expand-btn") as HTMLElement | null;
    if (!expandBtn) return;

    const panel = expandBtn.closest(".panel");
    if (!panel) return;
    const alreadyExpanded = panel.classList.contains("expanded");
    const hadExpanded = !!document.querySelector(".panel.expanded");
    document.querySelectorAll(".panel.expanded").forEach((p) => {
      p.classList.remove("expanded");
      const btn = p.querySelector(".expand-btn");
      if (btn) btn.textContent = "Expand";
    });
    if (alreadyExpanded) {
      popPause();
      return;
    }
    panel.classList.add("expanded");
    expandBtn.textContent = "✕ Close";
    if (!hadExpanded) pushPause();
  });

  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    const expanded = document.querySelector(".panel.expanded");
    if (expanded) {
      expanded.classList.remove("expanded");
      const btn = expanded.querySelector(".expand-btn");
      if (btn) btn.textContent = "Expand";
      popPause();
    }
  });

  byId("output-close-btn")?.addEventListener("click", () => closeOutput());
  byId("output-copy-btn")?.addEventListener("click", async () => {
    const text = byId("output-panel-content")?.textContent ?? "";
    try {
      await navigator.clipboard.writeText(text);
      showToast("success", "Copied", "Output copied to clipboard");
    } catch {
      showToast("error", "Copy failed", "Clipboard write was rejected");
    }
  });
}

export function escapeHTML(text: string): string {
  const div = document.createElement("div");
  div.textContent = text;
  return div.innerHTML;
}
