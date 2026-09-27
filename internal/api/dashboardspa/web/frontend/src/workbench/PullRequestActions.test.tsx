import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PullRequestActions } from './PullRequestActions';
import type { PullRequestContext } from './pullRequestPolicy';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import type { SupervisorBead } from '../supervisor/beadReads';

const attempt: ExecutionAttempt = {
  sessionId: 's-1',
  sessionName: 'worker-1',
  workDir: '/wt/s-1',
  state: 'active',
  active: true,
  startedAt: '2026-01-01T00:00:00Z',
  lastActive: '2026-01-01T00:00:00Z',
};

function ctx(over: Partial<PullRequestContext> = {}): PullRequestContext {
  return {
    bead: { id: 'gascity-1' } as SupervisorBead,
    attempt,
    attemptRevision: 'abc123',
    policyRevision: 'abc123',
    queueAvailable: true,
    policyRejected: false,
    hasConflict: false,
    ...over,
  };
}

afterEach(cleanup);

describe('PullRequestActions', () => {
  it('renders prepare and queue actions, and no merge control', () => {
    render(<PullRequestActions context={ctx()} onAction={vi.fn()} />);
    expect(screen.getByRole('button', { name: /prepare pr/i })).toBeTruthy();
    expect(screen.getByRole('button', { name: /queue pr/i })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /merge/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /force/i })).toBeNull();
  });

  it('shows the blocked reason and no actions', () => {
    render(<PullRequestActions context={ctx({ policyRejected: true })} onAction={vi.fn()} />);
    expect(screen.getByRole('status').textContent).toMatch(/policy rejected/i);
    expect(screen.queryByRole('button', { name: /prepare pr/i })).toBeNull();
  });

  it('submits once (idempotent while in flight)', async () => {
    const onAction = vi.fn(() => new Promise<void>(() => {}));
    render(<PullRequestActions context={ctx()} onAction={onAction} />);
    const queue = screen.getByRole('button', { name: /queue pr/i });
    fireEvent.click(queue);
    fireEvent.click(queue);
    await waitFor(() => expect(onAction).toHaveBeenCalledTimes(1));
    expect(onAction).toHaveBeenCalledWith('queue');
  });
});
