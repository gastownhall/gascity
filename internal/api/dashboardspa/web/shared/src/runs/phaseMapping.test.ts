import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { mapRunPhase, stepIdPhase, type RunIssue } from './phaseMapping.js';

// gascity-dashboard-q3p1: phase is derived from the run's CURRENT step
// (structured-first), not from a keyword scan over title+description+metadata.
// The old scan collapsed almost every real formula run onto 'approval' because
// broad needles like 'gate' (order:gate-sweep, "ship gate") and 'human' (the
// ubiquitous summary_for_human metadata key) matched incidental text in some
// bead of the group.

function root(overrides: Partial<RunIssue> & Pick<RunIssue, 'id'>): RunIssue {
  return {
    title: 'run root',
    status: 'open',
    issue_type: 'molecule',
    updated_at: '2026-06-06T00:00:00.000Z',
    metadata: { 'gc.formula_contract': 'graph.v2', 'gc.kind': 'run' },
    ...overrides,
  };
}

function step(
  id: string,
  stepId: string,
  status: string,
  overrides: Partial<RunIssue> = {},
): RunIssue {
  return {
    id,
    title: 'step',
    status,
    issue_type: 'task',
    updated_at: '2026-06-06T00:01:00.000Z',
    metadata: { 'gc.kind': 'step', 'gc.step_id': stepId },
    ...overrides,
  };
}

describe('mapRunPhase — structured derivation from the current step (gascity-dashboard-q3p1)', () => {
  test('an in_progress implementation step → implementation', () => {
    const phase = mapRunPhase([
      root({ id: 'r1' }),
      step('r1-s1', 'implement-change', 'in_progress'),
    ]);
    assert.equal(phase.phase, 'implementation');
  });

  test('an in_progress review step → review (with a review round)', () => {
    const phase = mapRunPhase([
      root({ id: 'r2' }),
      step('r2-s1', 'code-review-loop', 'in_progress'),
    ]);
    assert.equal(phase.phase, 'review');
    assert.ok(phase.reviewRound !== null && phase.reviewRound >= 1);
  });

  test('an in_progress approval/gate step → approval (true positive still works)', () => {
    const phase = mapRunPhase([root({ id: 'r3' }), step('r3-s1', 'human-approval', 'in_progress')]);
    assert.equal(phase.phase, 'approval');
  });

  test('an in_progress finalize step → finalization', () => {
    const phase = mapRunPhase([
      root({ id: 'r4' }),
      step('r4-s1', 'merge-and-finalize', 'in_progress'),
    ]);
    assert.equal(phase.phase, 'finalization');
  });

  test('phase advances as the active step advances (graph.v2 progression)', () => {
    const base = [root({ id: 'rp' })];
    // 1. implementation in progress, later steps not yet started.
    const atImpl = mapRunPhase([
      ...base,
      step('rp-s1', 'implement-change', 'in_progress'),
      step('rp-s2', 'code-review-loop', 'open'),
      step('rp-s3', 'human-approval', 'open'),
    ]);
    assert.equal(atImpl.phase, 'implementation');

    // 2. implementation closed, review now in progress.
    const atReview = mapRunPhase([
      ...base,
      step('rp-s1', 'implement-change', 'closed'),
      step('rp-s2', 'code-review-loop', 'in_progress', {
        updated_at: '2026-06-06T01:00:00.000Z',
      }),
      step('rp-s3', 'human-approval', 'open'),
    ]);
    assert.equal(atReview.phase, 'review');

    // 3. review closed, approval now in progress.
    const atApproval = mapRunPhase([
      ...base,
      step('rp-s1', 'implement-change', 'closed'),
      step('rp-s2', 'code-review-loop', 'closed'),
      step('rp-s3', 'human-approval', 'in_progress', {
        updated_at: '2026-06-06T02:00:00.000Z',
      }),
    ]);
    assert.equal(atApproval.phase, 'approval');
  });

  test('falls back to latest advanced step when nothing is in_progress', () => {
    const phase = mapRunPhase([
      root({ id: 'rl' }),
      step('rl-s1', 'implement-change', 'closed', { updated_at: '2026-06-06T00:01:00.000Z' }),
      step('rl-s2', 'code-review-loop', 'closed', { updated_at: '2026-06-06T02:00:00.000Z' }),
    ]);
    // Latest advanced step is the review-loop, so the run reads as review.
    assert.equal(phase.phase, 'review');
  });
});

