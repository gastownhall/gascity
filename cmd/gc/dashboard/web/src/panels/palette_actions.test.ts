import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { installSharedModals } from "../modals";
import { installCommandPalette } from "../palette";
import { syncCityScopeFromLocation } from "../state";
import { installAdminInteractions } from "./admin";
import { installConvoyInteractions } from "./convoys";
import { installIssueInteractions } from "./issues";
import { installMailInteractions } from "./mail";
import * as options from "./options";

describe("command palette action flows", () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <div id="toast-container"></div>
      <button id="open-palette-btn" type="button">Open</button>
      <div id="command-palette-overlay">
        <div class="command-palette">
          <input id="command-palette-input" />
          <div id="command-palette-results"></div>
        </div>
      </div>
      <div id="output-panel">
        <span id="output-panel-cmd"></span>
        <div id="output-panel-content"></div>
        <button id="output-close-btn" type="button"></button>
        <button id="output-copy-btn" type="button"></button>
      </div>

      <button id="compose-mail-btn" type="button">Compose</button>
      <button id="mail-back-btn" type="button">Back</button>
      <button id="compose-back-btn" type="button">Back</button>
      <button id="compose-cancel-btn" type="button">Cancel</button>
      <button id="mail-reply-btn" type="button">Reply</button>
      <button id="mail-send-btn" type="button">Send</button>
      <button id="mail-archive-btn" type="button">Archive</button>
      <button id="mail-toggle-unread-btn" type="button">Unread</button>
      <div id="mail-list"></div>
      <div id="mail-all" style="display:none"></div>
      <div id="mail-detail" style="display:none"></div>
      <div id="mail-compose" style="display:none">
        <select id="compose-to"></select>
        <input id="compose-subject" />
        <textarea id="compose-body"></textarea>
        <input id="compose-reply-to" />
        <span id="mail-compose-title"></span>
      </div>

      <button id="new-issue-btn" type="button">New issue</button>
      <div id="issue-modal" style="display:none">
        <div class="modal-backdrop"></div>
        <button id="issue-modal-close-btn" type="button"></button>
        <button id="issue-modal-cancel-btn" type="button"></button>
        <form id="issue-form">
          <input id="issue-title" />
          <textarea id="issue-description"></textarea>
          <select id="issue-priority"><option value="2">P2</option></select>
        </form>
      </div>
      <div id="issue-detail" style="display:none"></div>
      <div id="issues-list"></div>
      <div id="rig-filter-tabs"></div>
      <span id="issues-count"></span>
      <button id="issue-back-btn" type="button"></button>

      <button id="new-convoy-btn" type="button">New convoy</button>
      <div id="convoy-list"></div>
      <div id="convoy-detail" style="display:none"></div>
      <div id="convoy-create-form" style="display:none">
        <input id="convoy-create-name" />
        <input id="convoy-create-issues" />
      </div>
      <div id="convoy-add-issue-form" style="display:none"></div>
      <button id="convoy-back-btn" type="button"></button>
      <button id="convoy-create-back-btn" type="button"></button>
      <button id="convoy-create-cancel-btn" type="button"></button>
      <button id="convoy-create-submit-btn" type="button"></button>
      <button id="convoy-add-issue-btn" type="button"></button>
      <button id="convoy-add-issue-cancel" type="button"></button>
      <button id="convoy-add-issue-submit" type="button"></button>
      <input id="convoy-add-issue-input" />
      <span id="convoy-count"></span>

      <button id="open-assign-btn" type="button">Assign</button>
      <button id="clear-assigned-btn" type="button"></button>
      <div id="action-modal" style="display:none">
        <div class="modal-backdrop"></div>
        <button id="action-modal-close-btn" type="button"></button>
        <form id="action-form">
          <div id="action-bead-group"></div>
          <input id="action-bead-id" />
          <div id="action-bead-hint"></div>
          <label id="action-target-label"></label>
          <input id="action-target" />
          <div id="action-rig-group"></div>
          <input id="action-rig" />
          <div id="action-modal-help"></div>
          <datalist id="action-target-list"></datalist>
          <datalist id="action-rig-list"></datalist>
          <button id="action-modal-cancel-btn" type="button"></button>
          <button id="action-modal-submit-btn" type="submit"></button>
        </form>
        <div id="action-modal-title"></div>
      </div>
      <div id="confirm-modal" style="display:none">
        <div class="modal-backdrop"></div>
        <div id="confirm-modal-title"></div>
        <div id="confirm-modal-body"></div>
        <button id="confirm-modal-close-btn" type="button"></button>
        <button id="confirm-modal-cancel-btn" type="button"></button>
        <button id="confirm-modal-confirm-btn" type="button"></button>
      </div>
    `;
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    syncCityScopeFromLocation();
    vi.spyOn(options, "getOptions").mockResolvedValue({
      agents: ["mayor"],
      beads: [{ id: "gc-1", title: "Example" }],
      fetchedAt: Date.now(),
      mail: [],
      rigs: ["city"],
      sessions: [{ id: "mc-vv8", label: "mayor", recipient: "mayor" }],
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
  });

  it("opens the city-scoped forms from the command palette", async () => {
    installSharedModals();
    installAdminInteractions();
    installConvoyInteractions();
    installIssueInteractions();
    installMailInteractions();
    installCommandPalette({ refreshAll: vi.fn().mockResolvedValue(undefined) });

    await executePaletteCommand("new issue");
    expect((document.getElementById("issue-modal") as HTMLElement).style.display).toBe("block");

    (document.getElementById("issue-modal-close-btn") as HTMLButtonElement).click();
    await executePaletteCommand("new convoy");
    expect((document.getElementById("convoy-create-form") as HTMLElement).style.display).toBe("block");

    (document.getElementById("convoy-create-cancel-btn") as HTMLButtonElement).click();
    await executePaletteCommand("assign work");
    expect((document.getElementById("action-modal") as HTMLElement).style.display).toBe("flex");
  });
});

async function executePaletteCommand(query: string): Promise<void> {
  (document.getElementById("open-palette-btn") as HTMLButtonElement).click();
  const input = document.getElementById("command-palette-input") as HTMLInputElement;
  input.value = query;
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Enter" }));
  await Promise.resolve();
  await Promise.resolve();
}
