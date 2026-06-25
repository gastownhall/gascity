import type { BeadGraph } from '../../lib/beadGraph';
import { selectColumns } from '../../lib/beadGraph';
import { BeadBoard } from './BeadBoard';
import type { BadgeSeverity } from '../../attention/compose';

// One rig's slice of the board (gascity-dashboard-6frc). The board groups
// by rig so a city with hundreds of beads stays parseable — each rig gets a
// headline + its own status columns, projected from the single shared graph
// (so cross-rig dependency edges still resolve). Sections are separated by
// whitespace and a hairline, never cards (DESIGN.md Flat Page Rule).

interface BeadBoardSectionProps {
  /** Rig display name. */
  label: string;
  /** Bead count in this rig (matched set). */
  count: number;
  /** The full graph; this section renders only its `ids`. */
  graph: BeadGraph;
  ids: ReadonlySet<string>;
  selectedId: string | null;
  attentionSeverity?: (beadId: string) => BadgeSeverity | null;
  onSelect: (beadId: string) => void;
}

export function BeadBoardSection({
  label,
  count,
  graph,
  ids,
  selectedId,
  attentionSeverity,
  onSelect,
}: BeadBoardSectionProps) {
  const columns = selectColumns(graph, ids);
  return (
    <section aria-label={label}>
      <header className="flex items-baseline justify-between border-b border-rule pb-2 mb-4">
        <h2 className="text-headline text-fg">{label}</h2>
        <span className="text-label tnum text-fg-muted">{count}</span>
      </header>
      <BeadBoard
        columns={columns}
        selectedId={selectedId}
        {...(attentionSeverity === undefined ? {} : { attentionSeverity })}
        onSelect={onSelect}
      />
    </section>
  );
}
