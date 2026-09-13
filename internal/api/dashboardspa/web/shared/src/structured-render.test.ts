// Run with: npx tsx --test shared/src/structured-render.test.ts
//
// Slice 3a of the structured-transcript port (PR #3718 → new dashboard): the
// pure formatting layer. The exact-string assertions reproduce the old
// dashboard's crew.test.ts text-content contract — the `<pre>` body text, the
// header role class, and the diff-line kinds — so the Slice 3b React renderer
// can map these strings to JSX at parity.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  roleClass,
  diffLineKind,
  formatInteraction,
  formatInlineValue,
  formatArgument,
  userPromptRows,
  systemEventRows,
  historyRows,
  toolInputRows,
  toolResultSections,
  imageRows,
  pendingRows,
} from './structured-render.js';
import { roleClass as barrelRoleClass } from './index.js';
import type {
  SessionStructuredBlock,
  SessionStructuredHistory,
  SessionStructuredSystemEvent,
  SessionStructuredToolInput,
  SessionStructuredToolResult,
  SessionStructuredUserPrompt,
} from './structured-transcript.js';
import type { PendingInteraction } from './pending.js';

// Helper: build a tool_result block carrying a typed structured payload.
function resultBlock(structured: SessionStructuredToolResult): SessionStructuredBlock {
  return { type: 'tool_result', structured };
}

// --- roleClass (spec §1) ---------------------------------------------------

test('roleClass maps assistant/agent → assistant, system, result, else user', () => {
  assert.equal(roleClass('assistant'), 'assistant');
  assert.equal(roleClass('agent'), 'assistant');
  assert.equal(roleClass('AGENT'), 'assistant');
  assert.equal(roleClass('system'), 'system');
  assert.equal(roleClass('result'), 'result');
  assert.equal(roleClass('user'), 'user');
  assert.equal(roleClass('tool'), 'user');
  assert.equal(roleClass(''), 'user');
});

// --- diffLineKind (spec __diffRules__) -------------------------------------

test('diffLineKind classifies each prefix with the load-bearing order', () => {
  assert.equal(diffLineKind('@@ -1 +1 @@'), 'hunk');
  assert.equal(diffLineKind('diff --git a/x b/x'), 'file');
  assert.equal(diffLineKind('index abc..def 100644'), 'file');
  assert.equal(diffLineKind('*** Update File: src/app.ts'), 'file');
  // `---`/`+++` file headers must match as file BEFORE the +/- add/del rules.
  assert.equal(diffLineKind('--- a/src/app.ts'), 'file');
  assert.equal(diffLineKind('+++ b/src/app.ts'), 'file');
  assert.equal(diffLineKind('+ new line'), 'add');
  assert.equal(diffLineKind('- old line'), 'del');
  assert.equal(diffLineKind(' context line'), 'context');
  assert.equal(diffLineKind(''), 'context');
});

// --- formatInlineValue / formatArgument (spec §10) -------------------------

test('formatInlineValue renders null/undefined as empty and primitives verbatim', () => {
  assert.equal(formatInlineValue(null), '');
  assert.equal(formatInlineValue(undefined), '');
  assert.equal(formatInlineValue('hello'), 'hello');
  assert.equal(formatInlineValue(42), '42');
  assert.equal(formatInlineValue(0), '0');
  assert.equal(formatInlineValue(true), 'true');
  assert.equal(formatInlineValue({ a: 1 }), '{"a":1}');
});

test('formatArgument renders name: value, defaulting name and inlining non-string value', () => {
  assert.equal(
    formatArgument({ name: 'Select rollout scope', value: 'All providers' }),
    'Select rollout scope: All providers',
  );
  assert.equal(formatArgument({ value: 'x' }), 'argument: x');
  assert.equal(formatArgument({ name: 'count', value: 5 }), 'count: 5');
  assert.equal(formatArgument('plain'), 'plain');
});

// --- formatInteraction (spec §9) -------------------------------------------

