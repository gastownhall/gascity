import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api";
import { syncCityScopeFromLocation } from "../state";
import { installCrewInteractions, renderCrew } from "./crew";

vi.mock("../sse", () => ({
  connectAgentOutput: vi.fn(() => ({ close: vi.fn() })),
}));

describe("crew empty states", () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none"></div>
    `;
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    syncCityScopeFromLocation();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
  });

  it("shows no crew configured when the city has zero crew sessions", async () => {
    vi.spyOn(api, "GET").mockImplementation(async (path: string) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return { data: { items: [] } } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    await renderCrew();

    expect((document.getElementById("crew-empty") as HTMLElement).style.display).toBe("block");
    expect(document.getElementById("crew-empty")?.textContent).toContain("No crew configured");
    expect(document.getElementById("crew-empty")?.textContent).not.toContain("Select a city");
  });

  it("loads older transcript pages without losing the drawer loading sentinel", async () => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none">
        <span id="log-drawer-agent-name"></span>
        <span id="log-drawer-count"></span>
        <button id="log-drawer-older-btn" style="display:none">Load older</button>
        <button id="log-drawer-close-btn">Close</button>
        <div id="log-drawer-body">
          <div id="log-drawer-messages">
            <div id="log-drawer-loading">Loading logs...</div>
          </div>
        </div>
      </div>
    `;
    const transcriptQueries: Array<Record<string, string | undefined>> = [];
    vi.spyOn(api, "GET").mockImplementation(async (path: string, options?: unknown) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [{
              active_bead: "",
              attached: true,
              id: "s-reviewer",
              last_active: "2026-04-18T20:00:00Z",
              last_output: "",
              pool: "review",
              rig: "rig-a",
              running: true,
              template: "reviewer",
            }],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/transcript") {
        const query = (options as { params?: { query?: Record<string, string | undefined> } } | undefined)?.params?.query ?? {};
        transcriptQueries.push(query);
        if (query.before) {
          return {
            data: {
              turns: [{ role: "assistant", text: "Older transcript turn", timestamp: "2026-04-18T19:00:00Z" }],
              pagination: {
                has_older_messages: false,
                returned_message_count: 1,
                total_compactions: 0,
                total_message_count: 3,
              },
            },
          } as never;
        }
        return {
          data: {
            turns: [{ role: "assistant", text: "Newest transcript turn", timestamp: "2026-04-18T20:00:00Z" }],
            pagination: {
              has_older_messages: true,
              returned_message_count: 1,
              total_compactions: 0,
              total_message_count: 3,
              truncated_before_message: "cursor-1",
            },
          },
        } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    installCrewInteractions();
    await renderCrew();
    document.querySelector<HTMLButtonElement>(".agent-log-link")?.click();
    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Newest transcript turn");
    });

    expect(document.getElementById("log-drawer-loading")).not.toBeNull();
    document.getElementById("log-drawer-older-btn")?.click();
    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Older transcript turn");
    });

    expect(transcriptQueries.map((query) => query.before)).toEqual([undefined, "cursor-1"]);
    expect(document.getElementById("log-drawer-loading")).not.toBeNull();
  });
});

// Slow Blacksmith CI runs have shown the openLogDrawer + loadTranscript
// chain take ~1.3s while passing runs finish in ~100ms — same VM class,
// same code. The 1s budget here was missing those slow runs by a few
// hundred ms even though the chain ultimately completed (the
// `[crew] Transcript loaded` debug log fires *after* the assertion times
// out). Five seconds keeps the local cost negligible and absorbs the
// observed CI variance.
async function waitFor(assertion: () => void): Promise<void> {
  const started = Date.now();
  let lastError: unknown;
  while (Date.now() - started < 5000) {
    try {
      assertion();
      return;
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw lastError;
}
