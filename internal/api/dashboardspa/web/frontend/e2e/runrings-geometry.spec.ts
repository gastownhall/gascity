import type { Page, TestInfo } from '@playwright/test';

import { CITY_BASE } from './fixtures/expected';
import { gotoCityRoute } from './support/renderGuards';
import { expect, test } from './support/fixtures';

// Geometry regression guard for the cockpit "formula run progress" rings
// (components/cockpit/Instruments.tsx: RunRings). Reported production bug: long
// stage words ("Human approval", "Merge-ready", "repair-pre-approval-ci-
// failures") rendered shifted out of the ring circle — unbreakable words spilled
// past both edges, and breakable ones wrapped to fill the box and left-aligned.
// The in-circle overlay text centered flex ITEMS (items-center) but had no
// text-align, no width cap, and no truncation, so it neither centered wrapped
// lines nor contained an over-wide word.
//
// This spec drives the REAL SPA (served by the seeded fakesupervisor) but
// substitutes the run-summary payload with worst-case lanes, so the cockpit
// renders the longest realistic stage words + labels. It then asserts every
// ring's in-circle text stays within — and centered in — its own 80x80 ring
// container, staying a single line, and that the label under the ring stays in
// the ring's horizontal span. The rendered region + measurements are attached to
// the report for visual review.

// Real stage words/labels drawn from internal/runproj/phasemapping.go and the
// formula stage ladders — the short calm case up through the longest step labels
// that actually ship, plus a wisp-id label and a retry state.
interface WorstCase {
  id: string;
  label: string;
  stageWord: string;
  index: number;
  total: number;
  attempt?: number;
}
const WORST_CASES: WorstCase[] = [
  { id: 'wc-review', label: 'demo', stageWord: 'review', index: 6, total: 8 },
  { id: 'wc-impl', label: 'mol-adopt-pr-v2', stageWord: 'Implementation', index: 1, total: 8 },
  { id: 'wc-final', label: 'mol-review-changes-v2', stageWord: 'Finalization', index: 4, total: 8 },
  {
    id: 'wc-worktree',
    label: 'run-anchor-adopt',
    stageWord: 'Worktree / rebase',
    index: 2,
    total: 8,
  },
  {
    id: 'wc-prepctx',
    label: 'gcg-98635422',
    stageWord: 'prepare-review-context',
    index: 3,
    total: 8,
  },
  {
    id: 'wc-repair',
    label: 'orchestrate-adopt-pr-and-review-v2',
    stageWord: 'repair-pre-approval-ci-failures',
    index: 5,
    total: 12,
  },
  {
    id: 'wc-retry',
    label: 'mol-adopt-pr-v2',
    stageWord: 'Human approval',
    index: 7,
    total: 8,
    attempt: 12,
  },
];

/**
 * Rewrite the run-summary response so the cockpit renders worst-case rings. The
 * real lane is cloned as a schema-valid template, then the fields laneToRing()
 * maps into a ring model (label/stage/attempt) are overridden per case.
 */
async function injectWorstCaseRings(page: Page): Promise<void> {
  await page.route('**/runs/summary', async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as {
      lanes: unknown[];
      historicalLanes: unknown[];
      blockedLanes: unknown[];
      totalActive: number;
    };
    const template = (body.lanes[0] ?? body.historicalLanes[0]) as Record<string, unknown>;
    if (template === undefined) {
      await route.fulfill({ response });
      return;
    }
    const stageTemplate = (template['stages'] as unknown[])[0] as Record<string, unknown>;
    const lanes = WORST_CASES.map((wc) => {
      const lane = JSON.parse(JSON.stringify(template)) as Record<string, unknown>;
      lane['id'] = wc.id;
      lane['title'] = wc.label;
      lane['formula'] = { status: 'known', name: wc.label };
      lane['phase'] = 'implementation';
      lane['phaseLabel'] = wc.stageWord;
      lane['stages'] = Array.from({ length: wc.total }, (_, i) => ({ ...stageTemplate, index: i }));
      lane['progress'] = {
        status: 'active_step',
        stepId: wc.id,
        stage: { status: 'available', index: wc.index, key: wc.id, label: wc.stageWord },
        attempt:
          wc.attempt === undefined
            ? { status: 'unavailable', error: 'run step attempt unavailable' }
            : { status: 'available', value: wc.attempt },
      };
      return lane;
    });
    body.lanes = lanes;
    body.blockedLanes = [];
    body.totalActive = lanes.length;
    await route.fulfill({ response, json: body });
  });
}

interface Rect {
  left: number;
  right: number;
  top: number;
  bottom: number;
  width: number;
}
interface RingMeasurement {
  aria: string | null;
  stageWordText: string | null;
  labelText: string | null;
  ringBox: Rect;
  stageWord: Rect | null;
  label: Rect | null;
  numerator: Rect | null;
}