test('formatInteraction joins kind/state/request/action/prompt/options filtered', () => {
  const block: SessionStructuredBlock = {
    type: 'interaction',
    interaction: {
      kind: 'approval',
      state: 'awaiting_user',
      request_id: 'approval-1',
      action: 'Approve',
      prompt: 'Allow Edit to modify src/app.ts?',
      options: ['Approve', 'Deny'],
    },
  };
  assert.equal(
    formatInteraction(block),
    'approval awaiting_user approval-1 Approve Allow Edit to modify src/app.ts? Approve, Deny',
  );
});

test('formatInteraction defaults the kind to "interaction" and drops empty parts', () => {
  assert.equal(formatInteraction({ type: 'interaction' }), 'interaction');
  assert.equal(
    formatInteraction({ type: 'interaction', interaction: { state: 'pending' } }),
    'interaction pending',
  );
});

// --- userPromptRows (spec §2) ----------------------------------------------

test('userPromptRows renders prompt, opened files, uploaded files, and selections', () => {
  const prompt: SessionStructuredUserPrompt = {
    text: 'Please inspect this.',
    opened_files: ['/tmp/project/src/app.ts'],
    uploaded_files: [
      {
        original_name: 'diagram.png',
        size: '12 KB',
        mime_type: 'image/png',
        file_path: '/tmp/uploads/diagram.png',
      },
    ],
    selections: [{ text: 'const answer = 42;' }],
  };
  assert.deepEqual(userPromptRows(prompt), [
    'prompt: Please inspect this.',
    'opened files: /tmp/project/src/app.ts',
    'uploaded files:',
    'diagram.png (12 KB, image/png): /tmp/uploads/diagram.png'.replace(/^/, '- '),
    'selections:',
    '- const answer = 42;',
  ]);
});

test('userPromptRows renders an uploaded file with a preview suffix and no path', () => {
  assert.deepEqual(
    userPromptRows({
      uploaded_files: [{ original_name: 'note.txt', preview_url: 'https://ex/p' }],
    }),
    ['uploaded files:', '- note.txt preview: https://ex/p'],
  );
});

test('userPromptRows is empty for an empty prompt', () => {
  assert.deepEqual(userPromptRows({}), []);
});

// --- systemEventRows (spec §3) ---------------------------------------------

test('systemEventRows renders kind/category/code/message in order', () => {
  const event: SessionStructuredSystemEvent = {
    kind: 'error',
    category: 'usage_limit',
    code: 'usage_limit_exceeded',
    message: "You've hit your usage limit.",
  };
  assert.deepEqual(systemEventRows(event), [
    'kind: error',
    'category: usage_limit',
    'code: usage_limit_exceeded',
    "message: You've hit your usage limit.",
  ]);
});

// --- historyRows (spec §4) -------------------------------------------------

test('historyRows renders stream/generation/continuity/tail/diagnostics in order', () => {
  const history: SessionStructuredHistory = {
    transcript_stream_id: 'stream-open-code-1',
    provider_session_id: 'provider-session-99',
    generation: { id: 'generation-1', observed_at: '2026-04-18T20:00:00Z' },
    cursor: { after_entry_id: 'entry-42', resume_token: 'st1.history-rows' },
    continuity: { status: 'compacted', has_branches: true, note: 'compacted transcript' },
    tail_state: {
      activity: 'in-turn',
      last_entry_id: 'entry-42',
      open_tool_call_ids: ['tool-open'],
      pending_interaction_ids: ['approval-1'],
      degraded: true,
      degraded_reason: 'reader recovering',
    },
    diagnostics: [{ code: 'partial_history', count: 2, message: 'older entries compacted' }],
  };
  assert.deepEqual(historyRows(history), [
    'stream: stream-open-code-1',
    'provider session: provider-session-99',
    'generation: generation-1',
    'observed: 2026-04-18T20:00:00Z',
    'cursor: entry-42',
    'continuity: compacted',
    'branches: yes',
    'note: compacted transcript',
    'activity: in-turn',
    'last entry: entry-42',
    'open tools: tool-open',
    'pending: approval-1',
    'degraded: yes',
    'degraded reason: reader recovering',
    'diagnostic: code: partial_history, count: 2, message: older entries compacted',
  ]);
});

