# Session Refactor Derisk Sequence

| Field | Value |
|---|---|
| Status | Active |
| Behavior source | `REQUIREMENTS.md` (scenario ledger) |
| Design context | `DESIGN.md` core sections (Goal, Shape, Boundaries, Refactor Rules, Non-Goals) under review bead `ga-unpr2y` |
| Method | One small behavior-preserving extraction per PR, proven by existing characterization tests at the same SHA |

This is the executable sequence for the session refactor. It replaces
document-level gating with per-PR proof: each step is a normal PR reviewed
against the scenario rows it touches, and each step must leave every existing
test passing unchanged.

Step tracking is canonical in beads, not in this file: ga-ltlwc1 (Step 1),
ga-i9r8fi (Step 2), ga-4of1nc / ga-7f6ocx / ga-frfj2d / ga-kmoj9c (Step 3),
ga-q65c22 (Step 4), ga-mxchkb (Step 5). This document holds the rationale and
ordering; `bd show <id>` holds current status.

The target shape, restated from the design core:

```text
caller gathers facts -> internal/session decides -> caller executes
```

Deciders are pure: no store, runtime, config-loader, event, subprocess, or
ambient clock access. When a decision needs an expensive or live fact, the
decider returns a gather action naming the fact and the caller supplies it —
fact-gathering order and fail-open/fail-closed mappings stay caller policy.
`ComputeAwakeSet` (`cmd/gc/compute_awake_set.go`), the patch constructors in
`lifecycle_transition.go`, and `ProjectLifecycle` are the in-repo precedents.

## Rules for every step

1. Read the `SESSION-*` rows the step touches before changing code.
2. Characterization tests for current behavior exist (or are added) before the
   extraction; they pass identically before and after, at the same SHA.
3. The scenario ledger, code, and tests change in the same PR when behavior
   context moves (`REQUIREMENTS.md` "How To Reconcile").
4. No new YAML inventories, matrices, or manifests. Per-step artifacts only,
   and only when the step itself consumes them.
5. A step that cannot keep behavior identical stops and surfaces the
   difference as a finding instead of shipping it.

## Sequence

### Step 0 — Land the behavior ledger (this PR)

`REQUIREMENTS.md` and `AGENTS.md` move from untracked working-tree files to
committed sources on a current `origin/main` base. Evidence rows verified
against this base; two ledger defects fixed (SESSION-WORK-003 citation,
SESSION-RECON-007 min-floor exemption from PR #3113).

Exit: docs committed; every cited evidence file resolves on this branch.

### Step 1 — Pattern proof: lifecycle-timer decider (this PR)

Extract the idle-timeout and max-session-age decision ladders from
`cmd/gc/session_reconciler.go` into a pure decider in
`internal/session/lifecycle_timers.go`. Lowest-risk decision surface in the
reconciler: small fact set, two well-bounded blocks, 13+ existing
characterization tests covering every branch (holds, quarantine, pending
interaction, assigned-work busy, fail-closed store error, missing anchor, nil
trackers).

The decider owns precedence (blocker > pending > assigned-work > stop), trace
reason/outcome vocabulary, and sleep reasons. The reconciler keeps fact
gathering (trackers, provider probes, store queries), execution (kill, events,
telemetry, patches), and the fail-closed store-error mapping.

Exit: all existing reconciler tests pass unchanged; decider has its own unit
tests; new `SESSION-RECON-008`/`009` ledger rows cite both.

### Step 2 — Stability, churn, and rate-limit predicates (this PR)

Same treatment for the near-pure predicates in `cmd/gc/session_reconcile.go`
(`checkStability`, `recordWakeFailure`, `recordChurn` family): split fused
predicate+write helpers into pure decisions plus caller-applied patches.

Exit: every test in `session_reconcile_test.go` passes unchanged at the same
SHA.

### Step 3 — Fix the real bugs the design review surfaced

Independent bug PRs, not refactor work; each gets its own bead:

- Configured-name hijack: unmaterialized named-session lookup falls through to
  live alias matching (`internal/api/session_resolution.go:240-241` → `:443`),
  letting a rogue live session shadow a reserved configured name.
- `RepairEmptyType` performs store writes inside read paths
  (`internal/session/resolve.go`).
- Huma close constructs a `session.Manager` and calls `CloseDetailed`,
  bypassing the worker boundary; either route it or record it as the
  documented exception it already is in root `AGENTS.md`.
- `session.woke` can emit before the durable commit it reports.
- `session.stranded` carries work IDs only in human-readable message text.

Exit: each bug has a bead with a failing test, then a fix PR.

### Step 4 — Store fence decision (one decision, not ten contracts) (this PR)

Decided in `engdocs/design/session-store-fences.md`: what the beads store
actually provides (no CAS; non-atomic external batches; transactions cannot
read), the two sanctioned fences — city identifier flock
(`internal/session/names.go`, adoption-race fix `b0c53e84c`) and
token-precondition-with-reread (`instance_token` pattern, `4649e7105`,
`ca81d000a`) — and the NDI rules that make the reread-write residual safe
(idempotent re-application, edge-triggered consumption, partial-batch
tolerance). Every mutating extraction cites that document and states its
fence, convergence story, and contended-path test in the PR description.

Exit: document merged; mutating slices (Step 6 on) cite it. When DESIGN.md
lands, its Atomic Command Contract section defers to this document.

### Step 5 — Read-only target classification

The design's Slice 1: a side-effect-free classifier
(`internal/session/target_classifier.go`, `DecideSessionTarget`) owning the
`resolveSessionTargetIDWithContext` precedence, with the API resolver as its
gather/execute adapter. All resolver surfaces — read-only, allow-closed, and
materializing — adopt at once because they share one resolver function; the
design's shared-resolver sequencing rule is satisfied by construction rather
than by anti-drift tests.

Two deliberate scope reductions from the original slice description: the
classifier ships without repair vocabulary — the gather lookups keep the
empty-type normalization behavior of the inline resolver as the parity
baseline until the read-path repair fix (PR #3289) lands and this branch
rebases over it — and ambiguity is not a distinct result kind; ambiguous
lookups surface as the carried step error, preserving existing conflict
projections.

Exit: classifier owns the ladder; wire output is byte-identical under the
existing API tests plus the precedence fixtures
(`internal/api/session_resolution_precedence_test.go`). CLI, mail, and
extmsg surfaces remain on their own paths and follow one at a time.

### Step 6 — First mutating extraction: wake eligibility, then close

Only after Steps 1-5 hold. Wake eligibility decision moves behind the decider
pattern; close/identity-retirement follows as the first command applier,
fenced per the Step 4 ADR, with a writer ledger scoped to exactly the key
families those two operations touch.

Exit: one caller migrated per PR; old writers retired in the same PR or the
coexistence fence named from the ADR.

## Sequencing rationale

Reviews ga-unpr2y attempts 1-17 converged on four durable concerns: parity
proof, mutation fencing, migration sequencing, and boundary ownership. Steps
1-2 prove parity mechanics on decisions with no mutation risk. Step 4 settles
fencing once at the store level. Steps 5-6 then carry the proven pattern into
the contested surfaces with per-step scope. The highest-incident operations
(pending-create/start, drain-ack, config drift) stay in place until the
pattern has survived contact with Steps 1-6; they are explicitly not next.

## Non-goals

Unchanged from the design core: no reconciler rewrite, no `SessionService`
facade, no generic command bus, no event sourcing, no movement of work, mail,
extmsg, pool, or provider policy into `internal/session`.
