import { activeCityOrThrow } from '../api/cityBase';
import type { AgentResponse, ListBodyAgentResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApi } from './client';

export type SupervisorAgent = AgentResponse;

export interface SupervisorAgentList extends Omit<ListBodyAgentResponse, 'items'> {
  items: SupervisorAgent[];
}

export async function listSupervisorAgents(): Promise<SupervisorAgentList> {
  const list = await supervisorApi().listAgents(activeCityOrThrow('list supervisor agents'));
  return {
    ...list,
    items: list.items ?? [],
  };
}
