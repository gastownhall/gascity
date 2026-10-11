// Run with: npx tsx --test shared/src/structured-transcript.test.ts
//
// Slice 2 of the structured-transcript port (PR #3718 → new dashboard): the
// hand-authored wire types, the four accepted-frame shape guards, and the two
// pure render helpers (patchTextFromHunks, formatUsage). The exact-string
// assertions reproduce the old dashboard's test-asserted output verbatim so the
// Slice 3 renderers and Slice 4 stream can match it at parity.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  patchTextFromHunks,
  formatUsage,
  isSessionStructuredEvent,
  isSessionActivityEvent,
  isSessionHeartbeatEvent,
  isSessionStructuredHistory,
  isStructuredMessage,
  STRUCTURED_SCHEMA_VERSION,
  type SessionStructuredPatchHunk,
  type SessionStructuredUsage,
  type SessionStreamStructuredMessageEvent,
} from './structured-transcript.js';
import { patchTextFromHunks as barrelPatch } from './index.js';

test('STRUCTURED_SCHEMA_VERSION pins the wire schema constant', () => {
  assert.equal(STRUCTURED_SCHEMA_VERSION, 'session.structured.v1');
});

test('structured wire DTOs come only from the generated supervisor client', () => {
  const source = readFileSync(new URL('./structured-transcript.ts', import.meta.url), 'utf8');
  assert.match(source, /from '.\/generated\/gc-supervisor-client\/types\.gen\.js'/);
  assert.doesNotMatch(source, /export\s+interface\s+SessionStructured/);
  assert.doesNotMatch(source, /interface\s+SessionStreamStructuredMessageEventBase/);
});

test('patchTextFromHunks renders file separator + hunk header + lines', () => {
  const hunks: SessionStructuredPatchHunk[] = [
    {
      file_path: 'src/app.ts',
      old_start: 1,
      old_lines: 1,
      new_start: 1,
      new_lines: 1,
      lines: ['- old line', '+ new line'],
    },
  ];
  assert.equal(
    patchTextFromHunks(hunks),
    '*** Update File: src/app.ts\n@@ -1 +1 @@\n- old line\n+ new line',
  );
});

test('patchTextFromHunks emits multi-line ranges as start,count and single as start', () => {
  // old_lines=3 → "1,3"; new_lines=2 → "1,2"; no file_path → no separator.
  assert.equal(
    patchTextFromHunks([
      { old_start: 1, old_lines: 3, new_start: 1, new_lines: 2, lines: ['ctx'] },
    ]),
    '@@ -1,3 +1,2 @@\nctx',
  );
});

test('patchTextFromHunks emits bare @@ when both starts are absent', () => {
  assert.equal(patchTextFromHunks([{ lines: ['x'] }]), '@@\nx');
});

test('patchTextFromHunks emits the file separator once per distinct file_path', () => {
  const same = patchTextFromHunks([
    { file_path: 'a.ts', old_start: 1, new_start: 1, lines: ['one'] },
    { file_path: 'a.ts', old_start: 2, new_start: 2, lines: ['two'] },
  ]);
  assert.equal(same, '*** Update File: a.ts\n@@ -1 +1 @@\none\n@@ -2 +2 @@\ntwo');

  const cross = patchTextFromHunks([
    { file_path: 'a.ts', old_start: 1, new_start: 1, lines: ['one'] },
    { file_path: 'b.ts', old_start: 1, new_start: 1, lines: ['two'] },
  ]);
  assert.equal(
    cross,
    '*** Update File: a.ts\n@@ -1 +1 @@\none\n*** Update File: b.ts\n@@ -1 +1 @@\ntwo',
  );
});

test('patchTextFromHunks returns empty string for empty or absent input', () => {
  assert.equal(patchTextFromHunks([]), '');
  assert.equal(patchTextFromHunks(undefined), '');
});

test('formatUsage renders the full token line in canonical order', () => {
  const usage: SessionStructuredUsage = {
    input_tokens: 100,
    output_tokens: 20,
    reasoning_tokens: 7,
    cache_read_tokens: 5,
    cache_creation_tokens: 3,
    context_used_tokens: 108,
    context_window_tokens: 200000,
    context_percent: 1,
  };
  assert.equal(formatUsage(usage), 'tokens in 100 out 20 reason 7 cache 5 write 3 108/200000 1%');
});

test('formatUsage skips zero token counts but keeps a defined zero percent', () => {
  assert.equal(formatUsage({ input_tokens: 0, output_tokens: 20 }), 'tokens out 20');
  assert.equal(formatUsage({ context_percent: 0 }), 'tokens 0%');
});

test('formatUsage requires both context_used and context_window for the pair', () => {
  assert.equal(
    formatUsage({ context_used_tokens: 108, context_window_tokens: 200000 }),
    'tokens 108/200000',
  );
  assert.equal(formatUsage({ context_used_tokens: 108 }), '');
});

test('formatUsage returns empty string when nothing renders', () => {
  assert.equal(formatUsage({}), '');
  assert.equal(formatUsage(undefined), '');
});

