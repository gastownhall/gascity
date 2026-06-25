import type { DashboardSession } from 'gas-city-dashboard-shared';
import { isSessionStreamable, LiveSessionPeek } from '../LiveSessionPeek';

export function AgentLivePeek({ session }: { session: DashboardSession }) {
  return (
    <section>
      <header className="flex items-baseline justify-between mb-4">
        <h2 className="text-label uppercase tracking-wider text-fg-faint">Live peek</h2>
      </header>
      <LiveSessionPeek
        sessionId={session.id}
        stream={isSessionStreamable(session)}
        showBadge
        showCaption
      />
    </section>
  );
}
