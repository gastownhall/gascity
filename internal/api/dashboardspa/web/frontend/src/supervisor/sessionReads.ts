import type {
  ListBodySessionResponse,
  OutputTurn,
  SessionResponse,
  SessionTranscriptConversationResponse,
  SessionTranscriptGetResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { isSessionStructuredEvent } from 'gas-city-dashboard-shared';
import type {
  DashboardSession,
  SessionStreamStructuredMessageEvent,
} from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export type SupervisorSession = SessionResponse;
export type SupervisorSessionList = ListBodySessionResponse;

export type SessionTranscriptView = Omit<SessionTranscriptConversationResponse, 'turns'> & {
  turns: OutputTurn[];
  total_chars: number;
  captured_at: string;
  truncated: boolean;
};

export async function listSupervisorSessions(): Promise<SupervisorSessionList> {
  return supervisorApi().listSessions(activeCityOrThrow('list supervisor sessions'));
}

export async function fetchSupervisorSessionTranscript(
  sessionId: string,
): Promise<SessionTranscriptView> {
  const transcript = await supervisorApi().sessionTranscript(
    activeCityOrThrow('fetch supervisor session transcript'),
    sessionId,
    'conversation',
  );
  return sessionTranscriptView(transcript);
}

/**
 * Fetch a session transcript as `format=structured` and narrow the generated
 * response union at the edge. Returns null when the server fell back to a
 * non-structured response (the caller then renders the conversation transcript
 * instead).
 */
export async function fetchStructuredTranscript(
  sessionId: string,
): Promise<SessionStreamStructuredMessageEvent | null> {
  const transcript = await supervisorApi().sessionTranscript(
    activeCityOrThrow('fetch structured session transcript'),
    sessionId,
    'structured',
  );
  return structuredTranscriptOrNull(transcript);
}

export function structuredTranscriptOrNull(
  transcript: SessionTranscriptGetResponse,
): SessionStreamStructuredMessageEvent | null {
  if (transcript.format !== 'structured') return null;
  if (!isSessionStructuredEvent(transcript)) {
    throw new Error('Malformed structured transcript response.');
  }
  return transcript;
}

export function normalizeSessions(list: ListBodySessionResponse): DashboardSession[] {
  return (list.items ?? []).map(normalizeSession);
}

function normalizeSession(session: SessionResponse): DashboardSession {
  const normalized: DashboardSession = {
    id: session.id,
    template: session.template,
    session_name: session.session_name,
    title: session.title,
    state: session.state,
    created_at: session.created_at,
    attached: session.attached,
    running: session.running,
    provider: session.provider,
  };
  if (session.alias !== undefined) normalized.alias = session.alias;
  if (session.reason !== undefined) normalized.reason = session.reason;
  if (session.display_name !== undefined) normalized.display_name = session.display_name;
  if (session.last_active !== undefined) normalized.last_active = session.last_active;
  if (session.rig !== undefined) normalized.rig = session.rig;
  if (session.pool !== undefined) normalized.pool = session.pool;
  if (session.agent_kind !== undefined) normalized.agent_kind = session.agent_kind;
  if (session.model !== undefined) normalized.model = session.model;
  if (session.context_pct !== undefined) normalized.context_pct = session.context_pct;
  if (session.context_window !== undefined) normalized.context_window = session.context_window;
  if (session.activity !== undefined) normalized.activity = session.activity;
  return normalized;
}

export function sessionTranscriptView(
  transcript: SessionTranscriptGetResponse,
  capturedAt: string = new Date().toISOString(),
): SessionTranscriptView {
  if (transcript.format !== 'conversation' && transcript.format !== 'text') {
    throw new Error(`expected conversation transcript, got ${transcript.format}`);
  }
  const turns = transcript.turns ?? [];
  return {
    ...transcript,
    turns,
    total_chars: turns.reduce((sum, turn) => sum + turn.text.length, 0),
    captured_at: capturedAt,
    truncated: false,
  };
}
