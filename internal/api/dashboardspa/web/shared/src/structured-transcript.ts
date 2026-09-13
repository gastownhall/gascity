// Generated structured transcript wire types (`session.structured.v1`) plus
// the shape guards and pure render helpers the dashboard uses to consume them.
// The committed OpenAPI contract is the sole owner of every SessionStructured*
// DTO below; this module only re-exports or derives compatibility names.

import type {
  PaginationInfo,
  SessionStreamStructuredMessageEvent,
  SessionStructuredArgument,
  SessionStructuredBlock,
  SessionStructuredContinuity,
  SessionStructuredCursor,
  SessionStructuredDiagnostic,
  SessionStructuredGeneration,
  SessionStructuredHistory,
  SessionStructuredIdeSelection,
  SessionStructuredInteraction,
  SessionStructuredMessage,
  SessionStructuredPatchHunk,
  SessionStructuredPlanStep,
  SessionStructuredQuestion,
  SessionStructuredQuestionOption,
  SessionStructuredSearchResultItem,
  SessionStructuredSystemEvent,
  SessionStructuredTailState,
  SessionStructuredTodoItem,
  SessionStructuredToolError,
  SessionStructuredToolInput,
  SessionStructuredToolResult,
  SessionStructuredUploadedFile,
  SessionStructuredUsage,
  SessionStructuredUserPrompt,
  SessionTranscriptStructuredResponse,
} from './generated/gc-supervisor-client/types.gen.js';
import { zSessionStreamStructuredMessageEvent } from './generated/gc-supervisor-client/zod.gen.js';

export type {
  SessionStreamStructuredMessageEvent,
  SessionStructuredArgument,
  SessionStructuredBlock,
  SessionStructuredContinuity,
  SessionStructuredCursor,
  SessionStructuredDiagnostic,
  SessionStructuredGeneration,
  SessionStructuredHistory,
  SessionStructuredInteraction,
  SessionStructuredMessage,
  SessionStructuredPatchHunk,
  SessionStructuredPlanStep,
  SessionStructuredQuestion,
  SessionStructuredQuestionOption,
  SessionStructuredSearchResultItem,
  SessionStructuredSystemEvent,
  SessionStructuredTailState,
  SessionStructuredTodoItem,
  SessionStructuredToolError,
  SessionStructuredToolInput,
  SessionStructuredToolResult,
  SessionStructuredUploadedFile,
  SessionStructuredUsage,
  SessionStructuredUserPrompt,
};

/** The structured transcript schema version emitted on the wire. */
export const STRUCTURED_SCHEMA_VERSION =
  'session.structured.v1' satisfies SessionStreamStructuredMessageEvent['schema_version'];

/** How a structured frame is applied to the current transcript projection. */
export type SessionStructuredOperation = SessionStreamStructuredMessageEvent['operation'];

/** Why a reset frame replaces the current transcript projection. */
export type SessionStructuredResetReason = NonNullable<
  SessionStreamStructuredMessageEvent['reset_reason']
>;

/**
 * Diagnostic code the server attaches when the provider transcript is
 * unavailable and it falls back to provider-neutral text.
 */
export const STRUCTURED_TRANSCRIPT_UNAVAILABLE_CODE = 'transcript_unavailable';

/** Closed block discriminator generated from the structured wire union. */
export type StructuredBlockType = SessionStructuredBlock['type'];

/** Closed tool-input discriminator generated from the structured wire union. */
export type StructuredToolInputKind = SessionStructuredToolInput['kind'];

/** Closed tool-result discriminator generated from the structured wire union. */
export type StructuredToolResultKind = SessionStructuredToolResult['kind'];

/** REST `…/transcript?format=structured` response. */
export type SessionStructuredTranscriptResponse = SessionTranscriptStructuredResponse;

/** Pagination envelope (compatibility name for the generated wire type). */
export type SessionStructuredPagination = PaginationInfo;

/** Compatibility spelling retained for existing dashboard consumers. */
export type SessionStructuredIDESelection = SessionStructuredIdeSelection;