test('historyRows includes a zero compaction count (appendNumber keeps zero)', () => {
  const history: SessionStructuredHistory = {
    transcript_stream_id: 's',
    generation: { id: 'g' },
    cursor: { resume_token: 'st1.minimal-history' },
    continuity: { status: 'continuous', compaction_count: 0 },
    tail_state: { activity: 'idle' },
  };
  assert.deepEqual(historyRows(history), [
    'stream: s',
    'generation: g',
    'continuity: continuous',
    'compactions: 0',
    'activity: idle',
  ]);
});

// --- toolInputRows (spec §7) -----------------------------------------------

test('toolInputRows renders fields in the appendField order', () => {
  const input: SessionStructuredToolInput = {
    kind: 'patch',
    file_path: 'src/app.ts',
    language: 'typescript',
    patch: '*** Update File: src/app.ts',
  };
  assert.deepEqual(toolInputRows(input), [
    'kind: patch',
    'file: src/app.ts',
    'language: typescript',
    'patch: *** Update File: src/app.ts',
  ]);
});

test('toolInputRows renders plan steps and stdin linked command', () => {
  assert.deepEqual(
    toolInputRows({
      kind: 'plan',
      plan: 'Expose typed plan data without HTML.',
      explanation: 'Keep clients provider-neutral.',
      steps: [{ step: 'Add plan DTO', status: 'in_progress' }],
    }),
    [
      'kind: plan',
      'plan: Expose typed plan data without HTML.',
      'explanation: Keep clients provider-neutral.',
      'steps:',
      '- [in_progress] Add plan DTO',
    ],
  );
  assert.deepEqual(
    toolInputRows({
      kind: 'stdin',
      task_id: '42',
      text: 'hello\n',
      linked_command: 'claude --resume',
    }),
    ['kind: stdin', 'task: 42', 'linked command: claude --resume', 'text: hello\n'],
  );
});

test('toolInputRows renders typed todos and arguments', () => {
  assert.deepEqual(
    toolInputRows({
      kind: 'todo',
      todos: [
        {
          content: 'Normalize typed todos',
          status: 'in_progress',
          active_form: 'Normalizing typed todos',
          priority: 'high',
        },
      ],
    }),
    [
      'kind: todo',
      'todos:',
      '- [in_progress] Normalize typed todos priority high (Normalizing typed todos)',
    ],
  );
  assert.deepEqual(
    toolInputRows({
      kind: 'arguments',
      arguments: [
        { name: 'a', value: '1' },
        { name: 'b', value: '2' },
      ],
    }),
    ['kind: arguments', 'a: 1', 'b: 2'],
  );
});

test('toolInputRows falls back to inline value when no rows accumulate', () => {
  assert.deepEqual(
    toolInputRows({} as unknown as SessionStructuredToolInput),
    ['{}'],
  );
});

// --- toolResultSections per kind (spec §8 + __perKindRendering__) ----------

test('toolResultSections bash renders command/task/stdout lines/timestamp and tool error', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'bash',
      command: 'npm test',
      task_id: 'shell-123',
      task_status: 'completed',
      stdout: 'tests passed',
      stderr: 'warn',
      exit_code: 0,
      stdout_lines: 1,
      stderr_lines: 1,
      timestamp: '2026-06-01T00:00:02Z',
      error: {
        category: 'command_failure',
        message: 'npm ERR! test failed',
        user_reason: 'stopped by user',
      },
    }),
  );
  assert.equal(sections.kind, 'bash');
  assert.equal(sections.diff, '');
  assert.equal(
    sections.body,
    [
      'kind: bash',
      'error category: command_failure',
      'error: npm ERR! test failed',
      'user reason: stopped by user',
      'command: npm test',
      'task: shell-123',
      'task status: completed',
      'stdout: tests passed',
      'stderr: warn',
      'stdout lines: 1',
      'stderr lines: 1',
      'timestamp: 2026-06-01T00:00:02Z',
      'exit 0',
    ].join('\n'),
  );
});

