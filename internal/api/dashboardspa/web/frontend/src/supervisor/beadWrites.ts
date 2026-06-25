import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';
import type {
  Bead,
  BeadCreateInputBody,
  SlingInputBody,
  SlingResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';

export interface CreateAndSlingSupervisorBeadInput {
  title: string;
  description: string;
  rig: string;
  target: string;
}

export interface CreateAndSlingSupervisorBeadResult {
  bead: Bead;
  sling: SlingResponse;
}

export async function closeSupervisorBead(id: string, reason?: string): Promise<void> {
  const trimmedReason = reason?.trim() ?? '';
  await supervisorApi().closeBead(
    activeCityOrThrow('close supervisor bead'),
    id,
    trimmedReason.length === 0 ? undefined : { reason: trimmedReason },
  );
}

export async function nudgeSupervisorAgent(agentAlias: string): Promise<void> {
  const trimmedAlias = agentAlias.trim();
  if (trimmedAlias.length === 0) throw new Error('agent alias is required');
  await supervisorApi().nudgeAgent(activeCityOrThrow('nudge supervisor agent'), trimmedAlias);
}

export async function createAndSlingSupervisorBead(
  input: CreateAndSlingSupervisorBeadInput,
): Promise<CreateAndSlingSupervisorBeadResult> {
  const title = input.title.trim();
  const description = input.description.trim();
  const rig = input.rig.trim();
  const target = input.target.trim();

  if (title.length === 0) throw new Error('bead title is required');
  if (target.length === 0) throw new Error('sling target is required');

  const cityName = activeCityOrThrow('create and sling supervisor bead');
  const createBody: BeadCreateInputBody = { title };
  if (description.length > 0) createBody.description = description;

  const bead = await supervisorApi().createBead(cityName, createBody);
  const slingBody: SlingInputBody = { bead: bead.id, target };
  if (rig.length > 0) slingBody.rig = rig;
  const sling = await supervisorApi().sling(cityName, slingBody);

  return { bead, sling };
}