// ---------------------------------------------------------------------------
// Non-message stream frames consumed alongside structured frames. Dashboard-
// owned (the `pending.ts` precedent); the pending frame reuses pending.ts.
// ---------------------------------------------------------------------------

/** SSE `activity` frame: `idle` | `in-turn` (worker may also emit `unknown`). */
export interface SessionActivityEvent {
  activity: string;
}

/** SSE `heartbeat` keepalive frame. */
export interface SessionHeartbeatEvent {
  timestamp: string;
}

// ---------------------------------------------------------------------------
// Shape guards. These reproduce the old dashboard's sse.ts/crew.ts guards'
// accept/reject behavior for real wire frames: shallow envelope discriminators
// that trust the server contract for the remaining fields. One intentional
// hardening: `isRecord` excludes arrays (matching this dashboard's pending.ts
// convention), so an array supplied where an object is expected is rejected.
// The server never sends arrays for these fields, so real traffic is unchanged.
// ---------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** True for a `structured` SSE frame / structured transcript body. */
export function isSessionStructuredEvent(
  data: unknown,
): data is SessionStreamStructuredMessageEvent {
  if (
    !isRecord(data) ||
    data.format !== 'structured' ||
    data.schema_version !== STRUCTURED_SCHEMA_VERSION ||
    typeof data.id !== 'string' ||
    typeof data.template !== 'string' ||
    typeof data.provider !== 'string' ||
    !Array.isArray(data.structured_messages) ||
    !data.structured_messages.every(isStructuredMessage) ||
    !zSessionStreamStructuredMessageEvent.safeParse(data).success
  ) {
    return false;
  }
  if (!isSessionStructuredHistory(data.history)) return false;
  switch (data.operation) {
    case 'snapshot':
    case 'upsert':
      return data.reset_reason === undefined;
    case 'reset':
      return isSessionStructuredResetReason(data.reset_reason);
    default:
      return false;
  }
}

function isSessionStructuredResetReason(value: unknown): value is SessionStructuredResetReason {
  return (
    value === 'resume_invalid' ||
    value === 'stream_changed' ||
    value === 'cursor_invalidated' ||
    value === 'history_rewritten'
  );
}

/** True for an `activity` SSE frame. */
export function isSessionActivityEvent(data: unknown): data is SessionActivityEvent {
  return isRecord(data) && typeof data.activity === 'string';
}

/** True for a `heartbeat` SSE frame. */
export function isSessionHeartbeatEvent(data: unknown): data is SessionHeartbeatEvent {
  return isRecord(data) && typeof data.timestamp === 'string';
}

/**
 * True for a renderable history envelope — requires the load-bearing nested
 * fields the renderer reads (the old `isSessionStructuredHistory`, with the
 * array-excluding `isRecord` above).
 */
export function isSessionStructuredHistory(value: unknown): value is SessionStructuredHistory {
  if (!isRecord(value)) return false;
  if (typeof value.transcript_stream_id !== 'string') return false;
  const generation = value.generation;
  if (!isRecord(generation) || typeof generation.id !== 'string') return false;
  const cursor = value.cursor;
  if (!isRecord(cursor) || typeof cursor.resume_token !== 'string' || cursor.resume_token === '')
    return false;
  const continuity = value.continuity;
  if (!isRecord(continuity) || typeof continuity.status !== 'string') return false;
  const tailState = value.tail_state;
  if (!isRecord(tailState) || typeof tailState.activity !== 'string') return false;
  return true;
}

/** True for a structured message — requires the `blocks` array (matches old `isStructuredMessage`). */
export function isStructuredMessage(value: unknown): value is SessionStructuredMessage {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    isSessionStructuredRole(value.role) &&
    typeof value.status === 'string' &&
    Array.isArray(value.blocks) &&
    value.blocks.every(isSessionStructuredBlock)
  );
}

function isSessionStructuredRole(value: unknown): value is SessionStructuredMessage['role'] {
  return (
    value === 'unknown' ||
    value === 'user' ||
    value === 'assistant' ||
    value === 'system' ||
    value === 'tool'
  );
}

