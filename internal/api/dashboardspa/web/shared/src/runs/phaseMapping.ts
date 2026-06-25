// Pure phase-classification rules for the runs collector
// (gascity-dashboard-0t6). Kept in their own module so the rules can be
// tested independently of the lane builder and the transport layer.
//
// All exports here are deterministic functions over RunIssue values;
// no IO, no global state. The companion test file pins the upstream
// classifier behavior so the React translation of RunMap inherits a
// consistent phase grammar.
//
// Phase is derived structurally — from the run's CURRENT step (gc.step_id)
// classified into a generic RunPhase (see structuredPhase / stepIdPhase). Only
// when no step carries a gc.step_id does it fall back to a tightened keyword
// scan scoped to step-identity signals (titles + gc.step_id), never the full
// description+metadata dump (see fallbackPhase). Parent keyword scans read
// DashboardBead.parent first and fall back to the older
// metadata['gc.parent_bead_id'] marker when present.

import type { DashboardBead } from '../dashboard-beads.js';
import type { RunPhase as SharedRunPhase, RunStage } from '../snapshot/types.js';

export interface RunIssue {
  id: string;
  title: string;
  description?: string;
  status: string;
  issue_type: string;
  assignee?: string;
  updated_at: string;
  /** Populated from DashboardBead.parent or metadata['gc.parent_bead_id']. */
  parent?: string;
  metadata?: Record<string, string>;
}

export interface PhaseMapping {
  phase: SharedRunPhase;
  label: string;
  reviewRound: number | null;
}

export function mapRunPhase(issues: RunIssue[]): PhaseMapping {
  // Status-based branches first — these are authoritative and correct.
  if (issues.some((i) => i.status === 'blocked' || textForIssue(i).includes('blocked'))) {
    return { phase: 'blocked', label: 'blocked', reviewRound: null };
  }

  if (issues.length > 0 && issues.every((i) => i.status === 'closed')) {
    return { phase: 'complete', label: 'complete', reviewRound: null };
  }

  // gascity-dashboard-q3p1: structured-first phase derivation. When the run has
  // step beads carrying gc.step_id, the run's phase is the phase of its CURRENT
  // step (the in_progress primary step, else the latest-advanced primary step) —
  // a real progress signal that tracks the run forward as steps advance. The old
  // keyword scan over title+description+metadata collapsed almost every formula
  // run to 'approval' because needles like 'gate' (order:gate-sweep, "ship gate")
  // and 'human' (the ubiquitous summary_for_human metadata key) matched incidental
  // text in some bead of the group. Step-id is a structural identifier, not free
  // text, so it does not suffer that false-positive class.
  const structured = structuredPhase(issues);
  if (structured !== null) {
    return structured;
  }

  // Fallback (no resolvable step identity): a tightened keyword scan scoped to
  // step-identity signals only (titles + gc.step_id), with the incidental-match
  // needles removed. Conservative — returns 'active' when genuinely ambiguous.
  return fallbackPhase(issues);
}

/**
 * gascity-dashboard-q3p1: derive the phase from the run's current step.
 *
 * The active step is the latest in_progress primary step; if none is in_progress
 * (e.g. between steps) it is the latest primary step that carries a gc.step_id.
 * The step-id is classified into a generic RunPhase by stepIdPhase. Returns null
 * when no primary step carries a gc.step_id (no structured signal to use).
 */
function structuredPhase(issues: RunIssue[]): PhaseMapping | null {
  const primary = issues.filter(isPrimaryStepIssue);
  const inProgressStep = latestStepId(primary.filter((i) => i.status === 'in_progress'));
  // gascity-dashboard (Major 3): when no step is in_progress, the current step
  // is the FURTHEST-ADVANCED step by stage rank — a deterministic, order- and
  // timestamp-independent signal. The run-detail snapshot adapter sets every
  // bead updated_at='' (the snapshot carries no per-bead timestamp), so a
  // timestamp-based pick there is input-order-dependent and can drift from the
  // summary lane. Stage rank is derived from gc.step_id alone, so the summary
  // context (real timestamps) and the detail context (empty timestamps) resolve
  // the same run to the same phase.
  const activeStepId = inProgressStep ?? furthestStageStepId(primary);
  if (activeStepId === null) {
    return null;
  }

  const phase = stepIdPhase(activeStepId);
  if (phase === 'review') {
    const resolved = reviewRoundForIssues(issues) ?? fallbackReviewRound(issues);
    return { phase: 'review', label: `review round ${resolved}`, reviewRound: resolved };
  }
  return { phase, label: phase, reviewRound: null };
}