test('toolResultSections python renders code/stdout/stderr/exit', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'python',
      code: 'print(1)',
      stdout: 'out',
      stderr: 'err',
      exit_code: 0,
      truncated: true,
    }),
  );
  assert.equal(sections.kind, 'python');
  assert.equal(
    sections.body,
    'kind: python\ncode: print(1)\nstdout: out\nstderr: err\nexit 0\ntruncated',
  );
});

test('toolResultSections stdin renders task/content/text', () => {
  const sections = toolResultSections(
    resultBlock({ kind: 'stdin', task_id: '42', content: 'sent' }),
  );
  assert.equal(sections.kind, 'stdin');
  assert.equal(sections.body, 'kind: stdin\ntask: 42\ncontent: sent');
});

test('toolResultSections edit renders fields plus a diff from patch_hunks', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'edit',
      file_path: 'src/app.ts',
      language: 'typescript',
      old_string: 'old line',
      new_string: 'new line',
      original_file: 'export const message = "old line";\n',
      replace_all: false,
      user_modified: false,
      patch_hunks: [
        {
          file_path: 'src/app.ts',
          old_start: 1,
          old_lines: 1,
          new_start: 1,
          new_lines: 1,
          lines: ['- old line', '+ new line'],
        },
      ],
    }),
  );
  assert.equal(sections.kind, 'edit');
  assert.equal(
    sections.body,
    [
      'kind: edit',
      'file: src/app.ts',
      'language: typescript',
      'old: old line',
      'new: new line',
      // original_file carries a trailing newline; appendField preserves it verbatim.
      'original file: export const message = "old line";\n',
      'replace all: false',
      'user modified: false',
    ].join('\n'),
  );
  assert.equal(sections.diff, '*** Update File: src/app.ts\n@@ -1 +1 @@\n- old line\n+ new line');
  // The diff text classifies via diffLineKind at the spec's load-bearing order.
  const kinds = sections.diff.split('\n').map(diffLineKind);
  assert.deepEqual(kinds, ['file', 'hunk', 'del', 'add']);
});

test('toolResultSections places appendToolError in the shared preamble for any kind', () => {
  // The oracle only drove the error block through bash; pin its placement in
  // the common preamble by exercising a non-bash kind (read).
  const sections = toolResultSections(
    resultBlock({
      kind: 'read',
      content: 'file body',
      error: { category: 'file_error', message: 'no such file' },
    }),
  );
  assert.equal(sections.kind, 'read');
  assert.match(sections.body, /error category: file_error/);
  assert.match(sections.body, /error: no such file/);
});

test('toolResultSections edit prefers an explicit patch string over hunks', () => {
  const sections = toolResultSections(
    resultBlock({ kind: 'edit', patch: 'explicit patch', patch_hunks: [{ lines: ['x'] }] }),
  );
  assert.equal(sections.diff, 'explicit patch');
});

test('toolResultSections read renders content and numeric line fields', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'read',
      content: 'file body',
      start_line: 1,
      num_lines: 10,
      total_lines: 100,
    }),
  );
  assert.equal(sections.kind, 'read');
  assert.equal(sections.body, 'kind: read\ncontent: file body\nstart: 1\nlines: 10\ntotal: 100');
});

test('toolResultSections write renders the body and a patch diff', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'write',
      file_path: 'notes.txt',
      language: 'text',
      content: 'wrote notes.txt',
      num_lines: 1,
    }),
  );
  assert.equal(sections.kind, 'write');
  assert.equal(
    sections.body,
    'kind: write\nfile: notes.txt\nlanguage: text\ncontent: wrote notes.txt\nlines: 1',
  );
  assert.equal(sections.diff, '');
});

