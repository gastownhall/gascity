import type { RunSnapshotBead } from '../run-snapshot.js';
import type { RunConstructKind } from '../run-detail.js';
import { externalizeId, meta, nonEmpty, normalizedStepRef } from './bead-fields.js';

const HIDDEN_CONSTRUCTS = new Set<RunConstructKind>(['scope-check', 'run-finalize', 'spec']);

export function isHiddenConstruct(kind: RunConstructKind): boolean {
  return HIDDEN_CONSTRUCTS.has(kind) || kind === 'control';
}

export function semanticNodeIdFor(bead: RunSnapshotBead, rootBeadId: string): string {
  const beadId = nonEmpty(bead.id);
  if (beadId && beadId === rootBeadId) return rootBeadId;
  const explicit = meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id);
  if (explicit) return externalizeId(explicit);
  const stepId = meta(bead, 'gc.step_id');
  if (stepId) return externalizeId(stepId);
  const ref = normalizedStepRef(bead);
  if (ref) {
    const semanticId = semanticIdFromStepRef(ref);
    if (semanticId) return externalizeId(semanticId);
  }
  return externalizeId(beadId ?? 'run-node');
}

export function hiddenBadgeTargetFor(bead: RunSnapshotBead, rootBeadId: string): string | null {
  const kind = constructKindFor(bead, rootBeadId);
  if (kind === 'run-finalize') return rootBeadId;
  const controlRef = meta(bead, 'gc.control_for');
  if (controlRef) {
    const controlTarget = semanticIdFromControlRef(controlRef);
    if (controlTarget) return externalizeId(controlTarget);
  }
  const ref = normalizedStepRef(bead);
  if (!ref) return null;
  const target = semanticIdFromControlRef(ref);
  return target ? externalizeId(target) : null;
}

export function constructKindFor(bead: RunSnapshotBead, rootBeadId: string): RunConstructKind {
  const beadId = nonEmpty(bead.id);
  if (beadId && beadId === rootBeadId) return 'run-root';
  const kind = rawKind(bead);
  switch (kind) {
    case 'ralph':
      return 'check-loop';
    case 'retry':
      return 'retry';
    case 'scope':
    case 'epic':
    case 'body':
      return 'scope';
    case 'fanout':
      return 'fanout';
    case 'condition':
      return 'condition';
    case 'expand':
    case 'expansion':
      return 'expansion';
    case 'scope-check':
      return 'scope-check';
    case 'run-finalize':
      return 'run-finalize';
    case 'spec':
      return 'spec';
    case 'cleanup':
      return 'control';
    default:
      return 'step';
  }
}

export function externalKindFor(bead: RunSnapshotBead, constructKind: RunConstructKind): string {
  if (constructKind === 'check-loop') return 'check-loop';
  const kind = rawKind(bead);
  return kind === 'ralph' ? 'check-loop' : kind || constructKind;
}

export function displayTitleFor(bead: RunSnapshotBead, fallback: string): string {
  return externalizeDisplayText(nonEmpty(bead.title) ?? fallback.replace(/[-_]/g, ' '));
}

export function badgeLabelFor(kind: RunConstructKind): string {
  switch (kind) {
    case 'scope-check':
      return 'scope check';
    case 'run-finalize':
      return 'finalize';
    case 'check-loop':
    case 'condition':
    case 'control':
    case 'expansion':
    case 'fanout':
    case 'retry':
    case 'scope':
    case 'spec':
    case 'step':
    case 'unknown':
    case 'run-root':
      return kind.replace(/-/g, ' ');
  }
}

export function loopControlNodeIdFor(bead: RunSnapshotBead): string | undefined {
  const scopeRef = meta(bead, 'gc.scope_ref') ?? nonEmpty(bead.scope_ref);
  const scopeControlId = scopeRef
    ? loopControlIdFromRuntimeRef(scopeRef, ['iteration', 'run'])
    : undefined;
  if (scopeControlId) return scopeControlId;

  const ref = normalizedStepRef(bead);
  if (!ref) return undefined;
  return loopControlIdFromRuntimeRef(ref, ['iteration']);
}

function rawKind(bead: RunSnapshotBead): string {
  return meta(bead, 'gc.kind') ?? meta(bead, 'gc.original_kind') ?? nonEmpty(bead.kind) ?? '';
}

function semanticIdFromStepRef(ref: string): string | undefined {
  const parts = ref.split('.').filter(Boolean);
  if (parts.length === 0) return undefined;

  const semanticParts = stripRuntimeSuffix(parts);
  const iterationIndex = semanticParts.lastIndexOf('iteration');
  if (
    iterationIndex >= 0 &&
    iterationIndex < semanticParts.length - 2 &&
    isPositiveInteger(semanticParts[iterationIndex + 1])
  ) {
    return semanticParts.at(-1);
  }
  if (
    iterationIndex === semanticParts.length - 2 &&
    isPositiveInteger(semanticParts[iterationIndex + 1])
  ) {
    return semanticParts[iterationIndex - 1];
  }
  return semanticParts.at(-1);
}

function semanticIdFromControlRef(ref: string): string | undefined {
  return semanticIdFromStepRef(stripScopeCheckSuffix(ref));
}

function stripScopeCheckSuffix(ref: string): string {
  return ref.replace(/-scope-check$/, '').replace(/\.scope-check$/, '');
}

function stripRuntimeSuffix(parts: string[]): string[] {
  const marker = parts.at(-2);
  const value = parts.at(-1);
  if (
    value &&
    marker &&
    isPositiveInteger(value) &&
    (marker === 'attempt' || marker === 'run' || marker === 'check' || marker === 'eval')
  ) {
    return parts.slice(0, -2);
  }
  return parts;
}

function loopControlIdFromRuntimeRef(ref: string, markers: readonly string[]): string | undefined {
  const parts = ref.split('.').filter(Boolean);
  for (const marker of markers) {
    const markerIndex = parts.findIndex(
      (part, index) => part === marker && isPositiveInteger(parts[index + 1]),
    );
    if (markerIndex <= 0) continue;
    const controlId = parts[markerIndex - 1];
    return controlId ? externalizeId(controlId) : undefined;
  }
  return undefined;
}

function isPositiveInteger(value: string | undefined): boolean {
  if (!value) return false;
  const parsed = Number.parseInt(value, 10);
  return String(parsed) === value && parsed > 0;
}

function externalizeDisplayText(value: string): string {
  if (!/(^|[^A-Za-z0-9])ralph(?=$|[^A-Za-z0-9])/i.test(value)) return value;
  return value
    .replace(/[-_]+/g, ' ')
    .replace(/(^|[^A-Za-z0-9])ralph(?=$|[^A-Za-z0-9])/gi, '$1check loop')
    .replace(/\s+/g, ' ')
    .trim();
}
