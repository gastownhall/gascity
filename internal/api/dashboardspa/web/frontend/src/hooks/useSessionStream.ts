import { useEffect, useRef, useState } from 'react';
import type { MutableRefObject } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';
import { getActiveCity } from '../api/cityBase';
import { reportClientError } from '../lib/clientErrorReporting';
import type { OutputTurn } from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApi } from '../supervisor/client';
import {
  fetchSupervisorSessionTranscript,
  sessionTranscriptView,
  type SessionTranscriptView,
} from '../supervisor/sessionReads';

export type SessionStreamProgress =
  | { status: 'idle' }
  | { status: 'connecting' }
  | { status: 'open' }
  | { status: 'closed' }
  | { status: 'degraded'; error: string };

export type SessionStreamConnState = SessionStreamProgress['status'];

export type SessionStreamState =
  | { status: 'idle'; stream: { status: 'idle' } }
  | { status: 'loading'; stream: { status: 'idle' } | { status: 'connecting' } }
  | { status: 'failed'; error: string; stream: { status: 'idle' } }
  | { status: 'ready'; result: SessionTranscriptView; stream: SessionStreamProgress };

export function useSessionStream(sessionId: string | null, stream: boolean): SessionStreamState {
  const [state, setState] = useState<SessionStreamState>({
    status: 'idle',
    stream: { status: 'idle' },
  });
  const malformedEventReportedRef = useRef(false);

  useEffect(() => {
    malformedEventReportedRef.current = false;
    if (!sessionId) {
      setState({ status: 'idle', stream: { status: 'idle' } });
      return;
    }
    let cancelled = false;
    let source: EventSource | null = null;
    const canStream = stream && typeof EventSource !== 'undefined';
    setState({
      status: 'loading',
      stream: { status: canStream ? 'connecting' : 'idle' },
    });

    fetchSupervisorSessionTranscript(sessionId).then(
      (result) => {
        if (cancelled) return;
        setState({
          status: 'ready',
          result,
          stream: { status: canStream ? 'connecting' : 'idle' },
        });
        if (canStream) {
          source = new EventSource(
            supervisorApi().sessionStreamUrl(
              activeCityOrThrow('open supervisor session stream'),
              sessionId,
            ),
            {
              withCredentials: true,
            },
          );
          source.onopen = () => {
            if (cancelled) return;
            setState((current) =>
              current.status === 'ready' ? { ...current, stream: { status: 'open' } } : current,
            );
          };
          const onTurn = (event: MessageEvent<string>) => {
            if (cancelled) return;
            const payload = parseStreamPayload(event.data);
            if (payload.kind === 'invalid') {
              reportMalformedSessionEvent(sessionId, malformedEventReportedRef);
            }
            setState((current) => {
              const base = current.status === 'ready' ? current.result : result;
              if (payload.kind === 'invalid') {
                return {
                  status: 'ready',
                  result: base,
                  stream: { status: 'degraded', error: payload.error },
                };
              }
              if (payload.kind === 'snapshot') {
                return {
                  status: 'ready',
                  result: payload.result,
                  stream: { status: 'open' },
                };
              }
              return {
                status: 'ready',
                result: {
                  ...base,
                  turns: [...base.turns, payload.turn],
                  total_chars: base.total_chars + payload.turn.text.length,
                  captured_at: new Date().toISOString(),
                },
                stream: { status: 'open' },
              };
            });
          };
          source.onmessage = onTurn;
          source.addEventListener('turn', onTurn);
          source.onerror = () => {
            if (cancelled) return;
            const streamState = source?.readyState === EventSource.CLOSED ? 'closed' : 'connecting';
            setState((current) =>
              current.status === 'ready'
                ? { ...current, stream: { status: streamState } }
                : current,
            );
          };
        }
      },
      (err: unknown) => {
        if (!cancelled) {
          reportSessionStreamError('load transcript', sessionId, err);
          setState({
            status: 'failed',
            error: errorMessage(err) || 'Failed to load session.',
            stream: { status: 'idle' },
          });
        }
      },
    );

    return () => {
      cancelled = true;
      source?.close();
    };
  }, [sessionId, stream]);

  return state;
}

function reportMalformedSessionEvent(
  sessionId: string,
  reportedRef: MutableRefObject<boolean>,
): void {
  if (reportedRef.current) return;
  reportedRef.current = true;
  reportSessionStreamError('parse stream event', sessionId, MALFORMED_SESSION_STREAM_EVENT);
}

function reportSessionStreamError(operation: string, sessionId: string, err: unknown): void {
  void reportClientError({
    component: 'session-stream',
    operation,
    message: `${sessionId}: ${errorMessage(err)}`,
  });
}

function activeCityOrThrow(operation: string): string {
  const cityName = getActiveCity();
  if (cityName === null) {
    throw new Error(`${operation} called before an active city was resolved`);
  }
  return cityName;
}

type SessionStreamPayload =
  | { kind: 'turn'; turn: OutputTurn }
  | { kind: 'snapshot'; result: SessionTranscriptView }
  | { kind: 'invalid'; error: string };

const MALFORMED_SESSION_STREAM_EVENT = 'Malformed session stream event.';

function parseStreamPayload(data: string): SessionStreamPayload {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return { kind: 'invalid', error: MALFORMED_SESSION_STREAM_EVENT };
  }
  if (!isRecord(parsed)) return { kind: 'invalid', error: MALFORMED_SESSION_STREAM_EVENT };
  const snapshot = parseTranscriptSnapshot(parsed);
  if (snapshot) return { kind: 'snapshot', result: snapshot };
  if (typeof parsed.text !== 'string') {
    return { kind: 'invalid', error: MALFORMED_SESSION_STREAM_EVENT };
  }
  return {
    kind: 'turn',
    turn: {
      role: typeof parsed.role === 'string' ? parsed.role : 'assistant',
      text: parsed.text,
    },
  };
}

function parseTranscriptSnapshot(value: Record<string, unknown>): SessionTranscriptView | null {
  if (!Array.isArray(value.turns)) return null;
  const turns = value.turns.flatMap((turn): OutputTurn[] => {
    if (!isRecord(turn) || typeof turn.text !== 'string') return [];
    return [
      {
        role: typeof turn.role === 'string' ? turn.role : 'assistant',
        text: turn.text,
      },
    ];
  });
  if (turns.length !== value.turns.length) return null;
  const sessionId =
    typeof value.session_id === 'string'
      ? value.session_id
      : typeof value.id === 'string'
        ? value.id
        : '';
  if (!sessionId) return null;
  const totalChars =
    typeof value.total_chars === 'number'
      ? value.total_chars
      : turns.reduce((sum, turn) => sum + turn.text.length, 0);
  const result = sessionTranscriptView(
    {
      id: sessionId,
      template: typeof value.template === 'string' ? value.template : '',
      provider: typeof value.provider === 'string' ? value.provider : '',
      format: value.format === 'text' ? 'text' : 'conversation',
      turns,
    },
    typeof value.captured_at === 'string' ? value.captured_at : new Date().toISOString(),
  );
  return {
    ...result,
    total_chars: totalChars,
    truncated: value.truncated === true,
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
