import { describe, expect, it } from 'vitest';
import { runEventIdentity, formulaRunDetailEventMatches } from './runEventIdentity';

describe('run event identity helpers', () => {
  it('matches top-level run identity for the current run', () => {
    const identity = runEventIdentity({
      type: 'bead.updated',
      run_id: 'wf-1',
      root_bead_id: 'root-1',
    });

    expect(identity.runIds).toEqual(new Set(['wf-1']));
    expect(identity.rootBeadIds).toEqual(new Set(['root-1']));
    expect(
      formulaRunDetailEventMatches(identity, {
        runId: 'wf-1',
        rootBeadId: 'root-1',
      }),
    ).toBe(true);
  });

  it('finds nested bead metadata identity', () => {
    const identity = runEventIdentity({
      type: 'bead.updated',
      payload: {
        bead: {
          metadata: {
            'gc.run_id': 'wf-2',
            'gc.root_bead_id': 'root-2',
          },
        },
      },
    });

    expect(
      formulaRunDetailEventMatches(identity, {
        runId: 'wf-2',
        rootBeadId: 'root-2',
      }),
    ).toBe(true);
  });

  it('normalizes supervisor workflow identity to run identity', () => {
    const identity = runEventIdentity({
      type: 'bead.updated',
      workflow_id: 'wf-legacy-edge',
      root_bead_id: 'root-edge',
      payload: {
        bead: {
          metadata: {
            'gc.workflow_id': 'wf-legacy-edge',
            'gc.root_bead_id': 'root-edge',
          },
        },
      },
    });

    expect(identity.runIds).toEqual(new Set(['wf-legacy-edge']));
    expect(
      formulaRunDetailEventMatches(identity, {
        runId: 'wf-legacy-edge',
        rootBeadId: 'root-edge',
      }),
    ).toBe(true);
  });

  it('does not match events identified as another formula run', () => {
    const identity = runEventIdentity({
      type: 'bead.updated',
      run: {
        run_id: 'other-wf',
        root_bead_id: 'other-root',
      },
    });

    expect(
      formulaRunDetailEventMatches(identity, {
        runId: 'wf-3',
        rootBeadId: 'root-3',
      }),
    ).toBe(false);
  });

  it('treats events without run identity as broad invalidation signals', () => {
    const identity = runEventIdentity({ type: 'session.updated' });

    expect(
      formulaRunDetailEventMatches(identity, {
        runId: 'wf-4',
        rootBeadId: 'root-4',
      }),
    ).toBe(true);
  });
});
