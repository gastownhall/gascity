// Pure formatting layer for PR #3718's structured transcript rendering, ported
// from the old dashboard's crew.ts at parity (spec: `.dashport-spec/02-old-render.md`).
//
// This module contains NO React and NO DOM construction. Every export returns
// plain strings, string[], or small section objects; the React layer (Slice 3b)
// maps those to JSX. The text content produced here is the parity contract — it
// reproduces the exact `<pre>`/header/diff text the old crew.test.ts asserted.
//
// Because the wire is now typed (Slice 2's `structured-transcript.ts`), these
// helpers operate on the typed fields directly instead of the old
// `recordOf`/`unknown` probing — but the emitted output is byte-for-byte the
// same as the old `append*` helpers (e.g. `appendField` emits a row only for a
// non-empty string; `appendNumber` keeps an explicit zero; `formatUsage`'s
// zero-skip lives in `structured-transcript.ts`).

import { patchTextFromHunks } from './structured-transcript.js';
import type {
  SessionStructuredArgument,
  SessionStructuredBlock,
  SessionStructuredHistory,
  SessionStructuredPlanStep,
  SessionStructuredQuestion,
  SessionStructuredSearchResultItem,
  SessionStructuredSystemEvent,
  SessionStructuredToolError,
  SessionStructuredToolInput,
  SessionStructuredToolResult,
  SessionStructuredTodoItem,
  SessionStructuredUploadedFile,
  SessionStructuredUserPrompt,
} from './structured-transcript.js';
import type { PendingInteraction } from './pending.js';

// `formatUsage` and `patchTextFromHunks` stay owned by `structured-transcript.ts`
// (already barrel-exported there); this module imports them internally and does
// NOT re-export them, so the package barrel has a single source for each symbol.

// ---------------------------------------------------------------------------
// Low-level value coercion (internal). Typed inputs make most of the old
// `recordOf` probing unnecessary, but `formatArgument` still faces genuinely
// `unknown` values (argument records whose `value` can be any JSON type).
// ---------------------------------------------------------------------------

