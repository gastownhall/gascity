import type {
  AgentResponse,
  PendingInteraction,
  RespondSessionResponse,
  SessionRespondInputBody,
  SessionResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export interface AgentPendingInteraction {
  agentName: string;
  sessionId: string;
  sessionName: string;
  pending: PendingInteraction;
}

export async function listAgentPendingInteractions(
  agents: readonly AgentResponse[],
  sessions: readonly SessionResponse[],
): Promise<AgentPendingInteraction[]> {
  const cityName = activeCityOrThrow('list agent pending interactions');
  const sessionIdsByName = sessionIdByName(sessions);
  const candidates = agents.flatMap((agent) => {
    const sessionName = agent.session?.name;
    if (sessionName === undefined) return [];
    const sessionId = sessionIdsByName.get(sessionName);
    if (sessionId === undefined) return [];
    return [{ agentName: agent.name, sessionId, sessionName }];
  });

  const pending = await Promise.all(
    candidates.map(async (candidate) => {
      const response = await supervisorApi().sessionPending(cityName, candidate.sessionId);
      if (response.pending === undefined) return null;
      return { ...candidate, pending: response.pending };
    }),
  );
  return pending.filter((item): item is AgentPendingInteraction => item !== null);
}

export async function respondToAgentPendingInteraction(
  sessionId: string,
  body: SessionRespondInputBody,
): Promise<RespondSessionResponse> {
  const cityName = activeCityOrThrow('respond to agent pending interaction');
  return supervisorApi().respondSession(cityName, sessionId, body);
}

export function attachCommand(agentName: string): string {
  return `gc agent attach ${shellToken(agentName)}`;
}

function sessionIdByName(sessions: readonly SessionResponse[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const session of sessions) {
    if (session.session_name !== undefined) {
      out.set(session.session_name, session.id);
    }
  }
  return out;
}
function shellToken(value: string): string {
  if (/^[A-Za-z0-9_./:-]+$/.test(value)) return value;
  return `'${value.replaceAll("'", "'\\''")}'`;
}