test('isSessionStructuredEvent accepts a structured envelope and rejects others', () => {
  const event: SessionStreamStructuredMessageEvent = {
    id: 'e1',
    template: 'tmpl',
    provider: 'claude',
    format: 'structured',
    schema_version: STRUCTURED_SCHEMA_VERSION,
    operation: 'snapshot',
    history: {
      transcript_stream_id: 'stream-1',
      generation: { id: 'generation-1' },
      cursor: { resume_token: 'st1.snapshot' },
      continuity: { status: 'continuous' },
      tail_state: { activity: 'idle' },
    },
    structured_messages: [],
  };
  assert.equal(isSessionStructuredEvent(event), true);
  assert.equal(isSessionStructuredEvent({ ...event, operation: 'upsert' }), true);
  assert.equal(
    isSessionStructuredEvent({
      ...event,
      operation: 'reset',
      reset_reason: 'stream_changed',
    }),
    true,
  );
  assert.equal(isSessionStructuredEvent({ format: 'raw', messages: [] }), false);
  assert.equal(isSessionStructuredEvent({ format: 'structured', structured_messages: 'x' }), false);
  assert.equal(isSessionStructuredEvent({ ...event, operation: undefined }), false);
  assert.equal(isSessionStructuredEvent({ ...event, operation: 'append' }), false);
  assert.equal(isSessionStructuredEvent({ ...event, schema_version: 'session.structured.v2' }), false);
  assert.equal(isSessionStructuredEvent({ ...event, id: undefined }), false);
  assert.equal(isSessionStructuredEvent({ ...event, template: undefined }), false);
  assert.equal(isSessionStructuredEvent({ ...event, provider: undefined }), false);
  assert.equal(
    isSessionStructuredEvent({ ...event, structured_messages: [{ blocks: [] }] }),
    false,
  );
  assert.equal(
    isSessionStructuredEvent({
      ...event,
      structured_messages: [
        {
          id: 'tool-1',
          role: 'assistant',
          status: 'final',
          blocks: [
            {
              type: 'tool_use',
              input: { kind: 'plan', steps: 'not-an-array' },
            },
          ],
        },
      ],
    }),
    false,
  );
  assert.equal(
    isSessionStructuredEvent({ ...event, operation: 'reset', reset_reason: undefined }),
    false,
  );
  assert.equal(
    isSessionStructuredEvent({ ...event, operation: 'reset', reset_reason: 'unknown' }),
    false,
  );
  assert.equal(isSessionStructuredEvent({ ...event, history: undefined }), false);
  assert.equal(
    isSessionStructuredEvent({
      ...event,
      history: { ...event.history, cursor: {} },
    }),
    false,
  );
  assert.equal(isSessionStructuredEvent('nope'), false);
  assert.equal(isSessionStructuredEvent(null), false);
});

test('isSessionActivityEvent / isSessionHeartbeatEvent are shape guards', () => {
  assert.equal(isSessionActivityEvent({ activity: 'idle' }), true);
  assert.equal(isSessionActivityEvent({ activity: 5 }), false);
  assert.equal(isSessionActivityEvent({}), false);
  assert.equal(isSessionHeartbeatEvent({ timestamp: '2026-06-30T00:00:00Z' }), true);
  assert.equal(isSessionHeartbeatEvent({}), false);
});

test('isSessionStructuredHistory requires the load-bearing nested fields', () => {
  const ok = {
    transcript_stream_id: 's',
    generation: { id: 'g' },
    cursor: { resume_token: 'st1.history' },
    continuity: { status: 'continuous' },
    tail_state: { activity: 'idle' },
  };
  assert.equal(isSessionStructuredHistory(ok), true);
  assert.equal(isSessionStructuredHistory({ ...ok, transcript_stream_id: 1 }), false);
  assert.equal(isSessionStructuredHistory({ ...ok, generation: {} }), false);
  assert.equal(isSessionStructuredHistory({ ...ok, continuity: {} }), false);
  assert.equal(isSessionStructuredHistory({ ...ok, tail_state: {} }), false);
  assert.equal(isSessionStructuredHistory({ ...ok, cursor: {} }), false);
  assert.equal(isSessionStructuredHistory({ ...ok, cursor: { resume_token: 1 } }), false);
  assert.equal(isSessionStructuredHistory(null), false);
  // Intentional hardening over the old guard: an array is not a record, so a
  // sub-field supplied as an array is rejected (the server never sends one).
  assert.equal(isSessionStructuredHistory({ ...ok, cursor: [] }), false);
});

test('isStructuredMessage requires identity, a closed role, status, and typed blocks', () => {
  const message = { id: 'm1', role: 'assistant', status: 'final', blocks: [] };
  assert.equal(isStructuredMessage(message), true);
  assert.equal(isStructuredMessage({ ...message, id: undefined }), false);
  assert.equal(isStructuredMessage({ ...message, role: 'provider-special' }), false);
  assert.equal(isStructuredMessage({ ...message, status: undefined }), false);
  assert.equal(isStructuredMessage({ ...message, blocks: 'x' }), false);
  assert.equal(isStructuredMessage({ ...message, blocks: [{ type: 'provider-special' }] }), false);
  assert.equal(isStructuredMessage({}), false);
});

test('barrel re-exports the structured-transcript module', () => {
  assert.equal(barrelPatch, patchTextFromHunks);
});
