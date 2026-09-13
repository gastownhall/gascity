import {
  formatInlineValue,
  formatInteraction,
  formatUsage,
  historyRows,
  imageRows,
  pendingRows,
  roleClass,
  systemEventRows,
  toolInputRows,
  toolResultSections,
  userPromptRows,
  type PendingInteraction,
  type SessionStructuredBlock,
  type SessionStructuredHistory,
  type SessionStructuredMessage,
  type SessionStructuredSystemEvent,
  type SessionStructuredUserPrompt,
} from 'gas-city-dashboard-shared';
import type { ReactNode } from 'react';
import { formatClockTime } from '../../hooks/time';
import { DiffView } from './DiffView.js';

// React renderer for PR #3718's structured transcript. This layer produces ONLY
// the JSX shell; every piece of text content comes from the pure helpers in
// `gas-city-dashboard-shared` (structured-render.ts / structured-transcript.ts),
// so the parity contract asserted by the old crew.test.ts is preserved verbatim.
// The old `log-msg-*` BEM classes do not exist in this SPA — each element is
// rendered in the Tailwind design-token idiom shared with SessionPeek.

// roleClass suffix → header tone, mirroring SessionPeek's roleTone palette.
const ROLE_TONE: Record<string, string> = {
  assistant: 'text-accent',
  system: 'text-warn',
  result: 'text-fg-muted',
  user: 'text-fg',
};

function RoleLabel({ role }: { role: string }) {
  const tone = ROLE_TONE[roleClass(role)] ?? 'text-fg-faint';
  return <span className={`text-label uppercase tracking-wider font-medium ${tone}`}>{role}</span>;
}

// Small uppercase metadata chip used for the secondary header fields (provider,
// model, usage, status, …) so they read as labels next to the role/time.
function HeaderMeta({ children }: { children: string }) {
  return <span className="text-label uppercase tracking-wider text-fg-faint tnum">{children}</span>;
}

// Title row shared by every tool/metadata block: a kind chip then a label.
function BlockTitle({ kind, label }: { kind: string; label: string }) {
  return (
    <div className="text-label uppercase tracking-wider text-fg-muted">
      <span className="text-accent">{kind}</span> {label}
    </div>
  );
}

// A tool/metadata block: title + a pre body built from already-joined rows.
function ToolBlock({
  kind,
  label,
  body,
  children,
  isError,
}: {
  kind: string;
  label: string;
  body?: string;
  children?: ReactNode;
  isError?: boolean;
}) {
  return (
    <div className={`space-y-1 ${isError === true ? 'text-warn' : ''}`}>
      <BlockTitle kind={kind} label={label} />
      {body !== undefined && body !== '' && (
        <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto">{body}</pre>
      )}
      {children}
    </div>
  );
}

/** History envelope block (rendered before the messages). Spec §4. */
export function StructuredHistory({ history }: { history: SessionStructuredHistory }) {
  const rows = historyRows(history);
  return <ToolBlock kind="history" label="structured session" body={rows.join('\n') || 'structured history'} />;
}

/** User-prompt metadata block. Renders nothing when there are no rows. Spec §2. */
export function UserPromptMetadata({ prompt }: { prompt: SessionStructuredUserPrompt }) {
  const rows = userPromptRows(prompt);
  if (rows.length === 0) return null;
  return <ToolBlock kind="user" label="prompt" body={rows.join('\n')} />;
}

/** System-event metadata block. Renders nothing when there are no rows. Spec §3. */
export function SystemEventMetadata({ event }: { event: SessionStructuredSystemEvent }) {
  const rows = systemEventRows(event);
  if (rows.length === 0) return null;
  return <ToolBlock kind="system" label="event" body={rows.join('\n')} />;
}

/** `tool_use` block: a `tool` chip with the tool name plus the input `<pre>`. Spec §7. */
export function ToolUseBlock({
  block,
}: {
  block: Extract<SessionStructuredBlock, { type: 'tool_use' }>;
}) {
  const rows = block.input !== undefined ? toolInputRows(block.input) : [];
  return (
    <ToolBlock kind="tool" label={block.name !== undefined && block.name !== '' ? block.name : 'tool'} body={rows.join('\n')} />
  );
}

/** `tool_result` block: a `{kind} result` chip, the body `<pre>`, and a diff when present. Spec §8. */
export function ToolResultBlock({
  block,
}: {
  block: Extract<SessionStructuredBlock, { type: 'tool_result' }>;
}) {
  const { kind, body, diff } = toolResultSections(block);
  return (
    <ToolBlock
      kind={kind}
      label={block.is_error === true ? 'error result' : 'result'}
      body={body}
      isError={block.is_error === true}
    >
      {diff !== '' && <DiffView text={diff} />}
    </ToolBlock>
  );
}

/** `image` block: file/url/mime rows plus the inline `<img>` when an image_url is present. Spec §6. */
export function ImageBlock({
  block,
}: {
  block: Extract<SessionStructuredBlock, { type: 'image' }>;
}) {
  const rows = imageRows(block);
  const imageUrl = renderableImageUrl(block.image_url);
  return (
    <ToolBlock kind="image" label="block" body={rows.join('\n') || 'image'}>
      {typeof imageUrl === 'string' && imageUrl !== '' && (
        <img
          alt={block.file_path !== undefined && block.file_path !== '' ? block.file_path : 'image'}
          src={imageUrl}
          className="block max-w-full max-h-56 mt-2 rounded"
        />
      )}
    </ToolBlock>
  );
}

