import { byId, clear, el } from "./util/dom";
import { getOptions } from "./panels/options";
import { popPause, pushPause, reportUIError } from "./ui";

interface ActionDialogConfig {
  beadID?: string;
  beadLabel?: string;
  initialRig?: string;
  initialTarget?: string;
  mode: "assign" | "reassign" | "sling";
  title: string;
}

interface ActionDialogResult {
  beadID: string;
  rig: string;
  target: string;
}

interface ConfirmDialogConfig {
  body: string;
  confirmLabel: string;
  title: string;
}

let actionResolver: ((result: ActionDialogResult | null) => void) | null = null;
let confirmResolver: ((confirmed: boolean) => void) | null = null;

export function installSharedModals(): void {
  byId("action-modal-close-btn")?.addEventListener("click", () => closeActionModal(null));
  byId("action-modal-cancel-btn")?.addEventListener("click", () => closeActionModal(null));
  byId("action-modal")?.querySelector(".modal-backdrop")?.addEventListener("click", () => closeActionModal(null));
  byId("action-form")?.addEventListener("submit", (event) => {
    event.preventDefault();
    const beadID = byId<HTMLInputElement>("action-bead-id")?.value.trim() ?? "";
    const target = byId<HTMLInputElement>("action-target")?.value.trim() ?? "";
    const rig = byId<HTMLInputElement>("action-rig")?.value.trim() ?? "";
    if (!beadID || !target) return;
    closeActionModal({ beadID, rig, target });
  });

  byId("confirm-modal-close-btn")?.addEventListener("click", () => closeConfirmModal(false));
  byId("confirm-modal-cancel-btn")?.addEventListener("click", () => closeConfirmModal(false));
  byId("confirm-modal-confirm-btn")?.addEventListener("click", () => closeConfirmModal(true));
  byId("confirm-modal")?.querySelector(".modal-backdrop")?.addEventListener("click", () => closeConfirmModal(false));

  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    if (isModalOpen("action-modal")) {
      closeActionModal(null);
      return;
    }
    if (isModalOpen("confirm-modal")) {
      closeConfirmModal(false);
    }
  });
}

export async function promptActionDialog(config: ActionDialogConfig): Promise<ActionDialogResult | null> {
  const modal = byId("action-modal");
  const form = byId<HTMLFormElement>("action-form");
  const title = byId("action-modal-title");
  const submit = byId<HTMLButtonElement>("action-modal-submit-btn");
  const beadGroup = byId("action-bead-group");
  const beadInput = byId<HTMLInputElement>("action-bead-id");
  const beadHint = byId("action-bead-hint");
  const targetInput = byId<HTMLInputElement>("action-target");
  const targetLabel = byId("action-target-label");
  const rigGroup = byId("action-rig-group");
  const rigInput = byId<HTMLInputElement>("action-rig");
  const help = byId("action-modal-help");
  const targetList = byId("action-target-list");
  const rigList = byId("action-rig-list");
  if (
    !modal || !form || !title || !submit || !beadGroup || !beadInput ||
    !beadHint || !targetInput || !targetLabel || !rigGroup || !rigInput ||
    !help || !targetList || !rigList
  ) {
    reportUIError("Action modal unavailable", new Error("missing action modal DOM"));
    return null;
  }

  const options = await getOptions();
  populateDatalist(targetList, options.agents);
  populateDatalist(rigList, options.rigs);

  title.textContent = config.title;
  submit.textContent = submitLabel(config.mode);
  targetLabel.textContent = config.mode === "reassign" ? "Assignee" : "Target agent or pool";
  help.textContent = helpText(config.mode);

  beadInput.value = config.beadID ?? "";
  beadInput.readOnly = Boolean(config.beadID);
  beadGroup.classList.toggle("readonly", beadInput.readOnly);
  beadHint.textContent = config.beadLabel ?? "";

  targetInput.value = config.initialTarget ?? "";
  rigInput.value = config.initialRig ?? "";
  rigGroup.hidden = config.mode === "reassign";
  rigInput.disabled = config.mode === "reassign";

  if (!isModalOpen("action-modal")) pushPause();
  modal.style.display = "flex";

  window.setTimeout(() => {
    if (config.beadID) {
      targetInput.focus();
      return;
    }
    beadInput.focus();
  }, 0);

  return new Promise<ActionDialogResult | null>((resolve) => {
    actionResolver = resolve;
  });
}

export async function promptConfirmDialog(config: ConfirmDialogConfig): Promise<boolean> {
  const modal = byId("confirm-modal");
  const title = byId("confirm-modal-title");
  const body = byId("confirm-modal-body");
  const confirm = byId("confirm-modal-confirm-btn");
  if (!modal || !title || !body || !confirm) {
    reportUIError("Confirm modal unavailable", new Error("missing confirm modal DOM"));
    return false;
  }

  title.textContent = config.title;
  body.textContent = config.body;
  confirm.textContent = config.confirmLabel;
  if (!isModalOpen("confirm-modal")) pushPause();
  modal.style.display = "flex";

  return new Promise<boolean>((resolve) => {
    confirmResolver = resolve;
  });
}

function populateDatalist(node: Element, values: string[]): void {
  clear(node);
  values.forEach((value) => {
    node.append(el("option", { value }));
  });
}

function submitLabel(mode: ActionDialogConfig["mode"]): string {
  switch (mode) {
    case "assign":
      return "Assign";
    case "reassign":
      return "Reassign";
    default:
      return "Sling";
  }
}

function helpText(mode: ActionDialogConfig["mode"]): string {
  switch (mode) {
    case "assign":
      return "Launch a bead directly to a target, with an optional rig override.";
    case "reassign":
      return "Pick a new assignee from the active city sessions or type one manually.";
    default:
      return "Dispatch this bead to a target, with an optional rig constraint.";
  }
}

function closeActionModal(result: ActionDialogResult | null): void {
  const modal = byId("action-modal");
  const form = byId<HTMLFormElement>("action-form");
  if (!modal || !form) return;
  const wasOpen = isModalOpen("action-modal");
  modal.style.display = "none";
  form.reset();
  byId<HTMLInputElement>("action-rig")!.disabled = false;
  byId<HTMLInputElement>("action-bead-id")!.readOnly = false;
  if (wasOpen) popPause();
  actionResolver?.(result);
  actionResolver = null;
}

function closeConfirmModal(confirmed: boolean): void {
  const modal = byId("confirm-modal");
  if (!modal) return;
  const wasOpen = isModalOpen("confirm-modal");
  modal.style.display = "none";
  if (wasOpen) popPause();
  confirmResolver?.(confirmed);
  confirmResolver = null;
}

function isModalOpen(id: string): boolean {
  return byId(id)?.style.display === "flex";
}
