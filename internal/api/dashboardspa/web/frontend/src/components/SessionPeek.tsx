import { AnsiUp } from 'ansi_up';
import { useMemo, type ReactNode } from 'react';
import type { OutputTurn } from 'gas-city-dashboard-shared/gc-supervisor';
import { formatClockTime, formatRelative, formatShortDate } from '../hooks/time';
import { PROMPT_INJECTION_NOTICE } from '../lib/constants';
import { stripTerminalControls } from '../lib/stripTerminalControls';
import type { SessionTranscriptView } from '../supervisor/sessionReads';
import { TranscriptBox } from './TranscriptBox';

// Render layer for a session's transcript snapshot. Used by:
//   - Agents page peek modal (one-shot fetch)
//   - Agent drilldown live peek panel (auto-refreshing)
//   - Formula run node session panel (snapshot plus active SSE turns)
//
// Pure presentation — fetch + cadence decisions belong to the caller.

// Bounded leading window for timestamp extraction. The gc prompt template
// embeds an ISO timestamp in the first ~120 chars; large turn bodies past
// this are treated as content, not metadata.
const TIMESTAMP_SEARCH_WINDOW = 512;

// ISO 8601 with required seconds component, optional fractional seconds,
// optional zone (Z or ±HH:MM). Requires the `T` separator so a bare date
// like `2026-05-20` doesn't false-match. The trailing `(?!\d)` (rather
// than `\b`) prevents the optional offset group from silently truncating
// when followed by a word character: `+09:00X` is preserved as part of
// the match rather than backtracked-away.
const ISO_TIMESTAMP_RE =
  /\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?(?!\d)/;

/**
 * Extract the first ISO 8601 datetime from the leading slice of a turn's
 * text. Returns `null` if no parseable timestamp is found in the search
 * window — most turns (assistant output, tool results) won't have one.
 *
 * Caveat: timestamps without a timezone designator (e.g. `2026-05-20T10:53:10`
 * with no `Z` or `±HH:MM`) are parsed by `Date.parse` as LOCAL time. For
 * gascity-dashboard's single-operator same-host deployment this is fine —
 * gc and the browser share a clock. Cross-timezone access would shift
 * relative displays; if that ever becomes a concern the gc supervisor
 * should be patched to emit explicit zone-suffixed timestamps.
 *
 * Exported for tests.
 */
export function extractTurnTimestamp(text: string): string | null {
  if (text.length === 0) return null;
  const head =
    text.length > TIMESTAMP_SEARCH_WINDOW ? text.slice(0, TIMESTAMP_SEARCH_WINDOW) : text;
  const match = head.match(ISO_TIMESTAMP_RE);
  return match ? match[0] : null;
}

interface SessionPeekContentProps {
  loading: boolean;
  error: string | null;
  result: SessionTranscriptView | null;
  /** Caption text shown on the same row as the expand button. */
  caption?: string;
}

export function SessionPeekContent({ loading, error, result, caption }: SessionPeekContentProps) {
  if (loading && result === null) {
    return <p className="text-fg-muted italic">Fetching transcript.</p>;
  }
  if (error) {
    return (
      <p className="text-accent" role="alert">
        {error}
      </p>
    );
  }
  if (!result) return null;
  if (result.turns.length === 0) {
    return <p className="text-fg-muted italic">No turns in this session yet.</p>;
  }

  // One `now` per render keeps every turn's relative timestamp consistent
  // with every other on this paint. The modal doesn't auto-tick; relative
  // values refresh on the next fetch / rerender.
  const now = Date.now();

  return (
    <TranscriptBox {...(caption !== undefined ? { caption } : {})}>
      <div className="space-y-6">
        {/* When a caption is shown it already carries "captured X ago", so the
            standalone captured-date line would be a duplicate timestamp; render
            it only when there is no caption (e.g. the run-node panel). */}
        {caption === undefined && (
          <p
            className="text-label uppercase tracking-wider text-fg-faint tnum"
            title={result.captured_at}
          >
            {formatShortDate(result.captured_at)}
          </p>
        )}
        <p className="text-label uppercase tracking-wider text-warn">▲ {PROMPT_INJECTION_NOTICE}</p>
        <ol className="space-y-5">
          {result.turns.map((turn, idx) => (
            <TurnBlock key={idx} turn={turn} index={idx} now={now} />
          ))}
        </ol>
        {result.truncated && (
          <p className="text-label uppercase tracking-wider text-fg-faint italic">
            Some turns truncated at the per-turn or total cap. Run{' '}
            <code className="text-fg-muted">gc session peek</code> in a terminal for the full
            transcript.
          </p>
        )}
      </div>
    </TranscriptBox>
  );
}

