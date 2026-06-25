import type { RunLane } from 'gas-city-dashboard-shared';
import { Link } from 'react-router-dom';
import type { BadgeSeverity } from '../../attention/compose';
import { attentionListItemProps } from '../../attention/routeHighlight';
import { formatRelative } from '../../hooks/time';
import { runDetailHref } from '../../supervisor/runHref';
import { StageLadder } from './StageLadder';

// Per-lane typographic row. No card chrome — vertical rhythm carries the
// hierarchy, hairline dividers separate lanes at the RunMap level.
// The phase label up top, the title underneath, then a glyph row for
// stage progress (StageLadder, shared with the run-detail view), then
// secondary metadata. Reads in greyscale: phase identity comes from
// order + label + stage glyphs, not color.

interface LaneCardProps {
  lane: RunLane;
  now: number;
  attentionSeverity?: BadgeSeverity | null;
  /**
   * gascity-dashboard-2j8e.2: why-blocked + how-to-unblock, shown only in the
   * Blocked section so a blocked run reads as actionable (which step, what to
   * do) rather than just "blocked". Derived by selectBlockedRuns.
   */
  blocked?: { reason: string; remedy: string };
}

/**
 * gascity-dashboard-f4ps: historical (closed) lanes carry
 * `health: { status: 'unavailable' }` because run health is derived only for
 * the active subset. The health concepts (thrashing, stalled-session) are
 * meaningless for completed runs.
 *
 * Single source for the historical predicate so any future UI that wants to
 * surface health-derived signal can gate on it without re-stating the rule.
 */
export function isHistoricalLane(lane: RunLane): boolean {
  return lane.phase === 'complete';
}

function phaseLabelTone(phase: RunLane['phase']): 'text-accent' | 'text-fg-muted' | 'text-fg' {
  if (phase === 'blocked') return 'text-accent';
  if (phase === 'complete') return 'text-fg-muted';
  return 'text-fg';
}

export function LaneCard({ lane, now, attentionSeverity = null, blocked }: LaneCardProps) {
  const statusEntries = Object.entries(lane.statusCounts).sort((a, b) =>
    statusSortKey(a[0]).localeCompare(statusSortKey(b[0])),
  );
  const { className: attentionClassName = '', ...attentionProps } =
    attentionListItemProps(attentionSeverity);

  return (
    <li
      {...attentionProps}
      className={`py-4 transition-colors duration-150 ease-out-quart ${attentionClassName}`}
    >
      <div className="flex items-baseline justify-between gap-4">
        <span className={`text-label uppercase tracking-wider ${phaseLabelTone(lane.phase)}`}>
          {lane.phaseLabel}
        </span>
        <span
          className="text-label uppercase tracking-wider text-fg-faint tnum tabular-nums"
          title={lane.updatedAt.status === 'available' ? lane.updatedAt.at : lane.updatedAt.error}
        >
          {lane.updatedAt.status === 'available' ? formatRelative(lane.updatedAt.at, now) : '·'}
        </span>
      </div>

      <Link
        to={runDetailHref(lane.id, lane.scope)}
        className="focus-mark mt-1 block text-body text-fg leading-snug hover:text-accent"
      >
        {lane.title}
      </Link>

      {(lane.external.status !== 'unavailable' || lane.formula.status === 'known') && (
        <div className="mt-1 flex items-baseline gap-x-4 gap-y-1 flex-wrap text-label">
          {lane.external.status !== 'unavailable' &&
            (lane.external.status === 'available' ? (
              <a
                href={lane.external.url}
                target="_blank"
                rel="noreferrer"
                className="text-fg-muted uppercase tracking-wider hover:text-fg focus-mark"
              >
                {lane.external.label}
              </a>
            ) : (
              <span className="text-fg-muted uppercase tracking-wider">{lane.external.label}</span>
            ))}
          {lane.formula.status === 'known' && (
            <span className="text-fg-faint tnum">{lane.formula.name}</span>
          )}
        </div>
      )}

      <StageLadder stages={lane.stages} label={lane.title} />

      <div className="mt-2 flex items-baseline gap-x-4 gap-y-1 flex-wrap text-label">
        <span className="text-fg-faint tnum" title="run root bead">
          {lane.id}
        </span>
        {lane.activeAssignees.length > 0 && (
          <span className="text-fg-muted lowercase tracking-normal">
            <span className="uppercase tracking-wider text-fg-faint">on </span>
            {lane.activeAssignees.join(', ')}
          </span>
        )}
        {statusEntries.length > 0 && (
          <span className="text-fg-faint uppercase tracking-wider tnum tabular-nums">
            {statusEntries
              .map(([status, count]) => `${count} ${status.replace(/_/g, ' ')}`)
              .join(' · ')}
          </span>
        )}
      </div>

      {blocked !== undefined && (
        <div className="mt-2">
          <p className="text-body text-fg leading-snug">
            <span aria-hidden="true" className="text-accent">
              ✕
            </span>{' '}
            {blocked.reason}
          </p>
          <p className="mt-1 text-body text-fg-muted leading-snug">{blocked.remedy}</p>
        </div>
      )}
    </li>
  );
}

function statusSortKey(status: string): string {
  const order: Record<string, string> = {
    blocked: '0',
    in_progress: '1',
    open: '2',
    closed: '3',
  };
  return `${order[status] ?? '9'}-${status}`;
}