/**
 * Classify a single gc.step_id into a generic RunPhase. The step id is split on
 * its structural delimiters (`-`, `.`, `_`, `:`, `/`) into TOKENS and matched
 * WHOLE-TOKEN against per-stage keyword sets — never as raw substrings. Whole-
 * token matching is what stops a CI/leading step from being misbucketed onto a
 * late stage: `pre-approval-ci` is not approval, `dispatch-implementation` is
 * implementation (not finalization), `prepare-review-context` is review (not
 * approval/finalization).
 *
 * Precedence is latest-stage-first (approval > finalization > review >
 * implementation > intake), preserving the approval-before-finalization rule so
 * an approval gate that names its successor (`approve-merge`,
 * `verify-merge-approval`) resolves to approval, not finalization, while a pure
 * finalization step (`merge-and-finalize`) still resolves to finalization.
 *
 * The gate stages (approval, finalization) additionally reject the stage token
 * when ANY lead-up qualifier token (`pre`, `prepare`, `wait`, `await`,
 * `pending`, `before`, `for`, `to`) appears anywhere in the step-id tokens —
 * not only immediately before the stage token. Those are steps that LEAD UP TO
 * the gate, not the gate itself (`pre-approval-ci`, `wait-for-approval`,
 * `prepare-for-merge`). Implementation/review/intake do not apply the qualifier
 * rule: a real stage token there is a reliable signal regardless of qualifier.
 *
 * Returns 'active' when no token names a recognizable stage — deliberately
 * conservative, so an unknown step never invents a specific late phase.
 */
export function stepIdPhase(stepId: string): SharedRunPhase {
  const tokens = tokenizeStepId(stepId);
  if (hasStageToken(tokens, APPROVAL_STAGE_TOKENS, { rejectWithLeadUpQualifier: true })) {
    return 'approval';
  }
  if (hasStageToken(tokens, FINALIZATION_STAGE_TOKENS, { rejectWithLeadUpQualifier: true })) {
    return 'finalization';
  }
  if (hasStageToken(tokens, REVIEW_STAGE_TOKENS)) return 'review';
  if (hasStageToken(tokens, IMPLEMENTATION_STAGE_TOKENS)) return 'implementation';
  if (hasStageToken(tokens, INTAKE_STAGE_TOKENS)) return 'intake';
  return 'active';
}

const STEP_ID_DELIMITERS = /[-._:/]+/;

function tokenizeStepId(stepId: string): string[] {
  return stepId.toLowerCase().split(STEP_ID_DELIMITERS).filter(Boolean);
}

// Qualifier tokens that mark a step as LEADING UP TO a gate rather than being
// the gate itself, wherever they appear in the step id: `pre-approval-ci`,
// `wait-for-approval`, `prepare-for-merge`, `before-merge`, `pending-approval`.
const LEAD_UP_QUALIFIER_TOKENS: ReadonlySet<string> = new Set([
  'pre',
  'prepare',
  'wait',
  'await',
  'pending',
  'before',
  'for',
  'to',
]);

function hasStageToken(
  tokens: readonly string[],
  stageTokens: ReadonlySet<string>,
  options: { rejectWithLeadUpQualifier?: boolean } = {},
): boolean {
  if (!tokens.some((token) => stageTokens.has(token))) return false;
  // Gate stages: a stage token is the gate only when no lead-up qualifier token
  // appears anywhere in the step id (a step that LEADS UP TO the gate is not it).
  if (options.rejectWithLeadUpQualifier) {
    return !tokens.some((token) => LEAD_UP_QUALIFIER_TOKENS.has(token));
  }
  return true;
}

// Whole-token stage vocabularies, matched against tokenized gc.step_id values.
// Drawn from the declared step ids in stagesForFormula plus the v1/wisp step
// names (do-work, load-context, …). Multi-word step ids contribute each of
// their tokens (e.g. `merge-and-finalize` → merge, finalize).
const APPROVAL_STAGE_TOKENS: ReadonlySet<string> = new Set([
  'approval',
  'approve',
  'approved',
  'gate',
]);
const FINALIZATION_STAGE_TOKENS: ReadonlySet<string> = new Set([
  'finalize',
  'finalization',
  'merge',
  'cleanup',
  'publish',
]);
const REVIEW_STAGE_TOKENS: ReadonlySet<string> = new Set([
  'review',
  'reviewer',
  'scorecard',
  'persona',
  'personas',
  'audit',
  'repro',
  'baseline',
  'investigation',
  'classify',
  'classification',
]);
const IMPLEMENTATION_STAGE_TOKENS: ReadonlySet<string> = new Set([
  'implement',
  'implementation',
  'patch',
  'fixes',
  'work',
  'design',
]);
const INTAKE_STAGE_TOKENS: ReadonlySet<string> = new Set([
  'intake',
  'bootstrap',
  'context',
  'router',
  'request',
  'preflight',
  'setup',
  'rebase',
]);

