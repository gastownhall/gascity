import { useEffect, useRef, useState } from 'react';
import {
  errorMessage,
  isSessionActivityEvent,
  isSessionHeartbeatEvent,
  isSessionStructuredEvent,
  parsePendingInteraction,
  structuredMessagesFromEnvelope,
} from 'gas-city-dashboard-shared';
import type {
  PendingInteraction,
  SessionStructuredHistory,
  SessionStructuredMessage,
} from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import { reportClientError } from '../lib/clientErrorReporting';
import { supervisorApi } from '../supervisor/client';
import { fetchStructuredTranscript } from '../supervisor/sessionReads';
import type { SessionStreamProgress } from './useSessionStream';

// Live structured-transcript reader, ported from the old dashboard's
// connectAgentOutput (PR #3718). It seeds from the REST structured snapshot,
// then consumes the five structured-mode SSE frames — structured, activity,
// pending, pending_cleared, heartbeat. Snapshot and reset frames replace the
// current projection; upserts merge by stable message ID so same-ID lifecycle
// updates replace their prior value. Raw conversation frames are rejected. A
// non-structured snapshot yields the `unavailable` state so the caller can fall
// back to conversation rendering.

/** One rendered item in arrival order: a structured message or a pending interaction. */
export type StructuredStreamItem =
  | { kind: 'message'; message: SessionStructuredMessage }
  | { kind: 'pending'; pending: PendingInteraction };

export interface StructuredTranscriptResult {
  provider: string;
  template: string;
  history: SessionStructuredHistory | null;
  items: StructuredStreamItem[];
  /** Latest tail activity (`idle` | `in-turn` | `unknown`). */
  activity: string;
}

export type StructuredStreamState =
  | { status: 'idle'; stream: SessionStreamProgress }
  | { status: 'loading'; stream: SessionStreamProgress }
  | { status: 'failed'; error: string; stream: SessionStreamProgress }
  | { status: 'unavailable'; stream: SessionStreamProgress }
  | { status: 'ready'; result: StructuredTranscriptResult; stream: SessionStreamProgress };

const STRUCTURED_FRAME_ERROR = 'Malformed structured session frame.';

