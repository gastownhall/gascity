import { useState, type ReactNode } from 'react';
import { resolveSessionForTarget } from 'gas-city-dashboard-shared';
import type { BeadNode } from '../lib/beadGraph';
import { useBeadDetail } from '../hooks/useBeadDetail';
import { useEntityLinks } from '../hooks/useEntityLinks';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { BeadBody } from './BeadBody';
import { BeadDependencies } from './beads/BeadDependencies';
import { BeadLiveRunModal } from './beads/BeadLiveRunModal';
import { Button } from './Button';
import { isSessionStreamable } from './LiveSessionPeek';
import { Modal } from './Modal';
import { RelatedEntities } from './RelatedEntities';

// Click-to-read modal for a single bead. Used from the Beads board
// and read-only drilldowns such as AgentDetail assigned-beads.
//
// Fetches direct supervisor bead detail on open. If the caller already has
// the full bead in state (Beads, AgentDetail), it can pass it via
// `initialBead` so the modal renders immediately and skips the network round
// trip.

interface BeadDetailModalProps {
  open: boolean;
  onClose: () => void;
  beadId: string | null;
  /** Optional pre-loaded bead. When present and complete, skips the fetch. */
  initialBead?: SupervisorBead | null;
  /**
   * Re-center the modal on a related bead (gascity-dashboard-j4x). When
   * omitted, related bead rows render as plain text (no in-place
   * navigation) so the modal can be used standalone.
   */
  onOpenBead?: (beadId: string) => void;
  /** Resolved graph node for the bead; when present, shows the dependency tree. */
  depNode?: BeadNode | null;
  /** Session list for assignee to live-run resolution; enables "View live run". */
  sessions?: readonly SupervisorSession[];
  /** Optional caller-owned action slot, used by Beads for direct supervisor writes. */
  renderActions?: (bead: SupervisorBead) => ReactNode;
}

export function BeadDetailModal({
  open,
  onClose,
  beadId,
  initialBead = null,
  onOpenBead,
  depNode = null,
  sessions,
  renderActions,
}: BeadDetailModalProps) {
  const { bead, loading, error, notFound, now } = useBeadDetail(open, beadId, initialBead);
  const links = useEntityLinks(open ? beadId : null);
  const [runOpen, setRunOpen] = useState(false);

  const session =
    bead && sessions && bead.assignee && bead.assignee.length > 0
      ? resolveSessionForTarget(bead.assignee, sessions)
      : null;
  const liveRunnable = isSessionStreamable(session);
  const actions = bead ? renderActions?.(bead) : undefined;
  const footer =
    actions || liveRunnable ? (
      <>
        {actions}
        {liveRunnable && (
          <Button size="sm" tone="quiet" onClick={() => setRunOpen(true)}>
            View live run
          </Button>
        )}
      </>
    ) : undefined;

  return (
    <>
      <Modal
        open={open}
        onClose={onClose}
        title={bead?.title ?? beadId ?? 'Bead'}
        caption={
          bead ? (
            <span>
              <code className="text-fg-muted">{bead.id}</code>
              {' · '}
              {bead.issue_type}
              {' · P'}
              {bead.priority == null ? '—' : bead.priority}
            </span>
          ) : beadId ? (
            <code className="text-fg-muted">{beadId}</code>
          ) : undefined
        }
        widthClass="max-w-3xl"
        footer={footer}
      >
        {notFound ? (
          <div className="space-y-2">
            <p className="text-fg-muted">This decision was resolved or removed.</p>
            <p className="text-fg-faint text-sm">
              The bead it pointed to is no longer in the supervisor — it was likely closed or pruned
              since this link was surfaced.
            </p>
          </div>
        ) : error ? (
          <p className="text-accent" role="alert">
            {error}
          </p>
        ) : loading && bead === null ? (
          <p className="text-fg-muted italic">Fetching bead.</p>
        ) : bead === null ? (
          <p className="text-fg-muted italic">No bead.</p>
        ) : (
          <div className="space-y-8">
            <BeadBody bead={bead} />
            {depNode && (
              <BeadDependencies
                node={depNode}
                {...(onOpenBead !== undefined ? { onOpenBead } : {})}
              />
            )}
            <RelatedEntities
              view={links.view}
              loading={links.loading}
              error={links.error}
              now={now}
              {...(onOpenBead !== undefined ? { onOpenBead } : {})}
            />
          </div>
        )}
      </Modal>

      {bead && (
        <BeadLiveRunModal
          open={runOpen}
          onClose={() => setRunOpen(false)}
          session={session}
          beadTitle={bead.title}
        />
      )}
    </>
  );
}
