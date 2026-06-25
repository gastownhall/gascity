import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { runSessionLinkFor } from './session-link.js';

// Regression coverage for the rig-store / polecat run "invalid session id"
// bug. A polecat run records its session as the pool-qualified NAME
// (`polecat-gc-333573`), whose real supervisor session id is the gc-suffix
// (`gc-333573`). When the session has completed it is absent from the live
// session index, so it can't be resolved by name — the link must normalize
// the recorded value to the supervisor id itself, or the Session tab feeds a
// name where an id is expected and the route rejects it as "invalid session id".
describe('runSessionLinkFor — session id normalization', () => {
  test('normalizes a pool-qualified session name in metadata to the supervisor id', () => {
    const bead = {
      assignee: 'polecat-gc-333573',
      metadata: { session_id: 'polecat-gc-333573' },
    } as never;
    const link = runSessionLinkFor(bead, 'done');
    assert.equal(link?.sessionId, 'gc-333573');
  });

  test('leaves a clean gc-prefixed session id unchanged', () => {
    const bead = { metadata: { session_id: 'gc-333573' } } as never;
    const link = runSessionLinkFor(bead, 'done');
    assert.equal(link?.sessionId, 'gc-333573');
  });

  test('derives the id from a pool-qualified assignee when no metadata id is present', () => {
    const bead = { assignee: 'polecat-gc-333573' } as never;
    const link = runSessionLinkFor(bead, 'done');
    assert.equal(link?.sessionId, 'gc-333573');
  });

  test('degrades to no link when an unresolvable value carries no supervisor id', () => {
    // A completed run whose recorded handle carries no extractable supervisor
    // id and is absent from the live session index must NOT leak that handle
    // into link.sessionId — the session route would reject it as "invalid
    // session id". The link degrades so the Session tab shows a clean
    // "session not available" empty state instead.
    const bead = { metadata: { session_id: 'mystery-handle' } } as never;
    assert.equal(runSessionLinkFor(bead, 'done'), undefined);
  });

  test('degrades a runtime-derived bare assignee that cannot yield a supervisor id', () => {
    // The runtime-derived path: a completed pool/rig-store run records only an
    // assignee (no session_id metadata), and that assignee is a bare worker
    // name with no embedded supervisor id. With no live index match there is
    // no usable id, so the link degrades rather than feeding "polecat" to the
    // route as an "invalid session id".
    const bead = { assignee: 'polecat' } as never;
    assert.equal(runSessionLinkFor(bead, 'done'), undefined);
  });

  test('returns undefined for pending/ready nodes (no session yet)', () => {
    const bead = { assignee: 'polecat-gc-333573' } as never;
    assert.equal(runSessionLinkFor(bead, 'pending'), undefined);
    assert.equal(runSessionLinkFor(bead, 'ready'), undefined);
  });
});