/**
 * Keyword fallback used only when no step carries a gc.step_id. Scans
 * step-identity signals (issue titles + any gc.step_id present) rather than the
 * full description+metadata dump, and drops the incidental-matching needles
 * ('gate', 'human', 'merge', 'close', 'report', 'work', 'fix', 'code') that
 * collapsed real runs onto late phases. Conservative: 'active' when ambiguous.
 */
function fallbackPhase(issues: RunIssue[]): PhaseMapping {
  if (stepSignalContainsAny(issues, ['approval', 'approved', 'finalize-scope'])) {
    return { phase: 'approval', label: 'approval', reviewRound: null };
  }

  if (stepSignalContainsAny(issues, ['post-merge', 'finalization', 'finalize'])) {
    return { phase: 'finalization', label: 'finalization', reviewRound: null };
  }

  const round = reviewRoundForIssues(issues);
  if (round !== null || stepSignalContainsAny(issues, ['review', 'reviewer', 'scorecard'])) {
    const resolved = round ?? fallbackReviewRound(issues);
    return { phase: 'review', label: `review round ${resolved}`, reviewRound: resolved };
  }

  if (stepSignalContainsAny(issues, ['implementation', 'patch', 'do-work'])) {
    return { phase: 'implementation', label: 'implementation', reviewRound: null };
  }

  if (stepSignalContainsAny(issues, ['intake', 'load-context', 'router', 'request'])) {
    return { phase: 'intake', label: 'intake', reviewRound: null };
  }

  return { phase: 'active', label: 'active', reviewRound: null };
}

/**
 * Step-identity text for fallback scanning: the issue title plus any gc.step_id.
 * Excludes description and the metadata dump so incidental words (e.g. a
 * summary_for_human value, a "ship gate" mention) never drive the phase.
 */
function stepSignalText(issue: RunIssue): string {
  const stepId = stringValue(issue.metadata?.['gc.step_id']);
  return [issue.title, stepId].filter(Boolean).join(' ').toLowerCase();
}

function stepSignalContainsAny(issues: RunIssue[], needles: string[]): boolean {
  return issues.some((i) => {
    const text = stepSignalText(i);
    return needles.some((n) => text.includes(n));
  });
}

/**
 * Returns the per-issue review round when one is encoded in metadata.
 * Three supported shapes (matching demo-dash):
 *   1. key like `*iteration.N` or `*attempt.N` → N (from key).
 *   2. key like `*iteration` or `*attempt` with numeric value → value.
 *   3. value matching `*iteration.N` or `*attempt.N` → N (from value).
 */
export function reviewRoundForIssue(issue: RunIssue): number | null {
  const metadata = issue.metadata ?? {};

  // Capture group 1 in both ROUND_IN_KEY and ROUND_IN_VALUE is the digit
  // sequence. noUncheckedIndexedAccess makes match[1] type as
  // `string | undefined`, so each branch checks `!== undefined` before
  // numeric coercion.
  for (const [key, value] of Object.entries(metadata)) {
    const keyMatch = key.match(ROUND_IN_KEY);
    if (keyMatch && keyMatch[1] !== undefined) {
      return Number(keyMatch[1]);
    }

    if (ROUND_KEY_NO_DIGITS.test(key)) {
      const attempt = parsePositiveInteger(value);
      if (attempt !== null) {
        return attempt;
      }
    }

    const valueMatch = String(value).match(ROUND_IN_VALUE);
    if (valueMatch && valueMatch[1] !== undefined) {
      return Number(valueMatch[1]);
    }
  }

  return null;
}

const ROUND_IN_KEY = /(?:^|\.)(?:iteration|attempt)\.(\d+)$/;
const ROUND_IN_VALUE = /(?:^|\.)(?:iteration|attempt)\.(\d+)$/;
const ROUND_KEY_NO_DIGITS = /(?:^|\.)(?:iteration|attempt)$/;

