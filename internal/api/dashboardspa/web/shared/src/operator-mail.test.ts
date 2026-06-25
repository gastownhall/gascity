// Run with: npx tsx --test shared/src/operator-mail.test.ts
//
// gascity-dashboard-2j8e.5: the operator-mail needs-you selector. The Mail nav
// badge and the Mail page both read selectOperatorActionableUnread, so the
// badge count and the page count cannot disagree. These tests pin the two
// behaviours the bead asks for: the pool-worker firehose is folded away, and
// unread escalations from the mayor / role agents are kept.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  isPoolWorkerSender,
  selectOperatorActionableUnread,
  type OperatorMailItem,
} from './operator-mail.js';

function mail(overrides: Partial<OperatorMailItem>): OperatorMailItem {
  return {
    from: 'mayor',
    read: false,
    created_at: '2026-06-07T12:00:00.000Z',
    ...overrides,
  };
}

// The supervisor reports a mail sender inconsistently — a bare name, a session
// id, or a filesystem path — so the polecat token can sit anywhere in the
// basename. All of these are the worker firehose.
test('isPoolWorkerSender classifies polecat senders in every wire form', () => {
  assert.equal(isPoolWorkerSender('/home/ds/gascity/polecat-2'), true);
  assert.equal(isPoolWorkerSender('polecat-gc-243376'), true);
  assert.equal(isPoolWorkerSender('/home/ds/gascity-packs/gascity-packs-polecat-1'), true);
  assert.equal(isPoolWorkerSender('POLECAT-7'), true);
});

test('isPoolWorkerSender does not flag the mayor, role agents, or the operator', () => {
  assert.equal(isPoolWorkerSender('mayor'), false);
  assert.equal(isPoolWorkerSender('zeldascension/oversight-rig.project-lead'), false);
  assert.equal(isPoolWorkerSender('human'), false);
  assert.equal(isPoolWorkerSender('stephanie'), false);
  assert.equal(isPoolWorkerSender(''), false);
});

test('selectOperatorActionableUnread keeps mayor/role escalations, drops the pool firehose', () => {
  const items = [
    mail({ from: 'mayor', read: false }),
    mail({ from: '/home/ds/gascity/polecat-2', read: false }),
    mail({ from: '/home/ds/gascity/polecat-1', read: false }),
    mail({ from: 'zeldascension/oversight-rig.project-lead', read: false }),
  ];
  assert.deepEqual(
    selectOperatorActionableUnread(items).map((m) => m.from),
    ['mayor', 'zeldascension/oversight-rig.project-lead'],
  );
});

test('selectOperatorActionableUnread drops already-read mail', () => {
  const items = [mail({ from: 'mayor', read: true }), mail({ from: 'mayor', read: false })];
  assert.equal(selectOperatorActionableUnread(items).length, 1);
});

test('selectOperatorActionableUnread returns nothing for a pure pool firehose (the ~93 inflation)', () => {
  const firehose = Array.from({ length: 93 }, (_, i) =>
    mail({ from: `/home/ds/gascity/polecat-${i}`, read: false }),
  );
  assert.equal(selectOperatorActionableUnread(firehose).length, 0);
});

test('selectOperatorActionableUnread preserves input order (caller owns sort)', () => {
  const items = [
    mail({ from: 'mayor', created_at: '2026-06-07T09:00:00.000Z' }),
    mail({ from: '/home/ds/gascity/polecat-2' }),
    mail({ from: 'clerk', created_at: '2026-06-07T11:00:00.000Z' }),
  ];
  assert.deepEqual(
    selectOperatorActionableUnread(items).map((m) => m.from),
    ['mayor', 'clerk'],
  );
});
