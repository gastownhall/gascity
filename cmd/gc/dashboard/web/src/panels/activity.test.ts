import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { renderActivity, seedActivity, type ActivityEntry } from "./activity";

describe("activity feed ordering", () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <div id="activity-filters"></div>
      <span id="activity-count"></span>
      <div id="activity-feed"></div>
    `;
  });

  afterEach(async () => {
    await seedActivity([]);
  });

  it("dedupes repeated events and keeps newest entries first", async () => {
    const oldEntry: ActivityEntry = {
      category: "work",
      id: "mc-city:12",
      rig: "city",
      scope: "mc-city",
      seq: 12,
      ts: "2026-04-01T10:00:00Z",
      type: "bead.created",
    };
    const newerEntry: ActivityEntry = {
      category: "work",
      id: "mc-city:13",
      rig: "city",
      scope: "mc-city",
      seq: 13,
      ts: "2026-04-02T10:00:00Z",
      type: "bead.updated",
    };
    const sameTimestampDifferentScope: ActivityEntry = {
      category: "system",
      id: "alpha-city:9",
      rig: "city",
      scope: "alpha-city",
      seq: 9,
      ts: "2026-04-02T10:00:00Z",
      type: "city.updated",
    };

    await seedActivity([oldEntry, newerEntry, { ...oldEntry }, sameTimestampDifferentScope]);
    renderActivity();

    const ids = [...document.querySelectorAll<HTMLElement>(".tl-entry")].map((node) => node.dataset.ts);
    expect(ids).toEqual([
      "2026-04-02T10:00:00Z",
      "2026-04-02T10:00:00Z",
      "2026-04-01T10:00:00Z",
    ]);
    expect(document.querySelectorAll(".tl-entry")).toHaveLength(3);
    expect(document.getElementById("activity-count")?.textContent).toBe("3");
  });
});
