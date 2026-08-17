import type { DashboardBead } from '../dashboard-beads.js';
import type { DashboardSession } from '../dashboard-sessions.js';

function metaString(bead: DashboardBead, key: string): string | undefined {
  const value = bead.metadata?.[key];
  if (typeof value === 'string') {
    const trimmed = value.trim();
    return trimmed.length > 0 ? trimmed : undefined;
  }
  if (typeof value === 'number' && Number.isFinite(value)) return String(value);
  return undefined;
}

function metaNumber(bead: DashboardBead, key: string): number | undefined {
  const raw = bead.metadata?.[key];
  if (typeof raw === 'number' && Number.isInteger(raw) && raw >= 0) return raw;
  if (typeof raw === 'string' && /^\d+$/.test(raw.trim())) {
    return Number.parseInt(raw.trim(), 10);
  }
  return undefined;
}

export interface IndexBead {
  id: string;
  title: string;
  status: string;
  scope: string;
  parentBeadId?: string;
  rootBeadId?: string;
  moleculeId?: string;
  prNumber?: string;
  prUrl?: string;
  issueNumber?: string;
  issueUrl?: string;
  sessionId?: string;
  sessionName?: string;
  stepId?: string;
  attempt?: number;
  superseded: boolean;
}

export interface RelationIndex {
  beads: Map<string, IndexBead>;
  allBeads: Map<string, IndexBead>;
  childrenOf: Map<string, string[]>;
  membersOfMolecule: Map<string, string[]>;
  beadsForPr: Map<string, string[]>;
  beadsForIssue: Map<string, string[]>;
  beadsForSession: Map<string, string[]>;
  sessions: Map<string, DashboardSession>;
}

const SCOPE_REF_KEYS = ['gc.scope_ref', 'scope_ref', 'scope_id'] as const;
const SCOPE_KIND_KEYS = ['gc.scope_kind', 'scope_kind'] as const;

function beadScope(bead: DashboardBead, cityName: string): string {
  let scopeRef: string | undefined;
  for (const key of SCOPE_REF_KEYS) {
    const value = metaString(bead, key);
    if (value !== undefined) {
      scopeRef = value;
      break;
    }
  }
  let scopeKind: string | undefined;
  for (const key of SCOPE_KIND_KEYS) {
    const value = metaString(bead, key);
    if (value !== undefined) {
      scopeKind = value;
      break;
    }
  }
  if (scopeRef === undefined) return `city:${cityName}`;
  return `${scopeKind ?? 'rig'}:${scopeRef}`;
}

