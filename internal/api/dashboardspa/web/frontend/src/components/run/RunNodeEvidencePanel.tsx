import type { RunDisplayNode } from 'gas-city-dashboard-shared';
import type { RunDiffLoadState } from '../../hooks/useRunDiff';
import { RunDiffPanel } from './RunDiffPanel';
import { RunNodeSessionPanel } from './RunNodeSessionPanel';

interface RunNodeEvidencePanelProps {
  tab: 'diff' | 'session';
  diff: RunDiffLoadState;
  selectedNode: RunDisplayNode | null;
}

export function RunNodeEvidencePanel({ tab, diff, selectedNode }: RunNodeEvidencePanelProps) {
  if (tab === 'session') {
    return <RunNodeSessionPanel node={selectedNode} visible />;
  }
  return <RunDiffResourcePanel diff={diff} />;
}

function RunDiffResourcePanel({ diff }: { diff: RunDiffLoadState }) {
  switch (diff.kind) {
    case 'idle':
      return (
        <p className="text-body text-fg-muted italic">Local changes are not loaded for this run.</p>
      );
    case 'loading':
      return <p className="text-body text-fg-muted italic">Loading local changes.</p>;
    case 'failed':
      return (
        <p className="text-body text-accent" role="alert">
          {diff.error}
        </p>
      );
    case 'ready':
      return (
        <>
          {diff.refreshState.kind === 'failed' && (
            <p className="mb-4 text-body text-accent" role="alert">
              {diff.refreshState.error}
            </p>
          )}
          {diff.refreshState.kind === 'refreshing' && (
            <p className="mb-4 text-label uppercase tracking-wider text-fg-faint" role="status">
              Refreshing local changes
            </p>
          )}
          <RunDiffPanel diff={diff.diff} />
        </>
      );
  }
}
