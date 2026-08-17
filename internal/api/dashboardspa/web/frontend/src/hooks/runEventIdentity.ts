import type { GcEventEnvelope } from './useGcEvents';

export interface FormulaRunIdentity {
  runId: string;
  rootBeadId: string;
}

export interface RunEventIdentity {
  runIds: Set<string>;
  rootBeadIds: Set<string>;
}

export function formulaRunDetailEventMatches(
  identity: RunEventIdentity,
  detail: FormulaRunIdentity,
): boolean {
  const runMatches = identity.runIds.size === 0 || identity.runIds.has(detail.runId);
  const rootMatches =
    identity.rootBeadIds.size === 0 || identity.rootBeadIds.has(detail.rootBeadId);
  return runMatches && rootMatches;
}

export function runEventIdentity(event: GcEventEnvelope): RunEventIdentity {
  const identity: RunEventIdentity = {
    runIds: new Set<string>(),
    rootBeadIds: new Set<string>(),
  };
  collectRunIdentity(event, identity);
  collectRunIdentity(recordValue(event.run), identity);
  collectRunIdentity(recordValue(event.payload), identity);
  collectRunIdentity(recordValue(recordValue(event.payload)?.run), identity);
  collectRunIdentity(recordValue(event.bead), identity);
  collectRunIdentity(recordValue(recordValue(event.payload)?.bead), identity);
  collectRunIdentity(recordValue(event.root), identity);
  collectRunIdentity(recordValue(recordValue(event.payload)?.root), identity);
  collectMetadataIdentity(recordValue(event.metadata), identity);
  collectMetadataIdentity(recordValue(recordValue(event.payload)?.metadata), identity);
  return identity;
}

function collectRunIdentity(
  value: Record<string, unknown> | undefined,
  identity: RunEventIdentity,
): void {
  if (!value) return;
  addString(identity.runIds, value.run_id);
  addString(identity.runIds, value.workflow_id);
  addString(identity.rootBeadIds, value.root_bead_id);
  collectMetadataIdentity(recordValue(value.metadata), identity);
}

function collectMetadataIdentity(
  metadata: Record<string, unknown> | undefined,
  identity: RunEventIdentity,
): void {
  if (!metadata) return;
  addString(identity.runIds, metadata['gc.run_id']);
  addString(identity.runIds, metadata['gc.workflow_id']);
  addString(identity.runIds, metadata.run_id);
  addString(identity.runIds, metadata.workflow_id);
  addString(identity.rootBeadIds, metadata['gc.root_bead_id']);
  addString(identity.rootBeadIds, metadata.root_bead_id);
}

function addString(target: Set<string>, value: unknown): void {
  if (typeof value !== 'string') return;
  const clean = value.trim();
  if (clean) target.add(clean);
}

function recordValue(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}
