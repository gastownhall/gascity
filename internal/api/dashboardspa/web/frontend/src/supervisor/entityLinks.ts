import type { EntityLinkView, DashboardBead, DashboardSession } from 'gas-city-dashboard-shared';
import { buildLinkView, buildRelationIndex, parseRef } from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApi } from './client';
import { listIsIncomplete } from './listPartial';
import { normalizeSessions, type SupervisorSessionList } from './sessionReads';

// Pre-exposure load bound (gascity-dashboard-q89b): this fetch runs once per
// focus ref per client, so the limit multiplies across viewers. Truncation is
// safe: the partial flag below trips when upstream total exceeds what was
// fetched, and RelatedEntities renders the partial notice.
const LINKS_FETCH_LIMIT = 1_000;

export async function loadSupervisorEntityLinks(ref: string): Promise<EntityLinkView> {
  const parsed = parseRef(ref);
  if (!parsed.ok) throw new Error(parsed.error);

  const cityName = activeCityOrThrow('load supervisor entity links');
  const supervisorFetchedAt = new Date().toISOString();
  const beadList = await supervisorApi().listBeads(cityName, { limit: LINKS_FETCH_LIMIT });
  const beads = normalizeBeads(beadList.items ?? []);
  let partial = listIsIncomplete(beadList, beads.length);

  let sessions: DashboardSession[] = [];
  try {
    const sessionList = await supervisorApi().listSessions(cityName);
    sessions = normalizeSessions(sessionList);
    partial ||= sessionListIsPartial(sessionList);
  } catch {
    partial = true;
  }

  const index = buildRelationIndex(beads, sessions, cityName);
  return buildLinkView(index, parsed, {
    partial,
    supervisorFetchedAt,
    githubFetchedAt: null,
  });
}

function normalizeBeads(beads: readonly Bead[]): DashboardBead[] {
  return beads.map(normalizeBead);
}

function normalizeBead(bead: Bead): DashboardBead {
  const normalized: DashboardBead = {
    id: bead.id,
    title: bead.title,
    status: bead.status,
    issue_type: bead.issue_type,
    priority: bead.priority ?? null,
    created_at: bead.created_at,
  };
  if (bead.description !== undefined) normalized.description = bead.description;
  if (bead.assignee !== undefined) normalized.assignee = bead.assignee;
  if (Array.isArray(bead.labels)) normalized.labels = bead.labels;
  if (bead.metadata !== undefined) normalized.metadata = bead.metadata;
  if (bead.ref !== undefined) normalized.ref = bead.ref;
  if (bead.parent !== undefined) normalized.parent = bead.parent;
  if (bead.from !== undefined) normalized.from = bead.from;
  if (bead.ephemeral !== undefined) normalized.ephemeral = bead.ephemeral;
  if (bead.needs !== undefined) normalized.needs = bead.needs;
  if (bead.dependencies !== undefined) normalized.dependencies = bead.dependencies;
  if (bead.updated_at !== undefined) normalized.updated_at = bead.updated_at;
  return normalized;
}

function sessionListIsPartial(list: SupervisorSessionList): boolean {
  return list.partial === true || (list.partial_errors?.length ?? 0) > 0;
}
