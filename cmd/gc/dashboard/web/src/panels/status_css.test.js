import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const dashboardCss = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), "../../public/dashboard.css"),
  "utf8",
);

function scopeStatusRuleAt(maxWidth) {
  const mediaStart = dashboardCss.indexOf(`@media (max-width: ${maxWidth})`);
  expect(mediaStart).toBeGreaterThan(-1);
  const nextMediaStart = dashboardCss.indexOf("@media", mediaStart + 1);
  const mediaCss = dashboardCss.slice(
    mediaStart,
    nextMediaStart === -1 ? dashboardCss.length : nextMediaStart,
  );
  return mediaCss.match(/\.scope-status(?![A-Za-z0-9_-])\s*\{([\s\S]*?)\}/)?.[1] ?? "";
}

describe("status panel scope CSS", () => {
  it("allows five-stat city scope rows to wrap at narrow breakpoints", () => {
    for (const maxWidth of ["768px", "600px"]) {
      const rule = scopeStatusRuleAt(maxWidth);
      expect(rule).toContain("flex-wrap: wrap");
      expect(rule).toContain("row-gap:");
    }

    expect(scopeStatusRuleAt("600px")).toContain("column-gap: 12px");
  });
});
