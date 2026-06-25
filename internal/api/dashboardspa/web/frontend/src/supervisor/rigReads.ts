import { activeCityOrThrow } from '../api/cityBase';
import type { ListBodyRigResponse, RigResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApi } from './client';

export type SupervisorRig = RigResponse;

export interface SupervisorRigList extends Omit<ListBodyRigResponse, 'items'> {
  items: SupervisorRig[];
}

export async function listSupervisorRigs(): Promise<SupervisorRigList> {
  const list = await supervisorApi().listRigs(activeCityOrThrow('list supervisor rigs'));
  return {
    ...list,
    items: list.items ?? [],
  };
}