describe('mapRunPhase — incidental text never forces approval (the core regression)', () => {
  test('a summary_for_human metadata key does NOT classify the run as approval', () => {
    const phase = mapRunPhase([
      root({
        id: 'h1',
        metadata: {
          'gc.formula_contract': 'graph.v2',
          'gc.kind': 'run',
          summary_for_human: 'the run summary that operators read',
        },
      }),
      step('h1-s1', 'implement-change', 'in_progress', {
        metadata: {
          'gc.kind': 'step',
          'gc.step_id': 'implement-change',
          summary_for_human: 'implementing the change',
        },
      }),
    ]);
    assert.notEqual(phase.phase, 'approval');
    assert.equal(phase.phase, 'implementation');
  });

  test('a "gate" reference (order:gate-sweep dep / "Run BLOCKING gates" desc) does NOT classify as approval', () => {
    const phase = mapRunPhase([
      root({
        id: 'g1',
        description: 'Run BLOCKING gates before merge; order:gate-sweep dependency',
      }),
      step('g1-s1', 'implement-change', 'in_progress', {
        title: 'Run BLOCKING gates',
        description: 'order:gate-sweep — ship gate',
      }),
    ]);
    assert.notEqual(phase.phase, 'approval');
    assert.equal(phase.phase, 'implementation');
  });

  test('a run with summary_for_human but NO step beads stays conservative, not approval', () => {
    const phase = mapRunPhase([
      root({
        id: 'n1',
        title: 'A generic run',
        metadata: {
          'gc.kind': 'run',
          summary_for_human: 'a human-readable summary that mentions a gate',
        },
      }),
    ]);
    // No step identity, and the summary_for_human value (with its 'gate'/'human'
    // text) is no longer scanned — so the fallback stays conservative.
    assert.notEqual(phase.phase, 'approval');
    assert.equal(phase.phase, 'active');
  });
});

describe('mapRunPhase — deterministic current-step pick without updated_at (gascity-dashboard Major 3)', () => {
  // The run-detail snapshot adapter (formula-run.ts fromRunSnapshotBead) sets
  // every bead updated_at=''. With no in_progress step the old timestamp-based
  // pick was input-ORDER-dependent (Date.parse('') === NaN). The fallback must
  // be deterministic and pick the furthest-advanced stage, and a summary-context
  // version of the same run (real timestamps) must agree with the detail context.

  function snapshotStep(stepId: string, status: string): RunIssue {
    // Detail-snapshot shape: no per-bead timestamp.
    return {
      id: `step-${stepId}`,
      title: 'step',
      status,
      issue_type: 'task',
      updated_at: '',
      metadata: { 'gc.kind': 'step', 'gc.step_id': stepId },
    };
  }

  test('no in_progress step + empty updated_at → deterministic furthest stage, order-independent', () => {
    const forward = mapRunPhase([
      root({ id: 'd1', updated_at: '' }),
      snapshotStep('implement-change', 'closed'),
      snapshotStep('code-review-loop', 'closed'),
    ]);
    const reversed = mapRunPhase([
      root({ id: 'd1', updated_at: '' }),
      snapshotStep('code-review-loop', 'closed'),
      snapshotStep('implement-change', 'closed'),
    ]);
    // Furthest advanced of the two closed steps is the review loop.
    assert.equal(forward.phase, 'review');
    // Input order must not change the result.
    assert.equal(reversed.phase, forward.phase);
  });

  test('summary-context (real timestamps) and detail-context (empty) of the same run agree', () => {
    const detail = mapRunPhase([
      root({ id: 'd2', updated_at: '' }),
      snapshotStep('implement-change', 'closed'),
      snapshotStep('code-review-loop', 'closed'),
    ]);
    const summary = mapRunPhase([
      root({ id: 'd2' }),
      step('d2-s1', 'implement-change', 'closed', { updated_at: '2026-06-06T00:01:00.000Z' }),
      step('d2-s2', 'code-review-loop', 'closed', { updated_at: '2026-06-06T02:00:00.000Z' }),
    ]);
    assert.equal(detail.phase, summary.phase);
  });
});