test('toolResultSections fetch renders url/status/bytes/duration', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'fetch',
      url: 'https://example.com/spec',
      status_code: 200,
      status_text: 'OK',
      bytes: 4096,
      duration_ms: 83,
      content: 'Fetched structured spec content.',
    }),
  );
  assert.equal(sections.kind, 'fetch');
  assert.equal(
    sections.body,
    [
      'kind: fetch',
      'url: https://example.com/spec',
      'status: 200',
      'status text: OK',
      'bytes: 4096',
      'duration ms: 83',
      'content: Fetched structured spec content.',
    ].join('\n'),
  );
});

test('toolResultSections todo renders old/new todo lists', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'todo',
      content: 'todos updated',
      old_todos: [
        {
          content: 'Normalize typed todos',
          status: 'in_progress',
          active_form: 'Normalizing typed todos',
        },
      ],
      new_todos: [
        {
          content: 'Normalize typed todos',
          status: 'completed',
          active_form: 'Normalizing typed todos',
        },
      ],
    }),
  );
  assert.equal(sections.kind, 'todo');
  assert.equal(
    sections.body,
    [
      'kind: todo',
      'content: todos updated',
      'old todos:',
      '- [in_progress] Normalize typed todos (Normalizing typed todos)',
      'new todos:',
      '- [completed] Normalize typed todos (Normalizing typed todos)',
    ].join('\n'),
  );
});

test('toolResultSections plan renders plan/explanation/steps', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'plan',
      plan: 'Expose typed plan data without HTML.',
      content: 'plan captured',
    }),
  );
  assert.equal(sections.kind, 'plan');
  assert.equal(
    sections.body,
    'kind: plan\nplan: Expose typed plan data without HTML.\ncontent: plan captured',
  );
});

test('toolResultSections question renders questions/options/answer/answers', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'question',
      question: 'Select rollout scope',
      questions: [
        {
          question: 'Select rollout scope',
          header: 'Scope',
          multi_select: true,
          options: [
            { label: 'All providers', description: 'Validate first-class and graceful providers' },
            { label: 'Claude only', description: 'Narrow smoke test' },
          ],
        },
      ],
      options: ['All providers', 'Claude only'],
      answer: 'All providers',
      answers: [{ name: 'Select rollout scope', value: 'All providers' }],
      content: 'question answered',
    }),
  );
  assert.equal(sections.kind, 'question');
  assert.equal(
    sections.body,
    [
      'kind: question',
      'question: Select rollout scope',
      'questions:',
      '- Scope | Select rollout scope | multi-select',
      '  options: All providers | Validate first-class and graceful providers; Claude only | Narrow smoke test',
      'options: All providers, Claude only',
      'answer: All providers',
      'answers:',
      '- Select rollout scope: All providers',
      'content: question answered',
    ].join('\n'),
  );
});

test('toolResultSections task renders task fields and total tool calls', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'task',
      task_id: 'task-123',
      task_type: 'subagent',
      task_status: 'completed',
      description: 'Run delegated check',
      total_duration_ms: 1234,
      total_tokens: 321,
      total_tool_use_count: 4,
      output: 'delegated check passed',
      exit_code: 0,
    }),
  );
  assert.equal(sections.kind, 'task');
  assert.equal(
    sections.body,
    [
      'kind: task',
      'task: task-123',
      'task type: subagent',
      'task status: completed',
      'description: Run delegated check',
      'total duration ms: 1234',
      'total tokens: 321',
      'total tool calls: 4',
      'output: delegated check passed',
      'exit 0',
    ].join('\n'),
  );
});

test('toolResultSections grep renders count mode with files/counts/results/limit', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'grep',
      mode: 'count',
      filenames: ['README.md', 'src/app.ts'],
      counts: [
        { name: 'README.md', value: '2' },
        { name: 'src/app.ts', value: '5' },
      ],
      num_files: 2,
      num_results: 7,
      applied_limit: 100,
      content: 'README.md:2\nsrc/app.ts:5\n',
    }),
  );
  assert.equal(sections.kind, 'grep');
  assert.equal(
    sections.body,
    [
      'kind: grep',
      'files: README.md, src/app.ts',
      'mode: count',
      'counts:',
      '- README.md: 2',
      '- src/app.ts: 5',
      'content: README.md:2\nsrc/app.ts:5\n',
      'files: 2',
      'results: 7',
      'applied limit: 100',
    ].join('\n'),
  );
});