function TurnBlock({ turn, index, now }: { turn: OutputTurn; index: number; now: number }) {
  const renderedText = useMemo(() => ansiToReactNodes(turn.text), [turn.text]);

  const timestamp = useMemo(() => extractTurnTimestamp(turn.text), [turn.text]);

  return (
    <li>
      <header className="flex items-start justify-between gap-3 pb-2 border-b border-rule mb-2">
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          #{(index + 1).toString().padStart(2, '0')}
        </span>
        <div className="flex flex-col items-end leading-tight" title={timestamp ?? undefined}>
          <span className="text-body text-fg tnum">{formatClockTime(timestamp)}</span>
          <span className="text-label uppercase tracking-wider text-fg-faint tnum">
            {formatRelative(timestamp, now)}
          </span>
          <RoleLabel role={turn.role} />
        </div>
      </header>
      <pre className="text-body whitespace-pre-wrap leading-relaxed overflow-x-auto text-fg">
        {renderedText}
      </pre>
    </li>
  );
}

function ansiToReactNodes(text: string): ReactNode[] {
  // Strip OSC / non-SGR CSI / lone-ESC / bare C1 control bytes before
  // ansi_up runs. ansi_up colorizes SGR but passes every other control
  // sequence through as visible text, leaking `^[`, `\x9c`, OSC titles, etc.
  // into the peek (gascity-dashboard-5e5v / xl07). SGR is preserved here so
  // ansi_up can still render colour.
  const cleaned = stripTerminalControls(text);
  const renderer = new AnsiUp();
  renderer.use_classes = true;
  const html = renderer.ansi_to_html(cleaned);
  if (typeof DOMParser === 'undefined') return [cleaned];

  const doc = new DOMParser().parseFromString(`<body>${html}</body>`, 'text/html');
  return Array.from(doc.body.childNodes).map((node, index) => htmlNodeToReact(node, String(index)));
}

function htmlNodeToReact(node: ChildNode, key: string): ReactNode {
  if (node.nodeType === 3) return node.textContent ?? '';
  if (node.nodeType !== 1) return null;

  const element = node as Element;
  const children = Array.from(element.childNodes).map((child, index) =>
    htmlNodeToReact(child, `${key}-${index}`),
  );

  if (element.tagName.toLowerCase() === 'br') {
    return <br key={key} />;
  }
  if (element.tagName.toLowerCase() !== 'span') {
    return <span key={key}>{children}</span>;
  }

  return (
    <span key={key} className={element.getAttribute('class') ?? undefined}>
      {children}
    </span>
  );
}

function RoleLabel({ role }: { role: string }) {
  const tone = roleTone(role);
  return (
    <span className={`text-label uppercase tracking-wider font-medium ${tone}`}>
      {role.replace(/_/g, ' ')}
    </span>
  );
}

function roleTone(role: string): string {
  switch (role) {
    case 'assistant':
      return 'text-accent';
    case 'user':
      return 'text-fg';
    case 'system':
      return 'text-warn';
    case 'tool_use':
    case 'tool_result':
      return 'text-fg-muted';
    default:
      return 'text-fg-faint';
  }
}