describe('mapRunPhase — status branches remain authoritative', () => {
  test('any blocked bead → blocked, regardless of step identity', () => {
    const phase = mapRunPhase([root({ id: 'b1' }), step('b1-s1', 'implement-change', 'blocked')]);
    assert.equal(phase.phase, 'blocked');
  });

  test('all closed → complete, regardless of step identity', () => {
    const phase = mapRunPhase([
      root({ id: 'c1', status: 'closed' }),
      step('c1-s1', 'implement-change', 'closed'),
    ]);
    assert.equal(phase.phase, 'complete');
  });
});

describe('mapRunPhase — tightened keyword fallback (no structured steps)', () => {
  test('a do-work title (no gc.step_id) still reads as implementation', () => {
    const phase = mapRunPhase([
      root({ id: 'f1', title: 'mol-do-work' }),
      {
        id: 'f1-c1',
        title: 'Do the work',
        status: 'in_progress',
        issue_type: 'task',
        updated_at: '2026-06-06T00:02:00.000Z',
        metadata: { molecule_id: 'f1' },
      },
    ]);
    assert.equal(phase.phase, 'implementation');
  });

  test('a description-only "gate"/"human"/"merge" mention does NOT reach approval/finalization in the fallback', () => {
    const phase = mapRunPhase([
      root({
        id: 'f2',
        title: 'A generic run',
        description: 'review the gate, ask a human, then merge and report',
      }),
    ]);
    // Description is no longer scanned and the title carries no step signal →
    // conservative active. The incidental 'gate'/'human'/'merge' words in the
    // description never force a late phase.
    assert.notEqual(phase.phase, 'approval');
    assert.notEqual(phase.phase, 'finalization');
    assert.equal(phase.phase, 'active');
  });
});

describe('stepIdPhase — step-identity classification', () => {
  test('classifies representative declared step ids', () => {
    assert.equal(stepIdPhase('bootstrap-run'), 'intake');
    assert.equal(stepIdPhase('preflight'), 'intake');
    assert.equal(stepIdPhase('implement-change'), 'implementation');
    assert.equal(stepIdPhase('implementation.patch'), 'implementation');
    assert.equal(stepIdPhase('do-work'), 'implementation');
    assert.equal(stepIdPhase('review-pipeline.review-claude'), 'review');
    assert.equal(stepIdPhase('code-review-loop'), 'review');
    assert.equal(stepIdPhase('human-approval'), 'approval');
    assert.equal(stepIdPhase('approve-merge'), 'approval');
    assert.equal(stepIdPhase('merge-and-finalize'), 'finalization');
    assert.equal(stepIdPhase('cleanup-worktree'), 'finalization');
  });

  test('unknown step id is conservative (active), never invents a late phase', () => {
    assert.equal(stepIdPhase('totally-unknown-step'), 'active');
  });

  // gascity-dashboard (Major 1): tokenized whole-token matching, not raw
  // substring includes(). A leading/CI step that merely CONTAINS a late-stage
  // word as a substring (or as a token behind a negating prefix) must not be
  // misbucketed onto the late stage — that falsely surfaces a CI step as
  // "waiting on human" through needsOperator (health.ts keys on phase==='approval').
  test('pre-approval-ci is NOT approval (negating `pre` prefix on the gate token)', () => {
    assert.notEqual(stepIdPhase('pre-approval-ci'), 'approval');
  });

  test('dispatch-implementation is implementation, not finalization', () => {
    assert.equal(stepIdPhase('dispatch-implementation'), 'implementation');
  });

  test('prepare-review-context is review, never approval or finalization', () => {
    const phase = stepIdPhase('prepare-review-context');
    assert.notEqual(phase, 'approval');
    assert.notEqual(phase, 'finalization');
    assert.equal(phase, 'review');
  });

  test('whole real step ids still classify correctly after tokenization', () => {
    assert.equal(stepIdPhase('review'), 'review');
    assert.equal(stepIdPhase('approval'), 'approval');
    assert.equal(stepIdPhase('approve'), 'approval');
    assert.equal(stepIdPhase('implementation'), 'implementation');
    assert.equal(stepIdPhase('do-work'), 'implementation');
    assert.equal(stepIdPhase('load-context'), 'intake');
    assert.equal(stepIdPhase('finalize'), 'finalization');
  });

  test('a substring that is not a whole token does not match (e.g. `approval` inside a longer token)', () => {
    // `disapproval-note` tokenizes to [disapproval, note]; neither token is a
    // recognized stage word, so the old includes('approval') false positive
    // is gone.
    assert.equal(stepIdPhase('disapproval-note'), 'active');
  });

  // gascity-dashboard (Residual A): the gate stages (approval, finalization)
  // reject the stage token when ANY lead-up qualifier token appears anywhere in
  // the step-id tokens — not only when the qualifier immediately precedes the
  // stage token. `wait-for-approval` ([wait,for,approval]) and `prepare-for-merge`
  // ([prepare,for,merge]) are steps that LEAD UP TO the gate, so they must not
  // classify as the gate even though the token before the stage token is `for`.
  describe('gate stages reject any lead-up qualifier token anywhere (Residual A)', () => {
    test('pre-approval-ci is NOT approval (qualifier `pre` anywhere)', () => {
      assert.notEqual(stepIdPhase('pre-approval-ci'), 'approval');
    });

    test('wait-for-approval is NOT approval (qualifier `wait`/`for`, not adjacent)', () => {
      assert.notEqual(stepIdPhase('wait-for-approval'), 'approval');
    });

    test('prepare-for-merge is NOT finalization (qualifier `prepare`/`for`, not adjacent)', () => {
      assert.notEqual(stepIdPhase('prepare-for-merge'), 'finalization');
    });

    test('true gates still classify: approval/approve → approval', () => {
      assert.equal(stepIdPhase('approval'), 'approval');
      assert.equal(stepIdPhase('approve'), 'approval');
    });

    test('true gates still classify: finalize/finalization → finalization', () => {
      assert.equal(stepIdPhase('finalize'), 'finalization');
      assert.equal(stepIdPhase('finalization'), 'finalization');
    });

    test('approve-merge is approval — it IS the approval step, no lead-up qualifier', () => {
      assert.equal(stepIdPhase('approve-merge'), 'approval');
    });

    test('non-gate stages keep classifying on a whole stage token (no qualifier rejection)', () => {
      assert.equal(stepIdPhase('review'), 'review');
      assert.equal(stepIdPhase('do-work'), 'implementation');
      assert.equal(stepIdPhase('implementation'), 'implementation');
      assert.equal(stepIdPhase('load-context'), 'intake');
    });
  });
});

