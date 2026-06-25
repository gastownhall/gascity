import type { FormulaDetail, RunSnapshotBead } from '../run-snapshot.js';
import { externalizeId, meta, nonEmpty, normalizedStepRef } from './bead-fields.js';
import type { RunNodeGroup } from './execution-instances.js';

export function orderRunNodeGroups(
  groups: readonly RunNodeGroup[],
  formulaDetail: FormulaDetail | undefined,
  rootBeadId: string,
): RunNodeGroup[] {
  const rankByAlias = formulaRankByAlias(formulaDetail);
  if (rankByAlias.size === 0) return [...groups];

  return groups
    .map((group, index) => ({
      group,
      index,
      rank:
        group.semanticNodeId === rootBeadId
          ? -1
          : rankForGroup(group, rankByAlias, formulaDetail?.name),
    }))
    .sort((left, right) => left.rank - right.rank || left.index - right.index)
    .map((entry) => entry.group);
}

function formulaRankByAlias(formulaDetail: FormulaDetail | undefined): Map<string, number> {
  const steps = formulaDetail?.preview?.nodes ?? formulaDetail?.steps ?? [];
  const ranks = new Map<string, number>();
  steps.forEach((step, index) => {
    for (const alias of formulaStepAliases(step.id, formulaDetail?.name)) {
      if (!ranks.has(alias)) ranks.set(alias, index);
    }
  });
  return ranks;
}

function rankForGroup(
  group: RunNodeGroup,
  ranks: ReadonlyMap<string, number>,
  formulaName: string | undefined,
): number {
  let rank = Number.POSITIVE_INFINITY;
  for (const alias of groupAliases(group, formulaName)) {
    const candidate = ranks.get(alias);
    if (candidate !== undefined && candidate < rank) rank = candidate;
  }
  return rank;
}

function groupAliases(group: RunNodeGroup, formulaName: string | undefined): string[] {
  return [
    group.semanticNodeId,
    ...group.beads.flatMap((bead) => beadAliases(bead, formulaName)),
  ].flatMap((alias) => aliasVariants(alias));
}

function beadAliases(bead: RunSnapshotBead, formulaName: string | undefined): string[] {
  return [
    nonEmpty(bead.id),
    meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id),
    meta(bead, 'gc.step_id'),
    normalizedStepRef(bead),
  ]
    .filter((value): value is string => value !== undefined)
    .flatMap((value) => aliasVariants(value, formulaName));
}

function formulaStepAliases(id: string, formulaName: string | undefined): string[] {
  return aliasVariants(id, formulaName);
}

function aliasVariants(value: string, formulaName?: string): string[] {
  const clean = nonEmpty(value);
  if (!clean) return [];
  const stripped = stripFormulaPrefix(clean, formulaName);
  return unique(
    [clean, stripped, stripControlSuffix(clean), stripControlSuffix(stripped)].map((candidate) =>
      externalizeId(candidate),
    ),
  );
}

function stripFormulaPrefix(value: string, formulaName: string | undefined): string {
  if (!formulaName) return value;
  const prefix = `${formulaName}.`;
  return value.startsWith(prefix) ? value.slice(prefix.length) : value;
}

function stripControlSuffix(value: string): string {
  return value.replace(/-scope-check$/, '').replace(/\.scope-check$/, '');
}

function unique(values: string[]): string[] {
  return [...new Set(values)];
}