function renderableImageUrl(imageUrl: string | undefined): string | undefined {
  if (imageUrl === undefined || imageUrl === '') return undefined;
  if (imageUrl.startsWith('data:image/')) return imageUrl;
  if (!imageUrl.startsWith('/')) return undefined;

  try {
    const resolved = new URL(imageUrl, document.baseURI);
    return resolved.origin === location.origin ? imageUrl : undefined;
  } catch {
    return undefined;
  }
}

/** `interaction` block: the single summary line from `formatInteraction`. Spec §9. */
export function InteractionBlock({
  block,
}: {
  block: Extract<SessionStructuredBlock, { type: 'interaction' }>;
}) {
  return <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto">{formatInteraction(block)}</pre>;
}

/** A streamed pending-interaction frame, rendered as its own message-shaped block. Spec §9. */
export function PendingInteractionView({ pending }: { pending: PendingInteraction }) {
  const rows = pendingRows(pending);
  return (
    <div className="space-y-2 border-b border-rule pb-3">
      <RoleLabel role="system" />
      <ToolBlock kind="interaction" label="pending" body={rows.join('\n')} />
    </div>
  );
}

/** Dispatch a single block to its renderer by `block.type`. Spec §5. */
export function StructuredBlock({ block }: { block: SessionStructuredBlock }) {
  switch (block.type) {
    case 'text':
      return (
        <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto text-fg">
          {block.text ?? ''}
        </pre>
      );
    case 'thinking':
      return (
        <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto text-fg-muted italic">
          {block.thinking !== undefined && block.thinking !== '' ? `[thinking] ${block.thinking}` : '[thinking]'}
        </pre>
      );
    case 'tool_use':
      return <ToolUseBlock block={block} />;
    case 'tool_result':
      return <ToolResultBlock block={block} />;
    case 'interaction':
      return <InteractionBlock block={block} />;
    case 'image':
      return <ImageBlock block={block} />;
    default:
      return (
        <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto text-fg-muted">
          {formatInlineValue(block)}
        </pre>
      );
  }
}

/**
 * A single structured message: a metadata header (role, provider, time, model,
 * usage, status, and stop_reason — in spec §1 order, empties omitted) followed
 * by the body. The body renders user-prompt and system-event
 * metadata first, then each block; the first `text` block is suppressed when
 * either metadata kind is present (spec §1.3), since the metadata already
 * structures that raw prompt/system text.
 */
export function StructuredMessage({ message }: { message: SessionStructuredMessage }) {
  const role = message.role;
  const assistantMetadata =
    message.role === 'assistant' || message.role === 'unknown' ? message : undefined;
  const userPrompt =
    message.role === 'user' || message.role === 'unknown' ? message.user_prompt : undefined;
  const systemEvent =
    message.role === 'system' || message.role === 'unknown' ? message.system_event : undefined;
  const usage = formatUsage(assistantMetadata?.usage);

  // Suppression gates on whether the metadata actually RENDERED (non-empty
  // rows), mirroring the old renderer that keyed off the returned element, not
  // mere field presence. When either metadata block renders, every `text` block
  // is dropped — the metadata already structures that raw prompt/system text.
  const promptRendered = userPrompt !== undefined && userPromptRows(userPrompt).length > 0;
  const systemRendered = systemEvent !== undefined && systemEventRows(systemEvent).length > 0;
  const suppressText = promptRendered || systemRendered;

  const blocks = message.blocks;

  return (
    <li className="space-y-2">
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1 pb-2 border-b border-rule">
        <RoleLabel role={role} />
        {message.provider !== undefined && message.provider !== '' && <HeaderMeta>{message.provider}</HeaderMeta>}
        <span className="text-body text-fg tnum" title={message.timestamp ?? undefined}>
          {formatClockTime(message.timestamp)}
        </span>
        {assistantMetadata?.model !== undefined && assistantMetadata.model !== '' && (
          <HeaderMeta>{assistantMetadata.model}</HeaderMeta>
        )}
        {usage !== '' && <HeaderMeta>{usage}</HeaderMeta>}
		<HeaderMeta>{message.status}</HeaderMeta>
        {assistantMetadata?.stop_reason !== undefined && assistantMetadata.stop_reason !== '' && (
          <HeaderMeta>{assistantMetadata.stop_reason}</HeaderMeta>
        )}
      </header>
      <div className="space-y-3">
        {userPrompt !== undefined && <UserPromptMetadata prompt={userPrompt} />}
        {systemEvent !== undefined && <SystemEventMetadata event={systemEvent} />}
        {blocks.map((block, index) => {
          if (suppressText && block.type === 'text') return null;
          return <StructuredBlock key={index} block={block} />;
        })}
      </div>
    </li>
  );
}

/**
 * Top-level structured transcript: the history envelope (when present) followed
 * by each structured message. Mirrors the old `loadTranscript` initial-load
 * order (history first, then messages). Spec §0.
 */
export function StructuredTranscript({
  history,
  messages,
}: {
  history?: SessionStructuredHistory;
  messages: SessionStructuredMessage[];
}) {
  return (
    <div className="space-y-5">
      {history !== undefined && <StructuredHistory history={history} />}
      <ol className="space-y-5">
        {messages.map((message, index) => (
          <StructuredMessage key={message.id !== '' ? message.id : index} message={message} />
        ))}
      </ol>
    </div>
  );
}