function recordOf(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

// ---------------------------------------------------------------------------
// Row helpers (internal). Each mutates the `rows` array in place, matching the
// old crew.ts `append*` signatures and emission rules exactly.
// ---------------------------------------------------------------------------

function appendField(rows: string[], label: string, value: string | undefined): void {
  if (value === undefined || value === '') return;
  rows.push(`${label}: ${value}`);
}

function appendNumber(rows: string[], label: string, value: number | undefined): void {
  if (value === undefined) return;
  rows.push(`${label}: ${String(value)}`);
}

function appendBoolean(rows: string[], label: string, value: boolean | undefined): void {
  if (value === undefined) return;
  rows.push(`${label}: ${String(value)}`);
}

function appendExit(rows: string[], value: number | undefined): void {
  if (value === undefined) return;
  rows.push(`exit ${String(value)}`);
}

function appendFlags(
  rows: string[],
  structured: Extract<SessionStructuredToolResult, { kind: 'bash' | 'python' | 'glob' }>,
): void {
  if (structured.truncated === true) rows.push('truncated');
  if ('interrupted' in structured && structured.interrupted === true) rows.push('interrupted');
}

function appendStringList(
  rows: string[],
  label: string,
  value: readonly string[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  const parts = value.filter((item) => item !== '');
  if (parts.length === 0) return;
  rows.push(`${label}: ${parts.join(', ')}`);
}

function appendUploadedFiles(
  rows: string[],
  value: readonly SessionStructuredUploadedFile[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push('uploaded files:');
  for (const file of value) {
    const name = file.original_name ?? '';
    const size = file.size ?? '';
    const mime = file.mime_type ?? '';
    const path = file.file_path ?? '';
    const preview = file.preview_url ?? '';
    const detail = [size, mime].filter((part) => part !== '').join(', ');
    const suffix = preview !== '' ? ` preview: ${preview}` : '';
    rows.push(`- ${name}${detail !== '' ? ` (${detail})` : ''}${path !== '' ? `: ${path}` : ''}${suffix}`);
  }
}

function appendIDESelections(
  rows: string[],
  value: readonly { text?: string }[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  const selections = value.map((item) => item.text ?? '').filter((text) => text !== '');
  if (selections.length === 0) return;
  rows.push('selections:');
  for (const selection of selections) rows.push(`- ${selection}`);
}

function appendPlanSteps(
  rows: string[],
  value: readonly SessionStructuredPlanStep[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push('steps:');
  value.forEach((step, index) => {
    const text = step.step ?? '';
    const status = step.status ?? '';
    const parts = [
      status !== '' ? `[${status}]` : '',
      text !== '' ? text : `step ${index + 1}`,
    ].filter((part) => part !== '');
    rows.push(`- ${parts.join(' ')}`);
  });
}

function appendArgumentList(
  rows: string[],
  label: string,
  value: readonly SessionStructuredArgument[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push(`${label}:`);
  for (const item of value) {
    const formatted = formatArgument(item);
    if (formatted !== '') rows.push(`- ${formatted}`);
  }
}

function appendSearchResultItems(
  rows: string[],
  value: readonly SessionStructuredSearchResultItem[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push('result items:');
  value.forEach((item, index) => {
    const title = item.title ?? '';
    const url = item.url ?? '';
    const snippet = item.snippet ?? '';
    const label = title !== '' ? title : `result ${index + 1}`;
    const parts = [label, url, snippet].filter((part) => part !== '');
    rows.push(`- ${parts.join(' | ')}`);
  });
}

function appendQuestions(
  rows: string[],
  value: readonly SessionStructuredQuestion[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push('questions:');
  value.forEach((question, index) => {
    const text = question.question ?? '';
    const header = question.header ?? '';
    const multiSelect = question.multi_select === true ? 'multi-select' : '';
    const label = text !== '' ? text : `question ${index + 1}`;
    const parts = [header, label, multiSelect].filter((part) => part !== '');
    rows.push(`- ${parts.join(' | ')}`);
    const options = question.options;
    if (options !== undefined && options !== null && options.length > 0) {
      const rendered = options
        .map((option) => {
          const optionLabel = option.label ?? '';
          const description = option.description ?? '';
          return [optionLabel, description].filter((part) => part !== '').join(' | ');
        })
        .filter((part) => part !== '');
      if (rendered.length > 0) rows.push(`  options: ${rendered.join('; ')}`);
    }
  });
}

function appendTodoList(
  rows: string[],
  label: string,
  value: readonly SessionStructuredTodoItem[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push(`${label}:`);
  value.forEach((todo, index) => {
    const status = todo.status ?? '';
    const content = todo.content ?? '';
    const activeForm = todo.active_form ?? '';
    const priority = todo.priority ?? '';
    const parts = [
      status !== '' ? `[${status}]` : '',
      content !== '' ? content : `todo ${index + 1}`,
      priority !== '' ? `priority ${priority}` : '',
      activeForm !== '' ? `(${activeForm})` : '',
    ].filter((part) => part !== '');
    rows.push(`- ${parts.join(' ')}`);
  });
}

function appendToolError(rows: string[], value: SessionStructuredToolError | undefined): void {
  if (value === undefined) return;
  appendField(rows, 'error category', value.category);
  appendField(rows, 'error', value.message);
  appendField(rows, 'user reason', value.user_reason);
}

// ---------------------------------------------------------------------------
// Inline value / argument formatting.
// ---------------------------------------------------------------------------

/**
 * Render an arbitrary value to a single inline string: `null`/`undefined` → "";
 * a string → itself; number/boolean → `String(value)`; anything else →
 * `JSON.stringify` (falling back to `String` if that throws). Spec §10.
 */
export function formatInlineValue(value: unknown): string {
  if (value === null || value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/**
 * Render a `{name, value}` argument record to `"<name>: <value>"`. A non-record
 * falls back to `formatInlineValue`; a missing `name` defaults to `"argument"`;
 * a non-string `value` is rendered via `formatInlineValue`. Spec §10.
 */
export function formatArgument(value: unknown): string {
  const argument = recordOf(value);
  if (argument === null) return formatInlineValue(value);
  const name = typeof argument.name === 'string' ? argument.name : 'argument';
  const argValue = typeof argument.value === 'string' ? argument.value : formatInlineValue(argument.value);
  return `${name}: ${argValue}`;
}

// ---------------------------------------------------------------------------
// CSS-class helpers.
// ---------------------------------------------------------------------------

/**
 * Map a message role to its header class suffix: `assistant`/`agent` →
 * "assistant", `system` → "system", `result` → "result", anything else →
 * "user". Spec §1.
 */
export function roleClass(role: string): string {
  switch ((role ?? '').toLowerCase()) {
    case 'assistant':
    case 'agent':
      return 'assistant';
    case 'system':
      return 'system';
    case 'result':
      return 'result';
    default:
      return 'user';
  }
}

/** Semantic class of a unified-diff line; the React layer maps each kind to a style. */
export type DiffLineKind = 'hunk' | 'file' | 'add' | 'del' | 'context';

/**
 * Classify a unified-diff line. The prefix checks run top-down (first match
 * wins) and the order is load-bearing — `---`/`+++` file headers must be matched
 * before the single `-`/`+` add/del rules. Spec __diffRules__ (the old dashboard
 * baked these into `log-msg-diff-*` CSS classes; the new SPA maps the kind to
 * Tailwind, so this returns the semantic kind, not a class string).
 */
export function diffLineKind(line: string): DiffLineKind {
  if (line.startsWith('@@')) return 'hunk';
  if (
    line.startsWith('diff --git') ||
    line.startsWith('index ') ||
    line.startsWith('*** ') ||
    line.startsWith('---') ||
    line.startsWith('+++')
  ) {
    return 'file';
  }
  if (line.startsWith('+')) return 'add';
  if (line.startsWith('-')) return 'del';
  return 'context';
}

// ---------------------------------------------------------------------------
// Interaction / pending.
// ---------------------------------------------------------------------------

/**
 * Render an `interaction` block to its single summary line:
 * `[kind, state, request_id, action, prompt, options.join(", ")]` with the
 * empty parts filtered out and the rest space-joined. `kind` defaults to
 * "interaction". Spec §9.
 */
export function formatInteraction(block: SessionStructuredBlock): string {
  const interaction =
    block.type === 'interaction' || block.type === 'unknown' ? block.interaction : undefined;
  const kind = interaction?.kind ?? 'interaction';
  const state = interaction?.state ?? '';
  const prompt = interaction?.prompt ?? '';
  const requestID = interaction?.request_id ?? '';
  const action = interaction?.action ?? '';
  const options = interaction?.options?.join(', ') ?? '';
  return [kind, state, requestID, action, prompt, options].filter(Boolean).join(' ');
}

/** Build the `<pre>` rows for a streamed pending-interaction frame. Spec §9. */
export function pendingRows(pending: PendingInteraction): string[] {
  const rows: string[] = [];
  appendField(rows, 'kind', pending.kind);
  appendField(rows, 'request', pending.request_id);
  appendField(rows, 'prompt', pending.prompt);
  appendStringList(rows, 'options', pending.options === undefined ? undefined : [...pending.options]);
  return rows;
}

// ---------------------------------------------------------------------------
// Metadata / history rows.
// ---------------------------------------------------------------------------

/** Build the user-prompt metadata rows (prompt text, opened/uploaded files, IDE selections). Spec §2. */
export function userPromptRows(prompt: SessionStructuredUserPrompt): string[] {
  const rows: string[] = [];
  appendField(rows, 'prompt', prompt.text);
  appendStringList(rows, 'opened files', prompt.opened_files);
  appendUploadedFiles(rows, prompt.uploaded_files);
  appendIDESelections(rows, prompt.selections);
  return rows;
}

/** Build the system-event metadata rows (kind/category/code/message, in order). Spec §3. */
export function systemEventRows(event: SessionStructuredSystemEvent): string[] {
  const rows: string[] = [];
  appendField(rows, 'kind', event.kind);
  appendField(rows, 'category', event.category);
  appendField(rows, 'code', event.code);
  appendField(rows, 'message', event.message);
  return rows;
}

/** Build the structured-history envelope rows in full spec order, including diagnostics. Spec §4. */
export function historyRows(history: SessionStructuredHistory): string[] {
  const rows: string[] = [];
  appendField(rows, 'stream', history.transcript_stream_id);
  appendField(rows, 'provider session', history.provider_session_id);
  appendField(rows, 'conversation', history.logical_conversation_id);
  appendField(rows, 'gc session', history.gc_session_id);

  appendField(rows, 'generation', history.generation.id);
  appendField(rows, 'observed', history.generation.observed_at);

  appendField(rows, 'cursor', history.cursor.after_entry_id);

  appendField(rows, 'continuity', history.continuity.status);
  appendNumber(rows, 'compactions', history.continuity.compaction_count);
  if (history.continuity.has_branches === true) rows.push('branches: yes');
  appendField(rows, 'note', history.continuity.note);

  appendField(rows, 'activity', history.tail_state.activity);
  appendField(rows, 'last entry', history.tail_state.last_entry_id);
  appendStringList(rows, 'open tools', history.tail_state.open_tool_call_ids);
  appendStringList(rows, 'pending', history.tail_state.pending_interaction_ids);
  if (history.tail_state.degraded === true) rows.push('degraded: yes');
  appendField(rows, 'degraded reason', history.tail_state.degraded_reason);

  for (const diagnostic of history.diagnostics ?? []) {
    const parts: string[] = [];
    appendField(parts, 'code', diagnostic.code);
    appendNumber(parts, 'count', diagnostic.count);
    appendField(parts, 'message', diagnostic.message);
    if (parts.length > 0) rows.push(`diagnostic: ${parts.join(', ')}`);
  }

  return rows;
}

// ---------------------------------------------------------------------------
// Image block.
// ---------------------------------------------------------------------------

/** Build the image-block metadata rows (file/url/mime). The `<img>` itself is the React layer's job. Spec §6. */
export function imageRows(block: SessionStructuredBlock): string[] {
  const rows: string[] = [];
  if (block.type !== 'image' && block.type !== 'unknown') return rows;
  appendField(rows, 'file', block.file_path);
  appendField(rows, 'url', block.image_url);
  appendField(rows, 'mime', block.mime_type);
  return rows;
}

// ---------------------------------------------------------------------------
// Tool input.
// ---------------------------------------------------------------------------

/**
 * Build the tool-input `<pre>` rows for a `tool_use` block, in the exact
 * appendField/list ordering of the old `renderToolInput`. When the block has no
 * structured input, falls back to a single `formatInlineValue` line (or an empty
 * row list when input is absent). Spec §7.
 */
export function toolInputRows(input: SessionStructuredToolInput): string[] {
  const rows: string[] = [];
  appendField(rows, 'kind', input.kind);

  switch (input.kind) {
    case 'command':
      appendField(rows, 'command', input.command);
      appendArgumentRows(rows, input.arguments);
      break;
    case 'stdin':
      appendField(rows, 'task', input.task_id);
      appendField(rows, 'linked command', input.linked_command);
      appendField(rows, 'text', input.text);
      break;
    case 'code':
      appendField(rows, 'language', input.language);
      appendField(rows, 'code', input.code);
      break;
    case 'patch':
      appendField(rows, 'file', input.file_path);
      appendField(rows, 'language', input.language);
      appendField(rows, 'patch', input.patch);
      break;
    case 'write':
      appendField(rows, 'file', input.file_path);
      appendField(rows, 'language', input.language);
      appendField(rows, 'text', input.text);
      break;
    case 'glob':
    case 'search':
      appendField(rows, 'file', input.file_path);
      if (input.kind === 'search') appendField(rows, 'command', input.command);
      appendField(rows, 'query', input.query);
      appendField(rows, 'pattern', input.pattern);
      appendArgumentRows(rows, input.arguments);
      break;
    case 'fetch':
      appendField(rows, 'url', input.url);
      appendField(rows, 'prompt', input.prompt);
      break;
    case 'file':
      appendField(rows, 'file', input.file_path);
      appendField(rows, 'language', input.language);
      appendField(rows, 'command', input.command);
      break;
    case 'todo':
      appendTodoList(rows, 'todos', input.todos);
      break;
    case 'plan':
      appendField(rows, 'plan', input.plan);
      appendField(rows, 'explanation', input.explanation);
      appendPlanSteps(rows, input.steps);
      break;
    case 'question':
      appendField(rows, 'question', input.question);
      appendStringList(rows, 'options', input.options);
      break;
    case 'task':
      appendField(rows, 'prompt', input.prompt);
      appendField(rows, 'task', input.task_id);
      appendField(rows, 'task type', input.task_type);
      appendField(rows, 'task status', input.task_status);
      appendField(rows, 'description', input.description);
      break;
    case 'text':
      appendField(rows, 'text', input.text);
      break;
    case 'arguments':
      appendArgumentRows(rows, input.arguments);
      break;
    case 'unknown':
      appendField(rows, 'file', input.file_path);
      appendField(rows, 'language', input.language);
      appendField(rows, 'url', input.url);
      appendField(rows, 'prompt', input.prompt);
      appendField(rows, 'task', input.task_id);
      appendField(rows, 'task type', input.task_type);
      appendField(rows, 'task status', input.task_status);
      appendField(rows, 'description', input.description);
      appendField(rows, 'question', input.question);
      appendStringList(rows, 'options', input.options);
      appendField(rows, 'command', input.command);
      appendField(rows, 'linked command', input.linked_command);
      appendField(rows, 'code', input.code);
      appendField(rows, 'query', input.query);
      appendField(rows, 'pattern', input.pattern);
      appendField(rows, 'plan', input.plan);
      appendField(rows, 'explanation', input.explanation);
      appendPlanSteps(rows, input.steps);
      appendField(rows, 'text', input.text);
      appendField(rows, 'patch', input.patch);
      appendTodoList(rows, 'todos', input.todos);
      appendArgumentRows(rows, input.arguments);
      break;
  }
  if (rows.length === 0) rows.push(formatInlineValue(input));
  return rows;
}

function appendArgumentRows(
  rows: string[],
  value: readonly SessionStructuredArgument[] | null | undefined,
): void {
  if (value === undefined || value === null || value.length === 0) return;
  rows.push(...value.map((argument) => formatArgument(argument)));
}

// ---------------------------------------------------------------------------
// Tool result.
// ---------------------------------------------------------------------------

/** A rendered tool-result: the title `kind`, the `<pre>` body text, and the diff text (empty when none). */
export interface ToolResultSections {
  kind: string;
  body: string;
  diff: string;
}

/**
 * Build the body + diff text for a `tool_result` block, reproducing the old
 * `renderToolResult`: a common preamble (kind/file/language/error), a per-kind
 * branch (bash, python, stdin, edit, read, write, fetch, todo, plan, question,
 * task, and the shared grep|search|glob branch), and a generic fallback. The
 * `body` is `lines.filter(Boolean).join("\n")`; the `diff` is the edit/write
 * patch text. When the block carries no structured payload, the body comes from
 * `block.content` and `kind` is "result". Spec §8 + __perKindRendering__.
 */
export function toolResultSections(block: SessionStructuredBlock): ToolResultSections {
  const structured =
    block.type === 'tool_result' || block.type === 'unknown' ? block.structured : undefined;
  if (structured === undefined) {
    const content =
      block.type === 'tool_result' || block.type === 'unknown' ? block.content : undefined;
    if (typeof content === 'string') return { kind: 'result', body: content, diff: '' };
    if (content !== undefined)
      return { kind: 'result', body: formatInlineValue(content), diff: '' };
    return { kind: 'result', body: '', diff: '' };
  }

  const kind = structured.kind;
  const lines: string[] = [];
  appendField(lines, 'kind', kind);
  appendField(lines, 'file', 'file_path' in structured ? structured.file_path : undefined);
  appendField(lines, 'language', 'language' in structured ? structured.language : undefined);
  appendToolError(lines, structured.error);

  if (structured.kind === 'bash') {
    appendField(lines, 'command', structured.command);
    appendField(lines, 'task', structured.task_id);
    appendField(lines, 'task status', structured.task_status);
    appendField(lines, 'stdout', structured.stdout);
    appendField(lines, 'stderr', structured.stderr);
    appendNumber(lines, 'stdout lines', structured.stdout_lines);
    appendNumber(lines, 'stderr lines', structured.stderr_lines);
    appendField(lines, 'timestamp', structured.timestamp);
    appendExit(lines, structured.exit_code);
    appendFlags(lines, structured);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'python') {
    appendField(lines, 'code', structured.code);
    appendField(lines, 'stdout', structured.stdout);
    appendField(lines, 'stderr', structured.stderr);
    appendExit(lines, structured.exit_code);
    appendFlags(lines, structured);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'stdin') {
    appendField(lines, 'task', structured.task_id);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'edit') {
    const patch = (structured.patch ?? '') || patchTextFromHunks(structured.patch_hunks);
    appendField(lines, 'old', structured.old_string);
    appendField(lines, 'new', structured.new_string);
    appendField(lines, 'original file', structured.original_file);
    appendBoolean(lines, 'replace all', structured.replace_all);
    appendBoolean(lines, 'user modified', structured.user_modified);
    appendField(lines, 'content', structured.content);
    return { kind, body: joinBody(lines), diff: patch };
  }
  if (structured.kind === 'read') {
    appendField(lines, 'content', structured.content);
    appendNumber(lines, 'start', structured.start_line);
    appendNumber(lines, 'lines', structured.num_lines);
    appendNumber(lines, 'total', structured.total_lines);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'write') {
    const patch = (structured.patch ?? '') || patchTextFromHunks(structured.patch_hunks);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    appendNumber(lines, 'start', structured.start_line);
    appendNumber(lines, 'lines', structured.num_lines);
    appendNumber(lines, 'total', structured.total_lines);
    return { kind, body: joinBody(lines), diff: patch };
  }
  if (structured.kind === 'fetch') {
    appendField(lines, 'url', structured.url);
    appendNumber(lines, 'status', structured.status_code);
    appendField(lines, 'status text', structured.status_text);
    appendNumber(lines, 'bytes', structured.bytes);
    appendNumber(lines, 'duration ms', structured.duration_ms);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'todo') {
    appendField(lines, 'content', structured.content);
    appendTodoList(lines, 'old todos', structured.old_todos);
    appendTodoList(lines, 'new todos', structured.new_todos);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'plan') {
    appendField(lines, 'plan', structured.plan);
    appendField(lines, 'explanation', structured.explanation);
    appendPlanSteps(lines, structured.steps);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'question') {
    appendField(lines, 'question', structured.question);
    appendQuestions(lines, structured.questions);
    appendStringList(lines, 'options', structured.options);
    appendField(lines, 'answer', structured.answer);
    appendArgumentList(lines, 'answers', structured.answers);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'task') {
    appendField(lines, 'task', structured.task_id);
    appendField(lines, 'task type', structured.task_type);
    appendField(lines, 'task status', structured.task_status);
    appendField(lines, 'description', structured.description);
    appendNumber(lines, 'total duration ms', structured.total_duration_ms);
    appendNumber(lines, 'total tokens', structured.total_tokens);
    appendNumber(lines, 'total tool calls', structured.total_tool_use_count);
    appendField(lines, 'output', structured.output);
    appendField(lines, 'stdout', structured.stdout);
    appendField(lines, 'stderr', structured.stderr);
    appendExit(lines, structured.exit_code);
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }
  if (structured.kind === 'grep' || structured.kind === 'search') {
    if (
      structured.filenames !== undefined &&
      structured.filenames !== null &&
      structured.filenames.length > 0
    ) {
      appendField(lines, 'files', structured.filenames.join(', '));
    }
    appendField(lines, 'query', structured.query);
    appendField(lines, 'mode', structured.mode);
    appendArgumentList(lines, 'counts', structured.counts);
    appendSearchResultItems(lines, structured.result_items);
    appendField(lines, 'content', structured.content);
    appendNumber(lines, 'files', structured.num_files);
    appendNumber(lines, 'results', structured.num_results);
    appendNumber(lines, 'duration ms', structured.duration_ms);
    appendNumber(lines, 'applied limit', structured.applied_limit);
    appendNumber(lines, 'lines', structured.num_lines);
    return { kind, body: joinBody(lines), diff: '' };
  }

  if (structured.kind === 'glob') {
    if (
      structured.filenames !== undefined &&
      structured.filenames !== null &&
      structured.filenames.length > 0
    ) {
      appendField(lines, 'files', structured.filenames.join(', '));
    }
    appendField(lines, 'content', structured.content);
    appendNumber(lines, 'files', structured.num_files);
    appendNumber(lines, 'duration ms', structured.duration_ms);
    appendNumber(lines, 'lines', structured.num_lines);
    appendFlags(lines, structured);
    return { kind, body: joinBody(lines), diff: '' };
  }

  if (structured.kind === 'text') {
    appendField(lines, 'content', structured.content);
    appendField(lines, 'text', structured.text);
    return { kind, body: joinBody(lines), diff: '' };
  }

  appendField(lines, 'content', structured.content);
  appendField(lines, 'text', structured.text);
  appendField(lines, 'stdout', structured.stdout);
  appendField(lines, 'stderr', structured.stderr);
  appendExit(lines, structured.exit_code);
  if (lines.length === 1) lines.push(formatInlineValue(structured));
  return { kind, body: joinBody(lines), diff: '' };
}

/** Body text = the non-empty result lines joined by newlines (mirrors `toolResultNodes`). */
function joinBody(lines: string[]): string {
  return lines.filter(Boolean).join('\n');
}
