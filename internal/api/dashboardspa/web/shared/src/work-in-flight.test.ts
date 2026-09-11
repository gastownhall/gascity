// Run with: npx tsx --test shared/src/work-in-flight.test.ts
//
// Work-in-flight assignee parsing: split a bead assignee into its worker role
// and the live session id it embeds. Fixtures use the real verified examples
// (see work-in-flight.ts header).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseAssignee } from './work-in-flight.js';

test('parseAssignee splits the real verified examples into role + session id', () => {
  assert.deepEqual(parseAssignee('polecat-gc-335825'), {
    role: 'polecat',
    sessionId: 'gc-335825',
  });
  assert.deepEqual(parseAssignee('scix-worker-gc-335812'), {
    role: 'scix-worker',
    sessionId: 'gc-335812',
  });
  assert.deepEqual(parseAssignee('enterprisebench-worker-gc-335808'), {
    role: 'enterprisebench-worker',
    sessionId: 'gc-335808',
  });
});

test('parseAssignee extracts td-/th-/4-letter-prefixed handles too', () => {
  assert.deepEqual(parseAssignee('polecat-td-9abc'), {
    role: 'polecat',
    sessionId: 'td-9abc',
  });
  assert.deepEqual(parseAssignee('worker-fddc-12xy'), {
    role: 'worker',
    sessionId: 'fddc-12xy',
  });
});

test('parseAssignee backfills role with the session id when the assignee is only a handle', () => {
  assert.deepEqual(parseAssignee('gc-335825'), {
    role: 'gc-335825',
    sessionId: 'gc-335825',
  });
});

test('parseAssignee falls back to the whole string when no session handle is present', () => {
  assert.deepEqual(parseAssignee('mayor'), { role: 'mayor' });
  assert.deepEqual(parseAssignee('/home/ds/gas-city/city-infra-polecat'), {
    role: '/home/ds/gas-city/city-infra-polecat',
  });
});

test('parseAssignee does NOT misparse a plain role as a bare session id', () => {
  // `scix-worker` is a worker ROLE, not a session id: its body has no digit, so
  // the bare-handle branch must reject it and leave sessionId undefined. (A live
  // session id always carries a numeric handle, e.g. `gc-335812`.)
  assert.deepEqual(parseAssignee('scix-worker'), { role: 'scix-worker' });
});

test('parseAssignee trims surrounding whitespace', () => {
  assert.deepEqual(parseAssignee('  polecat-gc-335825  '), {
    role: 'polecat',
    sessionId: 'gc-335825',
  });
});
