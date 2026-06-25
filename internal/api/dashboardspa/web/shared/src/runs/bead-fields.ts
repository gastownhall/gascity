import type { RunSnapshotBead } from '../run-snapshot.js';

export function meta(bead: RunSnapshotBead | undefined, key: string): string | undefined {
  const value = bead?.metadata?.[key];
  if (typeof value === 'string') return nonEmpty(value);
  return undefined;
}

export function nonEmpty(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim().length > 0 ? value.trim() : undefined;
}

export function normalizedStepRef(bead: RunSnapshotBead): string | null {
  const ref = meta(bead, 'gc.step_ref') ?? nonEmpty(bead.step_ref);
  return ref ?? null;
}

export function iterationFor(bead: RunSnapshotBead): number | undefined {
  return (
    numericMeta(bead, 'gc.iteration') ??
    numericRefSegment(bead, 'iteration') ??
    numericRefSegment(bead, 'run')
  );
}

export function attemptFor(bead: RunSnapshotBead): number | undefined {
  return (
    numericMeta(bead, 'gc.attempt') ??
    numericField(bead.attempt) ??
    numericRefSegment(bead, 'attempt')
  );
}

export function positiveIntegerMeta(bead: RunSnapshotBead, key: string): number | undefined {
  return numericMeta(bead, key);
}

export function externalizeId(id: string): string {
  return id.replace(/(^|[^A-Za-z0-9])ralph(?=$|[^A-Za-z0-9])/gi, '$1check-loop');
}

function numericRefSegment(bead: RunSnapshotBead, marker: string): number | undefined {
  const ref = normalizedStepRef(bead);
  if (!ref) return undefined;
  const parts = ref.split('.');
  for (let i = 0; i < parts.length - 1; i += 1) {
    if (parts[i] !== marker) continue;
    const parsed = numericField(parts[i + 1]);
    if (parsed !== undefined) return parsed;
  }
  return undefined;
}

function numericMeta(bead: RunSnapshotBead, key: string): number | undefined {
  return numericField(meta(bead, key));
}

function numericField(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isInteger(value) && value > 0) return value;
  if (typeof value !== 'string') return undefined;
  if (!/^[1-9]\d*$/.test(value)) return undefined;
  const parsed = Number.parseInt(value, 10);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}