const GITHUB_PR_ARTIFACT = /^github-pr:[^/]+\/[^/]+\/(\d+)$/;
const GITHUB_PR_URL_NUMBER = /\/(?:pull\/)?(\d+)(?:[/?#]|$)/;

function resolvePrRef(bead: DashboardBead): { prNumber?: string; prUrl?: string } {
  const evidenceUrl = metaString(bead, 'evidence.pr_url');
  const evidenceNumber = metaString(bead, 'evidence.pr_number');
  const artifactPath = metaString(bead, 'evidence.artifact_path');
  const reviewNumber = metaString(bead, 'pr_review.pr_number');
  const reviewUrl = metaString(bead, 'pr_review.pr_url');

  const artifactMatch = artifactPath?.match(GITHUB_PR_ARTIFACT);
  const urlNumberMatch = evidenceUrl?.match(GITHUB_PR_URL_NUMBER);

  const prNumber =
    evidenceNumber ?? artifactMatch?.[1] ?? urlNumberMatch?.[1] ?? reviewNumber ?? undefined;
  const prUrl = evidenceUrl ?? reviewUrl ?? undefined;

  const ref: { prNumber?: string; prUrl?: string } = {};
  if (prNumber !== undefined) ref.prNumber = prNumber;
  if (prUrl !== undefined) ref.prUrl = prUrl;
  return ref;
}

function toIndexBead(bead: DashboardBead, cityName: string): IndexBead {
  const { prNumber, prUrl } = resolvePrRef(bead);
  const indexBead: IndexBead = {
    id: bead.id,
    title: bead.title,
    status: bead.status,
    scope: beadScope(bead, cityName),
    superseded: false,
  };

  const optionalFields = {
    parentBeadId: metaString(bead, 'gc.parent_bead_id'),
    rootBeadId: metaString(bead, 'gc.root_bead_id'),
    moleculeId: metaString(bead, 'molecule_id'),
    prNumber,
    prUrl,
    issueNumber:
      metaString(bead, 'bugflow.github_issue_number') ??
      metaString(bead, 'design_review.github_issue_number'),
    issueUrl:
      metaString(bead, 'bugflow.github_issue_url') ??
      metaString(bead, 'design_review.github_issue_url'),
    sessionId: metaString(bead, 'session_id'),
    sessionName: metaString(bead, 'session_name'),
    stepId: metaString(bead, 'gc.step_id'),
    attempt: metaNumber(bead, 'gc.attempt'),
  };

  for (const [key, value] of Object.entries(optionalFields)) {
    if (value !== undefined) {
      Object.assign(indexBead, { [key]: value });
    }
  }

  return indexBead;
}

function retryKey(bead: IndexBead): string {
  return `${bead.moleculeId}\u0000${bead.stepId}`;
}

function markSuperseded(beads: IndexBead[]): void {
  const maxAttempt = new Map<string, number>();
  for (const bead of beads) {
    if (bead.moleculeId === undefined || bead.stepId === undefined || bead.attempt === undefined) {
      continue;
    }
    const key = retryKey(bead);
    const current = maxAttempt.get(key);
    if (current === undefined || bead.attempt > current) {
      maxAttempt.set(key, bead.attempt);
    }
  }
  for (const bead of beads) {
    if (bead.moleculeId === undefined || bead.stepId === undefined || bead.attempt === undefined) {
      continue;
    }
    const top = maxAttempt.get(retryKey(bead));
    if (top !== undefined && bead.attempt < top) bead.superseded = true;
  }
}

function push(map: Map<string, string[]>, key: string, value: string): void {
  const existing = map.get(key);
  if (existing) existing.push(value);
  else map.set(key, [value]);
}

export function buildRelationIndex(
  beads: readonly DashboardBead[],
  sessions: readonly DashboardSession[],
  cityName: string,
): RelationIndex {
  const indexBeads = beads.map((b) => toIndexBead(b, cityName));
  markSuperseded(indexBeads);

  const allBeads = new Map<string, IndexBead>();
  const live = new Map<string, IndexBead>();
  const childrenOf = new Map<string, string[]>();
  const membersOfMolecule = new Map<string, string[]>();
  const beadsForPr = new Map<string, string[]>();
  const beadsForIssue = new Map<string, string[]>();
  const beadsForSession = new Map<string, string[]>();

  for (const bead of indexBeads) {
    allBeads.set(bead.id, bead);
    if (bead.superseded) continue;
    live.set(bead.id, bead);

    if (bead.parentBeadId) push(childrenOf, bead.parentBeadId, bead.id);
    if (bead.moleculeId) push(membersOfMolecule, bead.moleculeId, bead.id);
    if (bead.prNumber) push(beadsForPr, bead.prNumber, bead.id);
    if (bead.issueNumber) push(beadsForIssue, bead.issueNumber, bead.id);
    if (bead.sessionId) push(beadsForSession, bead.sessionId, bead.id);
  }

  const sessionMap = new Map<string, DashboardSession>();
  for (const session of sessions) sessionMap.set(session.id, session);

  return {
    beads: live,
    allBeads,
    childrenOf,
    membersOfMolecule,
    beadsForPr,
    beadsForIssue,
    beadsForSession,
    sessions: sessionMap,
  };
}