async function measureRings(page: Page): Promise<RingMeasurement[]> {
  return page.evaluate(() => {
    const rect = (el: Element): Rect => {
      const b = el.getBoundingClientRect();
      return { left: b.left, right: b.right, top: b.top, bottom: b.bottom, width: b.width };
    };
    const rings = [...document.querySelectorAll('[data-testid="run-rings"] > a')];
    return rings.map((a): RingMeasurement => {
      const ringBox = a.querySelector(':scope > span') as Element;
      const overlay = ringBox.querySelector(':scope > span') as Element;
      // The overlay stacks the "N/M" numerator (first span) above the stage-word
      // / "retry N" caption (last span) — the caption is the overflow-prone node.
      const overlaySpans = [...overlay.querySelectorAll(':scope > span')];
      const numerator = overlaySpans[0] ?? null;
      const stageWord = overlaySpans[overlaySpans.length - 1] ?? null;
      const label = a.querySelector(':scope > span:nth-of-type(2)');
      return {
        aria: a.getAttribute('aria-label'),
        stageWordText: stageWord?.textContent ?? null,
        labelText: label?.textContent ?? null,
        ringBox: rect(ringBox),
        stageWord: stageWord ? rect(stageWord) : null,
        label: label ? rect(label) : null,
        numerator: numerator ? rect(numerator) : null,
      };
    });
  }) as Promise<RingMeasurement[]>;
}

/** How far each text node escapes its ring container — attached for review. */
function overflowReport(measurements: readonly RingMeasurement[]) {
  const escape = (box: Rect, r: Rect | null) =>
    r === null
      ? null
      : {
          left: Number((box.left - r.left).toFixed(2)),
          right: Number((r.right - box.right).toFixed(2)),
          width: Number(r.width.toFixed(2)),
          boxWidth: Number(box.width.toFixed(2)),
        };
  return measurements.map((m) => ({
    stageWordText: m.stageWordText,
    labelText: m.labelText,
    stageWord: escape(m.ringBox, m.stageWord),
    numerator: escape(m.ringBox, m.numerator),
    label: escape(m.ringBox, m.label),
  }));
}

test.describe('cockpit run-ring geometry', () => {
  test('every ring text node stays within its ring container', async ({
    page,
  }, testInfo: TestInfo) => {
    await injectWorstCaseRings(page);
    await gotoCityRoute(page, CITY_BASE, '');

    const region = page.getByRole('region', { name: 'formula run progress' });
    await expect(region).toBeVisible();
    const rings = page.getByTestId('run-rings').getByRole('link');
    await expect(rings).toHaveCount(WORST_CASES.length);
    // Text metrics depend on the web font: wait for it so bounding boxes are
    // final and not measured against a fallback face.
    await page.evaluate(() => document.fonts.ready);

    await testInfo.attach('formula-run-progress', {
      body: await region.screenshot(),
      contentType: 'image/png',
    });
    const measurements = await measureRings(page);
    await testInfo.attach('ring-measurements', {
      body: JSON.stringify(overflowReport(measurements), null, 2),
      contentType: 'application/json',
    });

    const TOL = 1;
    for (const m of measurements) {
      const ringCenterX = (m.ringBox.left + m.ringBox.right) / 2;

      // The in-circle overlay text — the "N/M" numerator and the stage-word /
      // "retry N" caption — must stay fully inside the 80x80 ring container on
      // all four sides. A long unbreakable word ("Implementation") used to spill
      // out the left and right of the ring.
      const inCircle: [string, Rect | null][] = [
        ['numerator', m.numerator],
        ['stage word', m.stageWord],
      ];
      for (const [name, r] of inCircle) {
        if (r === null) continue;
        const who = `${name}${name === 'stage word' ? ` "${m.stageWordText}"` : ''}`;
        expect(r.left, `${who} escapes ring LEFT`).toBeGreaterThanOrEqual(m.ringBox.left - TOL);
        expect(r.right, `${who} escapes ring RIGHT`).toBeLessThanOrEqual(m.ringBox.right + TOL);
        expect(r.top, `${who} escapes ring TOP`).toBeGreaterThanOrEqual(m.ringBox.top - TOL);
        expect(r.bottom, `${who} escapes ring BOTTOM`).toBeLessThanOrEqual(m.ringBox.bottom + TOL);
        // Centered in the ring: the wrapped-text bug left-aligned the caption,
        // shifting it out of the circle even while its box stayed in bounds.
        const centerX = (r.left + r.right) / 2;
        expect(
          Math.abs(centerX - ringCenterX),
          `${who} is not horizontally centered in the ring`,
        ).toBeLessThanOrEqual(TOL);
      }

      // The caption stays a single line. A wrapped caption (the bug) is taller
      // than the single-line numerator beside it, so its rendered height is the
      // wrap tell that four-side containment alone can miss.
      if (m.stageWord && m.numerator) {
        expect(
          m.stageWord.bottom - m.stageWord.top,
          `stage word "${m.stageWordText}" wrapped to multiple lines`,
        ).toBeLessThanOrEqual(m.numerator.bottom - m.numerator.top + TOL);
      }

      // The run label under the ring stays within the ring's horizontal span
      // (it truncates with an ellipsis rather than pushing the column wider).
      if (m.label) {
        expect(m.label.left, `label "${m.labelText}" escapes ring LEFT`).toBeGreaterThanOrEqual(
          m.ringBox.left - TOL,
        );
        expect(m.label.right, `label "${m.labelText}" escapes ring RIGHT`).toBeLessThanOrEqual(
          m.ringBox.right + TOL,
        );
      }
    }
  });
});
