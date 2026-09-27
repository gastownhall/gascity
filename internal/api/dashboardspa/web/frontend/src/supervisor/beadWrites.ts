import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';
import type {
  Bead,
  BeadCreateInputBody,
  BeadUpdateBody,
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

export async function closeSupervisorBead(id: string): Promise<void> {
  await supervisorApi().closeBead(activeCityOrThrow('close supervisor bead'), id);
}

export interface UpdateSupervisorBeadPatch {
  status?: string;
  priority?: number;
}

// updateSupervisorBead changes only the fields present in the patch, through the
// typed supervisor bead-update endpoint. The Workbench views use it for lane
// movement (Kanban -> status, Priority -> priority) and for the confirmed close
// (status='closed'). It never creates an Execution Attempt or Session: those
// are Gas City's responsibility, not the view's.
export async function updateSupervisorBead(
  id: string,
  patch: UpdateSupervisorBeadPatch,
): Promise<void> {
  const body: BeadUpdateBody = {};
  if (patch.status !== undefined) body.status = patch.status;
  if (patch.priority !== undefined) body.priority = patch.priority;
  if (Object.keys(body).length === 0) return;
  await supervisorApi().updateBead(activeCityOrThrow('update supervisor bead'), id, body);
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

// startSupervisorAttempt delegates lifecycle creation to Gas City: slinging the
// Bead creates a NEW Execution Attempt (Session + worktree). It never revives a
// completed or failed Session in place, and repeated calls are the caller's to
// dedupe (the UI disables while in flight).
export async function startSupervisorAttempt(beadId: string, target: string): Promise<void> {
  const trimmed = target.trim();
  if (trimmed.length === 0) throw new Error('a sling target is required to start an attempt');
  const cityName = activeCityOrThrow('start supervisor attempt');
  const body: SlingInputBody = { bead: beadId, target: trimmed };
  await supervisorApi().sling(cityName, body);
}