function isSessionStructuredBlock(value: unknown): value is SessionStructuredBlock {
  if (!isRecord(value)) return false;
  return (
    value.type === 'text' ||
    value.type === 'thinking' ||
    value.type === 'tool_use' ||
    value.type === 'tool_result' ||
    value.type === 'interaction' ||
    value.type === 'image' ||
    value.type === 'unknown'
  );
}

/**
 * Extract the renderable structured messages from an envelope, dropping any
 * element that is not a well-formed message. Mirrors the old
 * `structuredMessagesFromEnvelope` consumer helper.
 */
export function structuredMessagesFromEnvelope(
  event: SessionStreamStructuredMessageEvent,
): SessionStructuredMessage[] {
  if (!Array.isArray(event.structured_messages)) return [];
  return event.structured_messages.filter(isStructuredMessage);
}

// ---------------------------------------------------------------------------
// Pure render helpers (ported from the old dashboard crew.ts at parity).
// ---------------------------------------------------------------------------

function formatPatchRange(start: number | undefined, lines: number | undefined): string {
  const safeStart = start ?? 1;
  if (lines === undefined || lines === 1) return String(safeStart);
  return `${safeStart},${lines}`;
}

function formatPatchHunkHeader(hunk: SessionStructuredPatchHunk): string {
  const oldStart = hunk.old_start;
  const newStart = hunk.new_start;
  if (oldStart === undefined && newStart === undefined) return '@@';
  return `@@ -${formatPatchRange(oldStart, hunk.old_lines)} +${formatPatchRange(newStart, hunk.new_lines)} @@`;
}

/**
 * Render edit/write patch hunks to unified-diff text. Emits a
 * `*** Update File: <path>` separator each time the hunk's file_path changes,
 * a `@@ … @@` header per hunk, then the hunk's lines verbatim.
 */
export function patchTextFromHunks(
  hunks: readonly SessionStructuredPatchHunk[] | null | undefined,
): string {
  if (hunks === undefined || hunks === null || hunks.length === 0) return '';
  const lines: string[] = [];
  let lastFilePath = '';
  for (const hunk of hunks) {
    const filePath = hunk.file_path ?? '';
    if (filePath !== '' && filePath !== lastFilePath) {
      lines.push(`*** Update File: ${filePath}`);
      lastFilePath = filePath;
    }
    lines.push(formatPatchHunkHeader(hunk));
    if (hunk.lines !== undefined && hunk.lines !== null) {
      for (const line of hunk.lines) lines.push(line);
    }
  }
  return lines.join('\n');
}

function appendUsagePart(parts: string[], label: string, value: number | undefined): void {
  // Zero token counts are dropped (distinct from the context pair/percent below).
  if (value !== undefined && value !== 0) parts.push(`${label} ${value}`);
}

/**
 * Render provider-neutral token usage to the compact `tokens …` summary line.
 * Zero token counts are dropped; the context pair and percent render whenever
 * defined (including an explicit `0%`). Returns `""` when nothing renders.
 */
export function formatUsage(usage: SessionStructuredUsage | undefined): string {
  if (usage === undefined) return '';
  const parts: string[] = [];
  appendUsagePart(parts, 'in', usage.input_tokens);
  appendUsagePart(parts, 'out', usage.output_tokens);
  appendUsagePart(parts, 'reason', usage.reasoning_tokens);
  appendUsagePart(parts, 'cache', usage.cache_read_tokens);
  appendUsagePart(parts, 'write', usage.cache_creation_tokens);
  const contextUsed = usage.context_used_tokens;
  const contextWindow = usage.context_window_tokens;
  if (contextUsed !== undefined && contextWindow !== undefined) {
    parts.push(`${contextUsed}/${contextWindow}`);
  }
  const contextPercent = usage.context_percent;
  if (contextPercent !== undefined) parts.push(`${contextPercent}%`);
  return parts.length > 0 ? `tokens ${parts.join(' ')}` : '';
}