export function useStructuredSessionStream(
  sessionId: string | null,
  stream: boolean,
): StructuredStreamState {
  const [state, setState] = useState<StructuredStreamState>({
    status: 'idle',
    stream: { status: 'idle' },
  });
  const malformedReportedRef = useRef(false);

  useEffect(() => {
    malformedReportedRef.current = false;
    if (!sessionId) {
      setState({ status: 'idle', stream: { status: 'idle' } });
      return;
    }
    let cancelled = false;
    let source: EventSource | null = null;
    const canStream = stream && typeof EventSource !== 'undefined';
    setState({ status: 'loading', stream: { status: canStream ? 'connecting' : 'idle' } });

    const degrade = (): void => {
      if (!malformedReportedRef.current) {
        malformedReportedRef.current = true;
        reportStructuredStreamError('parse structured frame', sessionId, STRUCTURED_FRAME_ERROR);
      }
      setState((current) =>
        current.status === 'ready'
          ? { ...current, stream: { status: 'degraded', error: STRUCTURED_FRAME_ERROR } }
          : current,
      );
    };

    const upsertPending = (pending: PendingInteraction): void => {
      setState((current) =>
        current.status === 'ready'
          ? {
              status: 'ready',
              result: {
                ...current.result,
                items: upsertPendingItem(current.result.items, pending),
              },
              stream: { status: 'open' },
            }
          : current,
      );
    };

    const messageItems = (messages: SessionStructuredMessage[]): StructuredStreamItem[] =>
      messages.map((message) => ({ kind: 'message' as const, message }));

    const applyStructuredEnvelope = (
      current: StructuredTranscriptResult,
      envelope: Parameters<typeof structuredMessagesFromEnvelope>[0],
    ): StructuredTranscriptResult => {
      const messages = structuredMessagesFromEnvelope(envelope);
      return {
        provider: envelope.provider,
        template: envelope.template,
        history: envelope.history,
        items:
          envelope.operation === 'upsert'
            ? mergeStructuredItems(current.items, messages)
            : replaceStructuredMessages(current.items, messages),
        activity: envelope.history.tail_state.activity,
      };
    };

    fetchStructuredTranscript(sessionId).then(
      (envelope) => {
        if (cancelled) return;
        if (envelope === null) {
          setState({ status: 'unavailable', stream: { status: 'idle' } });
          return;
        }
        setState({
          status: 'ready',
          result: {
            provider: envelope.provider,
            template: envelope.template,
            history: envelope.history,
            items: messageItems(structuredMessagesFromEnvelope(envelope)),
            activity: envelope.history.tail_state.activity,
          },
          stream: { status: canStream ? 'connecting' : 'idle' },
        });
        if (!canStream) return;

        source = new EventSource(
          supervisorApi().sessionStreamUrl(
            activeCityOrThrow('open structured session stream'),
            sessionId,
            envelope.history.cursor.resume_token,
            'structured',
          ),
          { withCredentials: true },
        );
        source.onopen = () => {
          if (cancelled) return;
          setState((current) =>
            current.status === 'ready'
              ? {
                  ...current,
                  result: {
                    ...current.result,
                    items: current.result.items.filter((item) => item.kind !== 'pending'),
                  },
                  stream: { status: 'open' },
                }
              : current,
          );
        };
        source.addEventListener('structured', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionStructuredEvent(parsed)) return degrade();
          setState((current) =>
            current.status === 'ready'
              ? {
                  status: 'ready',
                  result: applyStructuredEnvelope(current.result, parsed),
                  stream: { status: 'open' },
                }
              : current,
          );
        });
        source.addEventListener('activity', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionActivityEvent(parsed)) return degrade();
          const activity = parsed.activity;
          setState((current) =>
            current.status === 'ready'
              ? {
                  status: 'ready',
                  result: { ...current.result, activity },
                  stream: { status: 'open' },
                }
              : current,
          );
        });
        source.addEventListener('pending', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          const pending = parsed === null ? null : parsePendingInteraction(parsed);
          if (pending === null) return degrade();
          upsertPending(pending);
        });
        source.addEventListener('pending_cleared', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          const requestID = pendingClearedRequestID(parsed);
          if (requestID === null) return degrade();
          setState((current) =>
            current.status === 'ready'
              ? {
                  status: 'ready',
                  result: {
                    ...current.result,
                    items: current.result.items.filter(
                      (item) => item.kind !== 'pending' || item.pending.request_id !== requestID,
                    ),
                  },
                  stream: { status: 'open' },
                }
              : current,
          );
        });
        source.addEventListener('heartbeat', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionHeartbeatEvent(parsed)) return degrade();
          // Liveness only: mark the stream open, leave the transcript untouched.
          setState((current) =>
            current.status === 'ready' &&
            (current.stream.status === 'connecting' || current.stream.status === 'closed')
              ? { ...current, stream: { status: 'open' } }
              : current,
          );
        });
        // Raw / unnamed `message` frames are not valid on a structured stream.
        source.onmessage = () => {
          if (cancelled) return;
          degrade();
        };
        source.onerror = () => {
          if (cancelled) return;
          const streamState = source?.readyState === EventSource.CLOSED ? 'closed' : 'connecting';
          setState((current) =>
            current.status === 'ready' ? { ...current, stream: { status: streamState } } : current,
          );
        };
      },
      (err: unknown) => {
        if (cancelled) return;
        reportStructuredStreamError('load structured transcript', sessionId, err);
        setState({
          status: 'failed',
          error: errorMessage(err) || 'Failed to load session.',
          stream: { status: 'idle' },
        });
      },
    );

    return () => {
      cancelled = true;
      source?.close();
    };
  }, [sessionId, stream]);

  return state;
}

function mergeStructuredItems(
  current: StructuredStreamItem[],
  incomingMessages: SessionStructuredMessage[],
): StructuredStreamItem[] {
  const incomingByID = new Map(incomingMessages.map((message) => [message.id, message]));
  const existingMessageIDs = new Set<string>();
  const merged = current.map((item) => {
    if (item.kind === 'pending') return item;
    existingMessageIDs.add(item.message.id);
    const replacement = incomingByID.get(item.message.id);
    return replacement === undefined ? item : { kind: 'message' as const, message: replacement };
  });
  for (const message of incomingMessages) {
    if (!existingMessageIDs.has(message.id)) {
      merged.push({ kind: 'message', message: incomingByID.get(message.id) ?? message });
      existingMessageIDs.add(message.id);
    }
  }
  return merged;
}

function replaceStructuredMessages(
  current: StructuredStreamItem[],
  messages: SessionStructuredMessage[],
): StructuredStreamItem[] {
  return [
    ...messages.map((message) => ({ kind: 'message' as const, message })),
    ...current.filter((item) => item.kind === 'pending'),
  ];
}

function upsertPendingItem(
  current: StructuredStreamItem[],
  pending: PendingInteraction,
): StructuredStreamItem[] {
  return [...current.filter((item) => item.kind !== 'pending'), { kind: 'pending', pending }];
}

function parseFrame(data: string): unknown {
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return null;
  }
}

function pendingClearedRequestID(data: unknown): string | null {
  if (typeof data !== 'object' || data === null || Array.isArray(data)) return null;
  const requestID = (data as Record<string, unknown>).request_id;
  return typeof requestID === 'string' && requestID !== '' ? requestID : null;
}

function reportStructuredStreamError(operation: string, sessionId: string, err: unknown): void {
  void reportClientError({
    component: 'structured-session-stream',
    operation,
    message: `${sessionId}: ${errorMessage(err)}`,
  });
}