export function reviewRoundForIssues(issues: RunIssue[]): number | null {
  const rounds = issues.map(reviewRoundForIssue).filter((r): r is number => r !== null);
  if (rounds.length === 0) return null;
  return Math.max(...rounds);
}

export function fallbackReviewRound(issues: RunIssue[]): number {
  const reviewIssueCount = issues.filter((i) => textForIssue(i).includes('review')).length;
  return Math.max(reviewIssueCount, 1);
}

export function textForIssue(issue: RunIssue): string {
  // gascity-dashboard-9w3k: skip `gc.var.*` keys. v1 / wisp runs carry operator
  // free-text template inputs there (e.g. gc.var.prompt = "review the blocked PR
  // and merge it") which would otherwise feed phase needles ('review', 'blocked',
  // 'merge', 'approval', ...) and mis-bucket the run's phase. These are inputs,
  // not structural progress signals, so they are excluded from classification.
  // graph.v2 roots derive phase from gc.step_id / status, not gc.var.*, so this
  // does not affect graph.v2 classification.
  const metadataText = Object.entries(issue.metadata ?? {})
    .filter(([key]) => !key.startsWith('gc.var.'))
    .map(([key, value]) => `${key} ${String(value)}`)
    .join(' ');

  return [
    issue.title,
    issue.description,
    issue.status,
    issue.issue_type,
    issue.assignee,
    issue.parent,
    metadataText,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase();
}

export function stringValue(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function parsePositiveInteger(value: unknown): number | null {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : null;
}

// ── DashboardBead adapter ─────────────────────────────────────────────────────────

/**
 * Adapt the dashboard bead projection to the phase classifier's input. The
 * metadata fallback survives because formula scaffolding can still write the
 * older `gc.parent_bead_id` key on synthetic beads.
 */
export function fromDashboardBead(bead: DashboardBead): RunIssue {
  const parent = bead.parent ?? stringValue(bead.metadata?.['gc.parent_bead_id']);
  const issue: RunIssue = {
    id: bead.id,
    title: bead.title,
    status: bead.status,
    issue_type: bead.issue_type,
    updated_at: bead.updated_at ?? bead.created_at,
  };
  if (bead.description !== undefined) issue.description = bead.description;
  if (bead.assignee !== undefined) issue.assignee = bead.assignee;
  if (parent) issue.parent = parent;
  if (bead.metadata !== undefined) issue.metadata = bead.metadata;
  return issue;
}

// ── Stage progression ──────────────────────────────────────────────────────

const runStages = [
  ['intake', 'Intake'],
  ['implementation', 'Implementation'],
  ['review', 'Review'],
  ['approval', 'Approval'],
  ['finalization', 'Finalization'],
] as const;

export function stageProgress(
  phase: PhaseMapping,
  formula: string | null,
  issues: RunIssue[],
): RunStage[] {
  const formulaStages = stagesForFormula(formula);
  if (formulaStages.length > 0) {
    return formulaStageProgress(formulaStages, issues);
  }

  if (phase.phase === 'blocked') {
    return [{ key: 'blocked', label: 'Blocked', status: 'blocked' }];
  }

  if (phase.phase === 'complete') {
    return runStages.map(([key, label]) => ({
      key,
      label,
      status: 'complete' as const,
    }));
  }

  const activeIndex = runStages.findIndex(([key]) => key === phase.phase);

  if (activeIndex < 0) {
    return runStages.map(([key, label]) => ({
      key,
      label,
      status: (key === 'implementation' ? 'active' : 'pending') as RunStage['status'],
    }));
  }

  return runStages.map(([key, label], idx) => ({
    key,
    label:
      key === 'review' && phase.reviewRound !== null ? `Review round ${phase.reviewRound}` : label,
    status: (idx < activeIndex
      ? 'complete'
      : idx === activeIndex
        ? 'active'
        : 'pending') as RunStage['status'],
  }));
}

export function stagesForFormula(
  formula: string | null,
): Array<{ key: string; label: string; steps: string[] }> {
  if (formula === 'mol-adopt-pr-v2') {
    return [
      { key: 'preflight', label: 'Preflight', steps: ['preflight'] },
      { key: 'rebase', label: 'Worktree / rebase', steps: ['rebase-check'] },
      {
        key: 'review',
        label: 'Review loop',
        steps: [
          'review-loop',
          'review-pipeline.review-claude',
          'review-pipeline.review-codex',
          'review-pipeline.review-gemini',
          'review-pipeline.synthesize',
          'review-pipeline.quality-scorecard',
          'apply-fixes',
        ],
      },
      {
        key: 'ci',
        label: 'Pre-approval CI',
        steps: ['pre-approval-ci', 'repair-ci-failures'],
      },
      { key: 'approval', label: 'Human approval', steps: ['human-approval'] },
      { key: 'finalize', label: 'Merge-ready', steps: ['finalize'] },
      { key: 'cleanup', label: 'Cleanup', steps: ['cleanup-worktree'] },
    ];
  }

  if (formula === 'mol-design-review-v2') {
    return [
      { key: 'setup', label: 'Setup', steps: ['design-review.setup'] },
      {
        key: 'personas',
        label: 'Personas',
        steps: [
          'design-review.persona-gen-claude',
          'design-review.persona-gen-codex',
          'design-review.persona-gen-gemini',
          'design-review.persona-synthesis',
        ],
      },
      {
        key: 'fanout',
        label: 'Persona fanout',
        steps: ['design-review.prepare-review-items', 'design-review.persona-review-fanout'],
      },
      {
        key: 'synthesis',
        label: 'Synthesis',
        steps: ['design-review.global-synthesis'],
      },
      {
        key: 'apply',
        label: 'Apply findings',
        steps: ['design-review.apply-design-changes'],
      },
      { key: 'finalize', label: 'Finalize', steps: ['finalize'] },
    ];
  }

  if (formula === 'mol-bug-report-flow-v2') {
    return [
      {
        key: 'intake',
        label: 'Intake',
        steps: ['bootstrap-run', 'refresh-intake'],
      },
      {
        key: 'repro',
        label: 'Reproduction',
        steps: ['historical-baseline', 'reported-build-repro', 'main-repro'],
      },
      {
        key: 'audit',
        label: 'Audit',
        steps: ['code-path-audit', 'coverage-audit', 'related-refs-audit'],
      },
      {
        key: 'classify',
        label: 'Classify',
        steps: ['investigation-synthesis', 'followup-evidence', 'normalize-outcome'],
      },
      {
        key: 'approval',
        label: 'Human approval',
        steps: ['approve-classification', 'verify-classification-approval'],
      },
      { key: 'publish', label: 'Publish', steps: ['publish-classification'] },
      {
        key: 'dispatch',
        label: 'Dispatch fix',
        steps: ['dispatch-implementation'],
      },
    ];
  }

  if (formula === 'mol-bug-report-implementation-v2') {
    return [
      {
        key: 'plan',
        label: 'Plan approval',
        steps: ['approve-fix-plan', 'approve-test-hardening-plan', 'verify-selected-plan-approval'],
      },
      {
        key: 'design',
        label: 'Design review',
        steps: ['prepare-design-review-doc', 'design-review'],
      },
      {
        key: 'implement',
        label: 'Implement',
        steps: ['implement-change', 'prepare-review-context'],
      },
      {
        key: 'review',
        label: 'Code review',
        steps: ['code-review-loop', 'apply-code-fixes'],
      },
      {
        key: 'pr',
        label: 'Open PR',
        steps: ['approve-pr-open', 'verify-pr-open-approval', 'open-or-update-pr'],
      },
      { key: 'ci', label: 'CI', steps: ['wait-for-ci'] },
      {
        key: 'merge',
        label: 'Merge',
        steps: ['approve-merge', 'verify-merge-approval', 'merge-and-finalize'],
      },
    ];
  }

  return [];
}

function formulaStageProgress(
  stages: Array<{ key: string; label: string; steps: string[] }>,
  issues: RunIssue[],
): RunStage[] {
  const primary = issues.filter(isPrimaryStepIssue);
  const activeStepId = latestStepId(primary.filter((i) => i.status === 'in_progress'));
  const activeIndex = activeStepId
    ? stages.findIndex((s) => s.steps.includes(activeStepId))
    : firstOpenStageIndex(stages, primary);
  const furthestClosedIndex = furthestClosedStageIndex(stages, primary);

  const stageHasClosed = (stage: { steps: string[] }): boolean =>
    stage.steps.some((step) => stepIssues(primary, step).some((i) => i.status === 'closed'));

  return stages.map((stage, idx) => {
    let status: RunStage['status'];
    if (activeIndex >= 0) {
      status = idx < activeIndex ? 'complete' : idx === activeIndex ? 'active' : 'pending';
    } else if (stageHasClosed(stage) || idx < furthestClosedIndex) {
      status = 'complete';
    } else {
      status = 'pending';
    }
    return { key: stage.key, label: stage.label, status };
  });
}

function firstOpenStageIndex(stages: Array<{ steps: string[] }>, issues: RunIssue[]): number {
  return stages.findIndex((s) =>
    s.steps.some((step) => stepIssues(issues, step).some((i) => i.status !== 'closed')),
  );
}

function furthestClosedStageIndex(stages: Array<{ steps: string[] }>, issues: RunIssue[]): number {
  let furthest = -1;
  stages.forEach((s, idx) => {
    if (s.steps.some((step) => stepIssues(issues, step).some((i) => i.status === 'closed'))) {
      furthest = idx;
    }
  });
  return furthest;
}

export function latestStepId(issues: RunIssue[]): string | null {
  return (
    [...issues]
      .sort(byMostRecentThenStage)
      .map((i) => stringValue(i.metadata?.['gc.step_id']))
      .find(Boolean) ?? null
  );
}

/**
 * gascity-dashboard (Major 3): the deterministic current-step pick for the
 * "no step in_progress" fallback. Among the steps that carry a gc.step_id,
 * select the one whose stage is furthest along the ladder (approval >
 * finalization > review > implementation > intake > active). This depends only
 * on gc.step_id — never on updated_at — so it is stable regardless of input
 * order and identical whether the beads come from the summary projection (real
 * timestamps) or the run-detail snapshot adapter (updated_at=''). Ties on stage
 * rank break on the step id string for total determinism.
 */
function furthestStageStepId(issues: RunIssue[]): string | null {
  const stepIds = issues
    .map((i) => stringValue(i.metadata?.['gc.step_id']))
    .filter((id): id is string => id.length > 0);
  if (stepIds.length === 0) return null;
  return [...stepIds].sort((a, b) => {
    const rankDelta = stageRank(stepIdPhase(b)) - stageRank(stepIdPhase(a));
    if (rankDelta !== 0) return rankDelta;
    return a < b ? -1 : a > b ? 1 : 0;
  })[0]!;
}

// Lifecycle rank (higher = further along the run lifecycle), used ONLY to pick
// the furthest-reached stage among steps (furthestStageStepId, and the latest-
// step stage tiebreak in byMostRecentThenStage). The lifecycle order is
// intake → implementation → review → approval → finalization, so finalization
// is the FURTHEST stage. 'active' is the conservative floor.
//
// NOTE: this is deliberately decoupled from the CURRENT-phase precedence in
// stepIdPhase, which checks approval BEFORE finalization so an approval gate
// that names its successor (`approve-merge`) resolves to approval. That
// precedence is encoded in the if-order of stepIdPhase, not here — the two
// concerns must not be conflated (a closed approval + closed finalization run
// has its furthest stage = finalization, while a single `approve-merge` step
// is classified as the approval phase).
const LIFECYCLE_RANK: Record<SharedRunPhase, number> = {
  active: 0,
  intake: 1,
  implementation: 2,
  review: 3,
  approval: 4,
  finalization: 5,
  blocked: 6,
  complete: 7,
};

function stageRank(phase: SharedRunPhase): number {
  return LIFECYCLE_RANK[phase];
}

/**
 * Deterministic step ordering: most-recently-updated first, then furthest stage,
 * then step id. Date.parse('') is NaN, so when the run-detail snapshot adapter
 * leaves every updated_at empty the timestamp comparison collapses to 0 and the
 * stage / step-id tiebreakers make the pick deterministic instead of leaving it
 * to the engine's NaN-comparator behavior (input-order-dependent).
 */
function byMostRecentThenStage(a: RunIssue, b: RunIssue): number {
  const timeDelta = parseTimestamp(b.updated_at) - parseTimestamp(a.updated_at);
  if (timeDelta !== 0) return timeDelta;
  const aStep = stringValue(a.metadata?.['gc.step_id']);
  const bStep = stringValue(b.metadata?.['gc.step_id']);
  const rankDelta = stageRank(stepIdPhase(bStep)) - stageRank(stepIdPhase(aStep));
  if (rankDelta !== 0) return rankDelta;
  return aStep < bStep ? -1 : aStep > bStep ? 1 : 0;
}

function parseTimestamp(value: string): number {
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? 0 : parsed;
}

export function stepIssues(issues: RunIssue[], step: string): RunIssue[] {
  return issues.filter((i) => stringValue(i.metadata?.['gc.step_id']) === step);
}

export function isPrimaryStepIssue(issue: RunIssue): boolean {
  const kind = stringValue(issue.metadata?.['gc.kind']);
  return kind !== 'spec' && kind !== 'scope-check' && kind !== 'workflow-finalize';
}
