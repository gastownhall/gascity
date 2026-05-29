import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import indexHTML from "../../index.html?raw";
import { api, type BeadRecord } from "../api";
import { renderIssues } from "./issues";

vi.mock("../api", () => ({
  api: {
    GET: vi.fn(),
    POST: vi.fn(),
  },
  cityScope: vi.fn(() => "test-city"),
  mutationHeaders: { "X-GC-Request": "true" },
}));

vi.mock("../ui", () => ({
  popPause: vi.fn(),
  pushPause: vi.fn(),
  showToast: vi.fn(),
}));

vi.mock("./options", () => ({
  getOptions: vi.fn(async () => ({
    agents: ["builder"],
    beads: [],
    fetchedAt: Date.now(),
    mail: [],
    rigs: ["test"],
    sessions: [],
  })),
}));

const getMock = api.GET as unknown as ReturnType<typeof vi.fn>;

async function waitFor(assertion: () => void | Promise<void>): Promise<void> {
  const deadline = Date.now() + 2_000;
  let lastError: unknown;
  while (Date.now() < deadline) {
    try {
      await assertion();
      return;
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw lastError;
}

function installDOM(): void {
  document.body.innerHTML = `
    <span id="issues-count"></span>
    <div id="rig-filter-tabs"></div>
    <div id="issues-list"></div>
    <div id="issue-detail" style="display: none;">
      <span id="issue-detail-priority" class="badge"></span>
      <span id="issue-detail-id" class="issue-id"></span>
      <span id="issue-detail-status" class="issue-status"></span>
      <h3 id="issue-detail-title-text"></h3>
      <div class="issue-detail-meta">
        <span id="issue-detail-type"></span>
        <span id="issue-detail-owner"></span>
        <span id="issue-detail-created"></span>
        <span id="issue-detail-updated"></span>
      </div>
      <div id="issue-detail-actions"></div>
      <pre id="issue-detail-description"></pre>
      <div id="issue-detail-deps" style="display: none;">
        <div id="issue-detail-depends-on"></div>
      </div>
      <div id="issue-detail-blocks-section" style="display: none;">
        <div id="issue-detail-blocks"></div>
      </div>
    </div>
  `;
}

function bead(overrides: Partial<BeadRecord> = {}): BeadRecord {
  return {
    created_at: "2026-05-27T12:00:00Z",
    id: "ga-demo",
    issue_type: "task",
    priority: 2,
    status: "open",
    title: "Demo bead",
    ...overrides,
  };
}

function mockIssue(issue: BeadRecord): void {
  getMock.mockImplementation(async (path: string, init?: { params?: { query?: { status?: string } } }) => {
    if (path === "/v0/city/{cityName}/beads") {
      const status = init?.params?.query?.status;
      return {
        data: { items: status === issue.status ? [issue] : [] },
        error: undefined,
        request: undefined,
        response: undefined,
      };
    }
    if (path === "/v0/city/{cityName}/bead/{id}") {
      return { data: issue, error: undefined, request: undefined, response: undefined };
    }
    if (path === "/v0/city/{cityName}/bead/{id}/deps") {
      return { data: { children: [] }, error: undefined, request: undefined, response: undefined };
    }
    throw new Error(`unexpected GET ${path}`);
  });
}

async function openDetail(issue: BeadRecord): Promise<void> {
  mockIssue(issue);
  await renderIssues();
  document.querySelector<HTMLElement>(".issue-row")?.click();
  await waitFor(() => {
    expect(document.getElementById("issue-detail-id")?.textContent).toBe(issue.id);
  });
}

describe("issue detail timestamps", () => {
  beforeEach(() => {
    getMock.mockReset();
    installDOM();
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("keeps the updated metadata slot immediately after created in source markup", () => {
    const template = document.createElement("template");
    template.innerHTML = indexHTML;

    const ids = [...template.content.querySelectorAll(".issue-detail-meta > span")]
      .map((node) => node.id);

    expect(ids).toEqual([
      "issue-detail-type",
      "issue-detail-owner",
      "issue-detail-created",
      "issue-detail-updated",
    ]);
  });

  it("renders created and updated timestamps with semantic datetimes", async () => {
    const createdAt = "2026-05-27T12:00:00Z";
    const updatedAt = "2026-05-27T12:05:02Z";

    await openDetail(bead({ created_at: createdAt, updated_at: updatedAt }));

    const created = document.getElementById("issue-detail-created");
    const updated = document.getElementById("issue-detail-updated");
    expect(created?.textContent).toContain("Created:");
    expect(created?.querySelector("time")?.getAttribute("datetime")).toBe(createdAt);
    expect(updated?.textContent).toContain("Updated:");
    expect(updated?.querySelector("time")?.getAttribute("datetime")).toBe(updatedAt);
  });

  it("hides updated when updated_at is absent", async () => {
    await openDetail(bead({ updated_at: undefined }));

    const updated = document.getElementById("issue-detail-updated");
    expect(updated?.textContent).toBe("");
    expect(updated?.querySelector("time")).toBeNull();
  });

  it("hides updated inside the one-second threshold", async () => {
    await openDetail(bead({
      created_at: "2026-05-27T12:00:00Z",
      updated_at: "2026-05-27T12:00:01Z",
    }));

    const updated = document.getElementById("issue-detail-updated");
    expect(updated?.textContent).toBe("");
    expect(updated?.querySelector("time")).toBeNull();
  });
});
