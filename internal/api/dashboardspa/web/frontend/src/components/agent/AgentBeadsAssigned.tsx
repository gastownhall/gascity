import type { SupervisorBead } from '../../supervisor/beadReads';

export function AgentBeadsAssigned({
  beads,
  error,
  loading,
  onSelect,
}: {
  beads: ReadonlyArray<SupervisorBead>;
  error: string | null;
  loading: boolean;
  onSelect: (bead: SupervisorBead) => void;
}) {
  return (
    <section className="mb-12">
      <header className="flex items-baseline justify-between mb-4">
        <h2 className="text-label uppercase tracking-wider text-fg-faint">Beads assigned</h2>
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {loading ? '·' : beads.length}
        </span>
      </header>
      {error !== null ? (
        <p className="text-body text-accent" role="alert">
          {error}
        </p>
      ) : loading ? (
        <p className="text-body text-fg-muted italic">Loading beads.</p>
      ) : beads.length === 0 ? (
        <p className="text-body text-fg-muted italic">No beads assigned to this agent.</p>
      ) : (
        <ul className="space-y-2">
          {beads.map((b) => (
            <li key={b.id} className="flex items-baseline gap-3 min-w-0">
              <span className="text-label uppercase tracking-wider text-fg-faint tnum shrink-0">
                {b.id}
              </span>
              <button
                type="button"
                onClick={() => onSelect(b)}
                className="text-body text-fg hover:text-accent truncate min-w-0 text-left focus-mark"
                title={`Open ${b.id}`}
              >
                {b.title}
              </button>
              <span className="text-label uppercase tracking-wider text-fg-faint shrink-0">
                {b.status}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
