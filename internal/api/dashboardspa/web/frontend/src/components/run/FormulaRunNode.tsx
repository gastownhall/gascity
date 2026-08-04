import type { RunConstructKind, RunDisplayNode, RunNodeStatus } from 'gas-city-dashboard-shared';

interface FormulaRunNodeProps {
  node: RunDisplayNode;
  selected: boolean;
  onToggle: (nodeId: string) => void;
}

const STATUS_LABEL: Record<RunNodeStatus, string> = {
  pending: 'pending',
  ready: 'ready',
  running: 'running',
  active: 'running',
  done: 'done',
  completed: 'done',
  failed: 'failed',
  blocked: 'blocked',
  skipped: 'skipped',
  canceled: 'canceled',
};

export function FormulaRunNode({ node, selected, onToggle }: FormulaRunNodeProps) {
  const shapeClass = shapeClassFor(node.constructKind);
  const statusClass = statusClassFor(node.status);
  const history =
    node.iterationSummary.kind === 'stacked'
      ? `${node.iterationSummary.iterationCount} iterations, showing ${node.iterationSummary.visibleIteration}`
      : null;
  const attemptLabel =
    node.attemptSummary.kind === 'tracked' && node.attemptSummary.badge.kind === 'bounded'
      ? ` · attempt ${node.attemptSummary.badge.label}${activeAttemptLabel(node)}`
      : '';

  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={() => onToggle(node.id)}
      className={`focus-mark w-full text-left px-4 py-3 bg-transparent transition-colors duration-150 ease-out-quart ${shapeClass} ${
        selected
          ? 'text-fg border-accent bg-surface-tint ring-2 ring-accent/45 ring-offset-2 ring-offset-surface'
          : 'text-fg border-rule hover:border-fg-faint hover:bg-surface-tint'
      }`}
    >
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="text-body text-fg leading-snug">{node.title}</p>
          <p className="mt-1 text-label uppercase tracking-wider text-fg-faint">
            {constructLabel(node.constructKind)}
            {attemptLabel}
          </p>
        </div>
        <span className={`text-label uppercase tracking-wider shrink-0 ${statusClass}`}>
          {statusGlyph(node.status)} {STATUS_LABEL[node.status]}
        </span>
      </div>
      {history && (
        <p className="mt-2 text-label uppercase tracking-wider text-fg-faint tnum">
          stacked history: {history}
        </p>
      )}
      {node.controlBadges.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-2">
          {node.controlBadges.map((badge) => (
            <span
              key={badge.id}
              className="text-label uppercase tracking-wider text-fg-muted border border-rule px-1.5 py-0.5"
            >
              {badge.label}: {STATUS_LABEL[badge.status]}
            </span>
          ))}
        </div>
      )}
    </button>
  );
}

function activeAttemptLabel(node: RunDisplayNode): string {
  return node.attemptSummary.kind === 'tracked' && node.attemptSummary.active.kind === 'running'
    ? ` · running attempt ${node.attemptSummary.active.value}`
    : '';
}

function constructLabel(constructKind: RunConstructKind): string {
  switch (constructKind) {
    case 'run-root':
      return 'run root';
    case 'run-finalize':
      return 'finalize';
    case 'step':
    case 'retry':
    case 'check-loop':
    case 'scope':
    case 'condition':
    case 'fanout':
    case 'expansion':
    case 'scope-check':
    case 'spec':
    case 'control':
    case 'unknown':
      return constructKind.replace(/-/g, ' ');
  }
}

function shapeClassFor(constructKind: RunConstructKind): string {
  switch (constructKind) {
    case 'run-root':
      return 'formula-run-node-shape-root';
    case 'step':
    case 'unknown':
      return 'formula-run-node-shape-step';
    case 'retry':
      return 'formula-run-node-shape-retry';
    case 'check-loop':
      return 'formula-run-node-shape-check-loop';
    case 'scope':
      return 'formula-run-node-shape-scope';
    case 'condition':
      return 'formula-run-node-shape-condition';
    case 'fanout':
      return 'formula-run-node-shape-fanout';
    case 'expansion':
      return 'formula-run-node-shape-expansion';
    case 'scope-check':
    case 'run-finalize':
    case 'spec':
    case 'control':
      return 'formula-run-node-shape-control';
  }
}

function statusClassFor(status: RunNodeStatus): string {
  switch (status) {
    case 'failed':
    case 'blocked':
      return 'text-accent';
    case 'active':
    case 'running':
    case 'ready':
      return 'text-fg';
    case 'completed':
    case 'done':
      return 'text-fg-muted';
    case 'pending':
    case 'skipped':
    case 'canceled':
      return 'text-fg-faint';
  }
}

function statusGlyph(status: RunNodeStatus): string {
  switch (status) {
    case 'completed':
    case 'done':
      return '✓';
    case 'active':
    case 'running':
      return '●';
    case 'failed':
    case 'blocked':
      return '!';
    case 'skipped':
      return '∅';
    case 'canceled':
      return '⊘';
    case 'pending':
    case 'ready':
      return '·';
  }
}
