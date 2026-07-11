import { Link } from 'react-router-dom';
import type { AttentionItem } from '../../attention/compose';
import { elapsedSince } from '../../attention/elapsed';
import { agoStr } from './format';

// The needs-you strip below the header: the ONLY place maroon may appear on the
// cockpit home (DESIGN.md One Mark Rule). It surfaces the top actionable
// attention item (attention/watch severity) with an overflow count, or "nothing
// needs you" when the city is calm. The glyph + "needs you" label are wrapped in
// a single `.text-accent` element so the strip carries exactly one mark; the
// item link is not maroon.

export interface NeedsYouStripProps {
  items: readonly AttentionItem[];
}

export function NeedsYouStrip({ items }: NeedsYouStripProps) {
  const actionable = items.filter(
    (item) => item.severity === 'attention' || item.severity === 'watch',
  );
  const needsYou = actionable[0] ?? null;
  const moreCount = Math.max(0, actionable.length - 1);
  const needsYouAgeMs = elapsedSince(needsYou?.updatedAt, Date.now());

  return (
    <div
      data-testid="needs-you"
      className="-mt-5 mb-[1.2rem] flex min-h-[2.4rem] items-baseline gap-3 border-y border-rule py-2"
    >
      {needsYou !== null ? (
        <>
          <span className="inline-flex items-baseline gap-2 text-accent">
            <span aria-hidden>●</span>
            <span className="text-label uppercase tracking-wider">needs you</span>
          </span>
          {needsYou.href !== undefined ? (
            <Link to={needsYou.href} className="focus-mark text-body text-fg hover:underline">
              {needsYou.title}
            </Link>
          ) : (
            <span className="text-body text-fg">{needsYou.title}</span>
          )}
          {needsYouAgeMs !== null && (
            <span className="text-label tnum text-fg-faint">{agoStr(needsYouAgeMs)} ago</span>
          )}
          {moreCount > 0 && <span className="text-label tnum text-fg-muted">· {moreCount} more</span>}
        </>
      ) : (
        <p className="text-body italic text-fg-muted">nothing needs you</p>
      )}
    </div>
  );
}
