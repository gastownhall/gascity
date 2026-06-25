import type { BeadNode } from '../../lib/beadGraph';

// The needs/blocks dependency view for a single bead (gascity-dashboard-14s1).
// Lifted out of the board row, where the tree was cramped into a kanban
// column, into the detail modal where it has room to read. Upstream
// (`needs`) and downstream (`blocks`) are separate subsections; each target
// is a button that re-centres the modal on that bead when it is inside the
// fetched window, or a typeset `unresolved` mark when it is not.

interface BeadDependenciesProps {
  /** The selected bead's resolved graph node (deps + blocks). */
  node: BeadNode;
  /** Re-centre the detail on a related bead. */
  onOpenBead?: (beadId: string) => void;
}

export function BeadDependencies({ node, onOpenBead }: BeadDependenciesProps) {
  const { deps, blocks } = node;
  const hasAny = deps.length > 0 || blocks.length > 0;

  return (
    <section>
      <h3 className="text-label uppercase tracking-wider text-fg-faint mb-3">Dependencies</h3>
      {!hasAny ? (
        <p className="text-body text-fg-muted italic">No dependencies.</p>
      ) : (
        <div className="space-y-6">
          {deps.length > 0 && (
            <div>
              <p className="text-label uppercase tracking-wider text-fg-muted mb-2">
                Needs <span className="tnum">{deps.length}</span>
              </p>
              <ul className="space-y-1">
                {deps.map((dep) => (
                  <DepLine
                    key={`needs-${dep.id}`}
                    relation={dep.kind === 'needs' ? null : dep.kind}
                    targetId={dep.id}
                    targetTitle={dep.bead?.title ?? null}
                    {...(dep.bead && onOpenBead ? { onOpenBead } : {})}
                  />
                ))}
              </ul>
            </div>
          )}
          {blocks.length > 0 && (
            <div>
              <p className="text-label uppercase tracking-wider text-fg-muted mb-2">
                Blocks <span className="tnum">{blocks.length}</span>
              </p>
              <ul className="space-y-1">
                {blocks.map((b) => (
                  <DepLine
                    key={`blocks-${b.id}`}
                    relation={null}
                    targetId={b.id}
                    targetTitle={b.title}
                    {...(onOpenBead ? { onOpenBead } : {})}
                  />
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

interface DepLineProps {
  /** Non-`needs` edge kind (e.g. a typed dependency); null for plain edges. */
  relation: string | null;
  targetId: string;
  targetTitle: string | null;
  /** Undefined when the target is outside the fetched window (unresolved). */
  onOpenBead?: (beadId: string) => void;
}

function DepLine({ relation, targetId, targetTitle, onOpenBead }: DepLineProps) {
  const label = (
    <>
      {relation && (
        <span className="text-label uppercase tracking-wider text-fg-faint">{relation} </span>
      )}
      <span className="tnum text-fg-muted">{targetId}</span>
      {targetTitle && <span className="text-fg"> · {targetTitle}</span>}
    </>
  );

  return (
    <li className="text-body leading-snug">
      {onOpenBead ? (
        <button
          type="button"
          onClick={() => onOpenBead(targetId)}
          className="text-left text-fg-muted hover:text-fg focus-mark rounded-sm"
          title={`Open ${targetId}`}
        >
          {label}
        </button>
      ) : (
        <span title="Outside the fetched window">
          {label} <span className="text-warn text-label uppercase tracking-wider">unresolved</span>
        </span>
      )}
    </li>
  );
}
