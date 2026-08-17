import type { DashboardSession } from 'gas-city-dashboard-shared';
import { LiveSessionPeek, isSessionStreamable } from '../LiveSessionPeek';
import { Modal } from '../Modal';

// Pop-out of a bead's live agent run (gascity-dashboard-6frc). Pure
// composition over the same Modal + LiveSessionPeek the Agents-tab peek
// uses, so the bead board and the agents list share one live-stream body.
// The bead → session resolution happens in the caller (BeadDetailModal);
// this component only renders the resolved session's stream.

interface BeadLiveRunModalProps {
  open: boolean;
  onClose: () => void;
  /** The session resolved from the bead's assignee, or null. */
  session: DashboardSession | null;
  /** Bead title, for the modal heading. */
  beadTitle: string;
}

export function BeadLiveRunModal({ open, onClose, session, beadTitle }: BeadLiveRunModalProps) {
  const streamable = isSessionStreamable(session);
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={beadTitle}
      caption={
        session === null
          ? 'No live session resolved for this bead.'
          : streamable
            ? "Live transcript from the supervisor's session stream."
            : "Snapshot from the supervisor's transcript API."
      }
      widthClass="max-w-5xl"
    >
      <LiveSessionPeek sessionId={session?.id ?? null} stream={streamable} showBadge showCaption />
    </Modal>
  );
}
