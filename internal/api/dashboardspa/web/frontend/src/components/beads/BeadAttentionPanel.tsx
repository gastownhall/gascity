import type { AttentionItem } from '../../attention/compose';
import { Button } from '../Button';
import { StatusBadge } from '../StatusBadge';

// gascity-dashboard-2j8e.3: the /beads "Needs you" section — the on-page
// counterpart of the Beads nav badge. It renders the badge-counting attention
// items (attention + watch tiers, the same `summary.attention + summary.watch`
// the nav indicator shows), so the page count and the nav badge cannot
// disagree, and gives each item a path to act: Open the escalation / decision /
// ready-unclaimed bead to act on it. The operator does not claim beads — a bead
// assignee must be a concrete session, never the human operator
// (gascity-dashboard-2j8e.8) — so there is no inline Claim affordance.

interface BeadAttentionPanelProps {
  /** The beads-domain attention items from the composed model (any severity). */
  items: readonly AttentionItem[];
  onOpen: (beadId: string) => void;
}

/** Extract the `bead` query param from an attention item's `/beads?bead=…` href. */
export function beadIdFromHref(href: string | undefined): string | null {
  if (href === undefined) return null;
  const queryStart = href.indexOf('?');
  if (queryStart < 0) return null;
  const beadId = new URLSearchParams(href.slice(queryStart + 1)).get('bead');
  return beadId !== null && beadId.length > 0 ? beadId : null;
}

export function BeadAttentionPanel({ items, onOpen }: BeadAttentionPanelProps) {
  // The nav badge counts attention + watch; mirror that exactly so the counts agree.
  const counted = items.filter(
    (item) => item.severity === 'attention' || item.severity === 'watch',
  );
  if (counted.length === 0) return null;

  return (
    <section aria-labelledby="beads-attention-title" className="mb-8 space-y-3">
      <h2 id="beads-attention-title" className="text-label uppercase tracking-wider text-fg-muted">
        Needs you <span className="tnum text-fg">({counted.length})</span>
      </h2>
      <ul className="space-y-2">
        {counted.map((item) => {
          const beadId = beadIdFromHref(item.href);
          return (
            <li
              key={item.id}
              className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1"
            >
              <div className="min-w-0 space-y-0.5">
                <StatusBadge
                  tone={item.severity === 'attention' ? 'stuck' : 'warn'}
                  label={item.title}
                />
                {item.summary !== undefined && (
                  <p className="text-body text-fg-muted">{item.summary}</p>
                )}
              </div>
              {beadId !== null && (
                <div className="flex items-center gap-2">
                  <Button type="button" size="sm" tone="quiet" onClick={() => onOpen(beadId)}>
                    Open
                  </Button>
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