test('toolResultSections search renders result items', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'search',
      query: 'structured tool result formats',
      mode: 'query',
      filenames: ['https://example.com/provider-format'],
      num_results: 1,
      result_items: [
        {
          title: 'Provider format notes',
          url: 'https://example.com/provider-format',
          snippet: 'Typed provider-neutral search item.',
        },
      ],
      content: 'https://example.com/provider-format: Provider format notes\n',
    }),
  );
  assert.equal(sections.kind, 'search');
  assert.equal(
    sections.body,
    [
      'kind: search',
      'files: https://example.com/provider-format',
      'query: structured tool result formats',
      'mode: query',
      'result items:',
      '- Provider format notes | https://example.com/provider-format | Typed provider-neutral search item.',
      'content: https://example.com/provider-format: Provider format notes\n',
      'results: 1',
    ].join('\n'),
  );
});

test('toolResultSections glob renders files/duration and truncated flag', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'glob',
      filenames: ['internal/api/session_structured_types.go'],
      num_files: 1,
      duration_ms: 27,
      truncated: true,
    }),
  );
  assert.equal(sections.kind, 'glob');
  assert.equal(
    sections.body,
    [
      'kind: glob',
      'files: internal/api/session_structured_types.go',
      'files: 1',
      'duration ms: 27',
      'truncated',
    ].join('\n'),
  );
});

test('toolResultSections generic fallback appends inline value when only kind rendered', () => {
  const sections = toolResultSections(
    resultBlock({ kind: 'mystery', foo: 'bar' } as unknown as SessionStructuredToolResult),
  );
  assert.equal(sections.kind, 'mystery');
  assert.equal(sections.body, 'kind: mystery\n{"kind":"mystery","foo":"bar"}');
  assert.equal(sections.diff, '');
});

test('toolResultSections generic fallback renders common content/stdout when present', () => {
  const sections = toolResultSections(
    resultBlock({
      kind: 'unknownkind',
      content: 'plain content',
      exit_code: 1,
    } as unknown as SessionStructuredToolResult),
  );
  assert.equal(sections.body, 'kind: unknownkind\ncontent: plain content\nexit 1');
});

test('toolResultSections with no structured payload uses block.content', () => {
  assert.deepEqual(toolResultSections({ type: 'tool_result', content: 'raw string' }), {
    kind: 'result',
    body: 'raw string',
    diff: '',
  });
  assert.deepEqual(toolResultSections({ type: 'tool_result' }), {
    kind: 'result',
    body: '',
    diff: '',
  });
});

// --- imageRows (spec §6) ---------------------------------------------------

test('imageRows renders file/url/mime rows', () => {
  assert.deepEqual(
    imageRows({
      type: 'image',
      file_path: 'screens/shot.png',
      image_url: 'https://example.com/shot.png',
      mime_type: 'image/png',
    }),
    ['file: screens/shot.png', 'url: https://example.com/shot.png', 'mime: image/png'],
  );
  assert.deepEqual(imageRows({ type: 'image' }), []);
});

// --- pendingRows (spec §9) -------------------------------------------------

test('pendingRows renders kind/request/prompt/options', () => {
  const pending: PendingInteraction = {
    kind: 'approval',
    request_id: 'approval-stream',
    prompt: 'Approve streamed write?',
    options: ['Accept', 'Reject'],
  };
  assert.deepEqual(pendingRows(pending), [
    'kind: approval',
    'request: approval-stream',
    'prompt: Approve streamed write?',
    'options: Accept, Reject',
  ]);
});

test('pendingRows omits absent optional fields', () => {
  assert.deepEqual(pendingRows({ kind: 'approval', request_id: 'r-1' }), [
    'kind: approval',
    'request: r-1',
  ]);
});

// --- barrel re-export ------------------------------------------------------

test('barrel re-exports the structured-render module', () => {
  assert.equal(barrelRoleClass, roleClass);
});
