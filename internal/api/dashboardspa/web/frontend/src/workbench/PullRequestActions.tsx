import { useState } from 'react';
import { Button } from '../components/Button';
import {
  blockedReasonLabel,
  pullRequestPolicy,
  type PullRequestAction,
  type PullRequestContext,
} from './pullRequestPolicy';

// Policy-bound pull-request actions for the selected Execution Attempt.
//
// The offered actions are exactly what Gas City policy allows; the Workbench
// renders NO merge, approve, or force-push control. Every action is delegated
// through onAction (which calls the typed Gas City API), so the request and its
// result are auditable against the Bead and attempt.
export function PullRequestActions({
  context,
  onAction,
}: {
  context: PullRequestContext;
  onAction: (action: PullRequestAction) => Promise<void>;
}) {
  const policy = pullRequestPolicy(context);
  const [busy, setBusy] = useState<PullRequestAction | null>(null);
  const [error, setError] = useState<string | null>(null);

  if (policy.blocked) {
    return (
      <p role="status" className="text-body text-fg-muted italic">
        {blockedReasonLabel(policy.blocked)}
      </p>
    );
  }

  const run = async (action: PullRequestAction) => {
    if (busy !== null) return; // idempotent: no duplicate submission in flight
    setBusy(action);
    setError(null);
    try {
      await onAction(action);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'action failed');
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="flex items-center gap-2">
      {policy.allowed.includes('prepare') && (
        <Button size="sm" onClick={() => void run('prepare')} disabled={busy !== null}>
          Prepare PR
        </Button>
      )}
      {policy.allowed.includes('queue') && (
        <Button size="sm" onClick={() => void run('queue')} disabled={busy !== null}>
          Queue PR
        </Button>
      )}
      {error && (
        <span role="alert" className="text-body text-accent">
          {error}
        </span>
      )}
    </div>
  );
}
