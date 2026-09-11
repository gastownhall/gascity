import { Fragment } from 'react';
import { diffLineKind, type DiffLineKind } from 'gas-city-dashboard-shared';

// Colorized unified-diff renderer. The text comes pre-built from the pure
// layer (`toolResultSections(block).diff`, itself fed by `patchTextFromHunks`);
// this component only splits it into per-line `<span>`s and maps each line's
// semantic kind to a Tailwind tone. The SPA already themes diff insert→--ok and
// delete→--warn, so the add/del tones below match the editor's diff palette.

// Semantic line kind → Tailwind tone. The classification (and its load-bearing
// top-down ordering, e.g. `+++`/`---` file headers before single `+`/`-`) lives
// in `diffLineKind`; this map is purely the visual projection.
const DIFF_LINE_TONE: Record<DiffLineKind, string> = {
  add: 'text-ok',
  del: 'text-warn',
  file: 'text-fg-faint',
  hunk: 'text-fg-muted',
  context: 'text-fg',
};

/**
 * Render unified-diff text as one classed `<span>` per line inside a
 * `whitespace-pre-wrap` `<pre>`, with a literal `\n` text node between lines —
 * the old `renderDiffPre` model. That keeps the `<pre>`'s textContent equal to
 * the original diff (so a selected diff copies with its line breaks) while each
 * line's tone is derived from `diffLineKind`, reproducing the old dashboard's
 * diff coloring without the `log-msg-diff-*` BEM classes. Splitting on `\n`
 * (after `\r\n` normalization) keeps blank lines as empty spans.
 */
export function DiffView({ text }: { text: string }) {
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  return (
    <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto">
      {lines.map((line, index) => (
        <Fragment key={index}>
          <span className={DIFF_LINE_TONE[diffLineKind(line)]}>{line}</span>
          {index < lines.length - 1 ? '\n' : null}
        </Fragment>
      ))}
    </pre>
  );
}