// gascity-dashboard (Residual B): furthestStageStepId / latestStepId stage
// tiebreak rank the lifecycle order intake → implementation → review → approval
// → finalization, so finalization is the FURTHEST stage. A no-in_progress run
// whose furthest closed step is finalization must read as finalization, not
// approval. The mapRunPhase CURRENT-phase precedence (approval before
// finalization) is a separate concern and stays as-is.
describe('mapRunPhase — finalization is the furthest lifecycle stage (Residual B)', () => {
  function snapshotStep(stepId: string, status: string): RunIssue {
    return {
      id: `step-${stepId}`,
      title: 'step',
      status,
      issue_type: 'task',
      updated_at: '',
      metadata: { 'gc.kind': 'step', 'gc.step_id': stepId },
    };
  }

  test('no in_progress, closed approve-merge + closed merge-and-finalize → finalization (NOT approval)', () => {
    const forward = mapRunPhase([
      root({ id: 'fb1', updated_at: '' }),
      snapshotStep('approve-merge', 'closed'),
      snapshotStep('merge-and-finalize', 'closed'),
    ]);
    const reversed = mapRunPhase([
      root({ id: 'fb1', updated_at: '' }),
      snapshotStep('merge-and-finalize', 'closed'),
      snapshotStep('approve-merge', 'closed'),
    ]);
    assert.equal(forward.phase, 'finalization');
    assert.equal(reversed.phase, forward.phase);
  });

  test('no in_progress, closed review + closed approval → approval (approval furthest of the two)', () => {
    const phase = mapRunPhase([
      root({ id: 'fb2', updated_at: '' }),
      snapshotStep('code-review-loop', 'closed'),
      snapshotStep('human-approval', 'closed'),
    ]);
    assert.equal(phase.phase, 'approval');
  });
});
