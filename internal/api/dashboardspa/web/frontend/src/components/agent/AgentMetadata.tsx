import type { ReactNode } from 'react';
import type { DashboardSession } from 'gas-city-dashboard-shared';
import { effectiveContextPct } from 'gas-city-dashboard-shared';
import { formatRelative } from '../../hooks/time';

export function AgentMetadata({ session, now }: { session: DashboardSession; now: number }) {
  const pct = effectiveContextPct(session);
  const items: ReadonlyArray<{ label: string; value: ReactNode }> = [
    { label: 'Rig', value: session.rig ?? '·' },
    { label: 'Pool', value: session.pool ?? '·' },
    { label: 'Provider', value: session.provider ?? '·' },
    { label: 'Model', value: session.model ?? '·' },
    {
      label: 'Context',
      value:
        typeof pct === 'number' ? (
          <span
            className={`tnum ${pct >= 95 ? 'text-accent' : pct >= 80 ? 'text-warn' : 'text-fg'}`}
          >
            {pct}%
          </span>
        ) : (
          '·'
        ),
    },
    { label: 'Attached', value: session.attached ? 'yes' : 'no' },
    {
      label: 'Created',
      value: <span className="tnum">{formatRelative(session.created_at, now)}</span>,
    },
    {
      label: 'Last active',
      value: <span className="tnum">{formatRelative(session.last_active, now)}</span>,
    },
  ];

  return (
    <dl className="grid grid-cols-2 sm:grid-cols-4 gap-x-8 gap-y-5 mb-12">
      {items.map((it) => (
        <div key={it.label}>
          <dt className="text-label uppercase tracking-wider text-fg-faint mb-1">{it.label}</dt>
          <dd className="text-body text-fg">{it.value}</dd>
        </div>
      ))}
    </dl>
  );
}
