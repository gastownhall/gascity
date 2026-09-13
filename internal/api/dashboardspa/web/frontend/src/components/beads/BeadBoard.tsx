import { BOARD_COLUMNS, type BeadNode, type BoardColumnId } from '../../lib/beadGraph';
import { BeadBoardRow } from './BeadBoardRow';
import type { BadgeSeverity } from '../../attention/compose';

// The Beads board: status columns (kanban) whose rows carry the dependency
// graph (gascity-dashboard-6frc). Editorial register — columns are
// separated by whitespace and a single hairline header rule, never by
// cards. The only maroon mark on the board is the blocked count when it
// crosses zero (an anomaly count, sanctioned by DESIGN.md §2); status is
// otherwise carried by the column a bead sits in, not by a per-row badge,
// so the page reads in greyscale.
//
// Takes already-grouped `columns` (rather than the whole graph) so the
// caller can render one board per rig from subsets of a single shared
// graph, keeping cross-rig dependency edges resolved.

interface BeadBoardProps {
  columns: Record<BoardColumnId, BeadNode[]>;
  selectedId: string | null;
  attentionSeverity?: (beadId: string) => BadgeSeverity | null;
  onSelect: (beadId: string) => void;
}

export function BeadBoard({ columns, selectedId, attentionSeverity, onSelect }: BeadBoardProps) {
  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-5 gap-x-8 gap-y-8">
      {BOARD_COLUMNS.map((col) => {
        const nodes = columns[col.id];
        const isBlocked = col.id === 'blocked';
        const countTone = isBlocked && nodes.length > 0 ? 'text-accent' : 'text-fg-muted';
        return (
          <section key={col.id} aria-label={col.label}>
            <header className="flex items-baseline justify-between border-b border-rule pb-2 mb-3">
              <h3 className="text-label uppercase tracking-wider text-fg-muted">{col.label}</h3>
              <span className={`text-label tnum ${countTone}`}>{nodes.length}</span>
            </header>
            {nodes.length === 0 ? (
              <p className="text-body text-fg-faint italic">·</p>
            ) : (
              <ul className="space-y-1">
                {nodes.map((node) => (
                  <BeadBoardRow
                    key={node.bead.id}
                    node={node}
                    selected={node.bead.id === selectedId}
                    attentionSeverity={attentionSeverity?.(node.bead.id) ?? null}
                    onSelect={onSelect}
                  />
                ))}
              </ul>
            )}
          </section>
        );
      })}
    </div>
  );
}
