# Release gate: session wedges in 'creating' forever when Runtime.Observed=false

**Deploy bead:** `ga-5hdwl6`
**Build bead:** `ga-uco1ol`
**Review bead:** `ga-uco1ol` (same bead — no separate review bead was created; the
review verdict is recorded directly in this bead's own notes)
**Reviewed commit:** `df22e72dcbddb10713a39f400d8972d7f815c4c1`
**Round 1 retried commit:** `eb87748d3c30e55f8edca958a91a435185f5c838` (superseded — see Round 2)
**Round 2 commits:** `0fcb4406cb0aa517a7631a9cc538f6bb19303ab9`
(red), `8eb3b4a11f3b65a7ff57f5fb0f016d1beb42964d` (green) — CLI age-gate rework
(superseded at the shipped head — see Round 3)
**Round 3 head (shipped):** `cad296949ca2dd583ad7ff0a14df8006bdb0b56d`
**Base checked:** `origin/main` at `679e6e46316aa50226ecf58e4f2df739dabcaf21`
(round 1 rebase target); round 2 re-rebased onto `08bba7a3a63faaece48cf88976e11c51727fb4e6`;
round 3 merged `origin/main` twice, most recently at
`3db4ed265db02a411b081e4581b435d80d4a5311` (see Round 3 section below)
**Isolated branch:** `builder/ga-5hdwl6-session-creating-wedge`
**Round 1 verdict:** **PASS** (superseded by mayor REQUEST-CHANGES on PR #4772 —
see Round 2)
**Round 2 verdict:** **TECHNICAL GATE PASS** at `8eb3b4a11`; its two code-review
criteria (1, 4) were open at the time that section was written and are now
resolved — the diff was reviewed on PR #4772 and merged. Round 2's technical
criteria (build/vet/tests/clean branch/clean divergence) were
builder-self-certified, as in round 1, and its evidence describes the
`8eb3b4a11` head, not the shipped one.
**Round 3 verdict:** **PASS** at `cad296949`. The CLI age-gate rework was
reviewed on PR #4772 and merged; see the Round 3 section for what changed after
`8eb3b4a11` and for the evidence that stands at the shipped head.

## Why this is a retry (round 1)

`ga-5hdwl6`'s prior gate attempt FAILed on `make test-fast-parallel` due to
`TestCityRuntimeReloadDrainBoundedByTimeout` flaking under shard-parallel host
load — a pre-existing, unrelated timing issue tracked separately in
`ga-ajmj0y`, not a defect in this diff. That flake's fix (PR #4730, commit
`7a5bdeeee5c240663964916cea4c8f72dd91c1f4`, widening CI-jitter tolerance from
500ms to 3s) merged to `origin/main` at `2026-07-27T23:33:13Z`, confirmed via
`gh pr view 4730 --json state,mergedAt,mergeCommit`. This gate rebases the
reviewed commit onto a main that includes that fix and re-verifies from
scratch.

## Round 1 gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-uco1ol`'s notes contain `VERDICT: PASS` from a full independent re-review dated 2026-07-28, reviewed at `df22e72d`. `git patch-id --stable` on `origin/builder/ga-uco1ol~2..origin/builder/ga-uco1ol` (the reviewed range) and on this branch's `HEAD~2..HEAD` (the rebased range) both produce `42a9356eedb69bf3304a3c0ba11988280e0389b8` — the diff content is byte-identical, only the base commit changed. The PASS verdict transfers unchanged. |
| 2 | Acceptance criteria met | PASS | Per `ga-uco1ol`'s SPEC COMPLIANCE finding: `projectRuntimeProjection` (`internal/session/lifecycle_projection.go`) no longer returns `RuntimeProjectionUnknown` unconditionally when `Runtime.Observed` is false — it now checks `creatingUnobservedExpired` first when `base == BaseStateCreating`, closing the short-circuit-before-staleness-heal gap. `creatingStateIsStale`'s `StaleCreatingAfter<=0` branch (the second gap, on the wake path) now falls back to `creatingUnobservedExpired` instead of hardcoding false. `cmd_session_wake.go`'s new switch arm makes `gc session wake` fail loudly instead of silently no-oping on a wedged session. The GREEN-phase zero-`Now` regression guard (`creatingUnobservedExpired` treats a zero `input.Now` as "cannot assess", not `time.Now()`) is present. |
| 3 | Tests pass | PASS | See Test evidence below. Covers unit build/vet, the feature's own new tests, the originally-flaky test in isolation, the full fast-parallel lane, a skip-hazard double-check on the CI-required `cmd_gc_process` filter's `TestTutorial01` path (`GC_FAST_UNIT=0`), and the full `make test-cmd-gc-process-parallel` run — the last of these was explicitly *not* run by the original reviewer (judged a narrower scoped double-check proportionate instead); this retry runs it in full per `engdocs/contributors/release-gate-criteria-conventions.md`. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings in `ga-uco1ol`'s OWASP-style security walk. One non-blocking finding: `cmd_session_wake.go`'s new loud-failure path (`sessionWakeStuckInFlightInfo` switch arm) has no direct test coverage; tracked separately as `ga-feuu02` (P3, non-blocking fast-follow), not required for this gate. |
| 5 | Final branch is clean | PASS | `git status --porcelain` is empty after committing this gate file on `builder/ga-5hdwl6-session-creating-wedge`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded with tree `57d58705330f2b4323f61ab90a885f0190da003c` against current `origin/main` tip `5fd0545f0` (one unrelated commit ahead: a push-ownership-guard deploy-gate fix, `ga-anwmtr` / #4761). No conflicts; no further rebase needed. |
| 7 | Single feature theme | PASS | Two commits: RED (failing repro test) then GREEN (fix) for one session-lifecycle defect. Patch-id-identical to the originally reviewed diff (criterion 1), so this holds unchanged from review: `cmd/gc/cmd_session_wake.go`, `internal/session/lifecycle_projection.go`, `internal/session/wedge_repro_test.go` — no unrelated changes. |

## Round 1 reviewed history

```text
eb87748d3 feat: green — Fix: sessions wedge in 'creating' forever when Runtime.Observed=false short-circuits the staleness heal (refs ga-uco1ol)
3504c2362 test(feat): red — Fix: sessions wedge in 'creating' forever when Runtime.Observed=false short-circuits the staleness heal (refs ga-uco1ol)
679e6e463 test(gc): migrate controller hang deadlines to shared wait helpers (#4745)
```

The commit set touches three files: `cmd/gc/cmd_session_wake.go`,
`internal/session/lifecycle_projection.go`, and the new
`internal/session/wedge_repro_test.go`. It does not change configuration,
HTTP/API schemas, generated assets, dashboard code, or CI workflow files.

## Round 1 test evidence

```text
go build ./...
PASS

go vet ./...
PASS

go test ./internal/session/... -run 'TestCreatingWedgesWhenRuntimeUnobserved|TestCreatingStaleDetectionIgnoresPendingCreateClaim|TestStaleCreatingAfterUnsetNeverGoesStale' -v -count=1
--- PASS: TestCreatingWedgesWhenRuntimeUnobserved (...)
    --- PASS: .../observed_dead_runtime_heals_to_asleep
    --- PASS: .../unobserved_runtime_stays_creating_forever
    --- PASS: .../young_unobserved_runtime_still_projects_creating
--- PASS: TestCreatingStaleDetectionIgnoresPendingCreateClaim (...)
--- PASS: TestStaleCreatingAfterUnsetNeverGoesStale (...)
PASS

go test ./internal/session/... -run 'TestCityRuntimeReloadDrainBoundedByTimeout' -count=4 -v
PASS (4/4 reps, 1.1-1.13s each, no timing variance)

make test-fast-parallel
Running 9 fast job(s) with LOCAL_TEST_JOBS=16
[fsys-darwin-compile] ok
[push-gate-lock-selftest] ok
[unit-core] ok
[unit-cmd-gc-1-of-6] ok   <- shard containing the originally-flaky test
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed
EXIT:0

go test ./cmd/gc/... -run 'Wake|Creating' -v -count=1        (GC_FAST_UNIT unset)
262 PASS, 0 FAIL, 0 SKIP

GC_FAST_UNIT=0 go test ./cmd/gc/... -run 'Wake|Creating' -v -count=1
262 PASS, 0 FAIL, 0 SKIP

make test-cmd-gc-process-parallel        (GC_FAST_UNIT=0, includes TestTutorial01)
Running 7 cmd-gc-process job(s) with LOCAL_TEST_JOBS=14
[cmd-gc-process-1-of-6] ok
[cmd-gc-process-2-of-6] ok
[cmd-gc-process-3-of-6] ok
[cmd-gc-process-4-of-6] ok
[cmd-gc-process-5-of-6] ok
[cmd-gc-process-6-of-6] ok
[productmetrics-testhook] ok
All cmd-gc-process jobs passed
EXIT:0
```

The unset-vs-`GC_FAST_UNIT=0` comparison on the `Wake|Creating` scope
reproduces the reviewer's own skip-hazard double-check (precedent
`ga-7jmqyx`/`ga-5bjrbd`) fresh on the rebased commit: identical PASS/FAIL/SKIP
counts either way confirm no test in that scope is silently skipped. The
`make test-cmd-gc-process-parallel` run additionally exercises `TestTutorial01`
under the real CI-required `cmd_gc_process` filter conditions — the coverage
gap `engdocs/contributors/release-gate-criteria-conventions.md` was written to
close, and one the original review explicitly chose not to run (judging the
narrower double-check proportionate). Both now pass on this diff.

## Why round 2: mayor REQUEST-CHANGES on PR #4772

Mayor reviewed PR #4772 directly in the checkout (2026-07-28) and returned
**DO NOT DEPLOY**, routing `ga-5hdwl6` back to builder. Full verdict recorded
verbatim on `ga-5hdwl6`'s comment thread. Three findings:

1. The projection fix (round 1) targets a mechanism that cannot occur in
   production: both production `RuntimeFacts{...}` construction sites
   (`cmd/gc/session_reconcile.go:887`, `cmd/gc/session_sleep.go:144-145`)
   hardcode `Observed: true`, so the new `!input.Runtime.Observed` branch in
   `projectRuntimeProjection` is unreachable from production, and every new
   test drives `Runtime{Observed: false}` — an input shape no production
   caller constructs. Mayor judged this **harmless defense-in-depth**, not a
   blocker: "The projection half is harmless defense-in-depth and can stay."
2. Round 1's CLI change (`cmd_session_wake.go`'s new switch arm) is a
   **confirmed regression**: `gc session wake` exits 1 on a *healthy*
   in-flight create, with no age check and no runtime probe, worsened by
   `sessionWakeHasRunnableTemplateInfo` returning `true` whenever
   `cfg == nil`. Zero test coverage existed for the new arm. **This is the
   blocking finding this round's commits address.**
3. The real root cause of the original wedge (`ga-pofwv9`) is still
   unconfirmed; the most plausible candidate — `PendingCreateClaim &&
   LastWokeAt == ""` racing ahead of any staleness check on the
   `Observed: true` path — was not actually excluded by
   `TestCreatingStaleDetectionIgnoresPendingCreateClaim`, which only drives
   `Observed: false` and returns before that branch is ever reached. Recorded
   on `ga-uco1ol` (the parent investigation bead) as an informational
   cross-reference; explicitly out of scope for this rework — mayor's WHAT TO
   DO list does not ask for root-cause work here, and item 1 above directs
   leaving the projection half untouched.

Mayor's WHAT TO DO list (verbatim, four items) and this round's disposition:

| Item | Instruction | Disposition |
|------|-------------|-------------|
| 1 | Age-gate the CLI arm using the *existing* `isStaleCreatingInfo` helper (`cmd/gc/city_runtime.go:2927`), not a new one — "keeps the CLI and the sweep agreeing" | Done: `isStaleCreatingInfo(res.Info)` added as a third conjunct on the `hasRunnableTemplate && sessionWakeStuckInFlightInfo(...)` switch case. `sessionWakeStuckInFlightInfo` itself is untouched. |
| 2 | Add `cmd_session_wake_test.go` coverage for the new arm, landing *with* this change, not deferred | Done: `TestDoSessionWake_StuckInFlightAgeGate`, 5 subtests at the shipped head (fresh creating/start-pending wake normally; stale creating/start-pending reject; leased never-started create wakes normally). Four of those landed in this round at `8eb3b4a11`; the fifth, plus the mirror test `TestDoSessionWake_NoRunnableTemplateAgeGate`, landed in round 3 — see the Round 3 section. `ga-feuu02` (the P3 bead that had deferred this) closed as done-elsewhere. |
| 3 | Reword the failure message to state only what was checked — "has been in state X since TIMESTAMP without completing its create" — not "no live runtime", which the path never probes | Done: message now reads `session %s has been in state %q since %s without completing its create`, sourced from new helper `stuckCreatingSinceInfo` (mirrors `isStaleCreatingInfo`'s anchor preference — `PendingCreateStartedAt` else `CreatedAt` — purely to pick what to print, not to re-decide staleness). |
| 4 | The projection half is harmless defense-in-depth and can stay | Done (by omission): `internal/session/lifecycle_projection.go` is untouched this round. |

## Round 2 gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS (resolved in round 3) | The patch-id-identical projection-fix portion retains `ga-uco1ol`'s round-1 PASS (verified unchanged below). The CLI age-gate rework (`0fcb4406c`, `8eb3b4a11`) was unreviewed when this section was written; it was subsequently reviewed on PR #4772 and merged, together with the round-3 commits listed below. |
| 2 | Acceptance criteria met | PASS | All four WHAT TO DO items addressed — see table above. |
| 3 | Tests pass | PASS | See Round 2 test evidence below: build/vet, the new test in isolation, full `cmd/gc`+`internal/session` suite, `Wake\|Creating` scope with and without `GC_FAST_UNIT=0` (skip-hazard double-check), `make test-fast-parallel` (10/10), `make test-cmd-gc-process-parallel` (7/7, includes `TestTutorial01`). |
| 4 | No high-severity review findings open | PASS (resolved in round 3) | Mayor's finding 2 (the blocking one) is addressed this round. Finding 3 (root-cause uncertainty) is informational, tracked on `ga-uco1ol`, explicitly out of scope per mayor's own item 4. No new HIGH findings self-identified. The round-3 review on PR #4772 opened no high-severity findings either; the production wedge root cause remains open under `ga-pofwv9.1`. |
| 5 | Final branch is clean | PASS | `git status --porcelain` empty after each commit on `builder/ga-5hdwl6-session-creating-wedge`. |
| 6 | Branch diverges cleanly from main | PASS | Rebased onto `origin/main` @ `08bba7a3a63faaece48cf88976e11c51727fb4e6` with zero conflicts (`Successfully rebased and updated refs/heads/builder/ga-5hdwl6-session-creating-wedge`). `git merge-tree --write-tree origin/main HEAD` succeeds with tree `5445643ced265e6cdccb26e3068de9ceafaed0ed`. |
| 7 | Single feature theme | PASS | Two new commits, RED then GREEN, touching exactly `cmd/gc/cmd_session_wake.go` and `cmd/gc/cmd_session_wake_test.go` — the CLI-arm regression mayor flagged, nothing else. The carried-forward round-1 diff remains patch-id-identical (criterion below), so no unrelated drift was introduced by the rebase. |

**Patch-id continuity check:** `git patch-id --stable` on the round-1
reviewed range (`2b191d9aa~1..7bb30ddf8` post-rebase) reproduces
`42a9356eedb69bf3304a3c0ba11988280e0389b8` — identical to the value recorded
in this file's criterion 1 (round 1) before this round's rebase. Only the
base commit changed; the reviewed content did not.

## Round 2 history

```text
8eb3b4a11 fix: green — age-gate gc session wake's stuck-in-flight rejection via isStaleCreatingInfo (refs ga-5hdwl6)
0fcb4406c test(fix): red — gc session wake exits 1 on a healthy in-flight create with no age check (refs ga-5hdwl6)
74505eb6e docs(release-gates): add retry gate evidence for ga-5hdwl6
7bb30ddf8 feat: green — Fix: sessions wedge in 'creating' forever when Runtime.Observed=false short-circuits the staleness heal (refs ga-uco1ol)
2b191d9aa test(feat): red — Fix: sessions wedge in 'creating' forever when Runtime.Observed=false short-circuits the staleness heal (refs ga-uco1ol)
08bba7a3a fix(paths): route discovery + store-scope through the single path normalizer (#4695)
```

Two new commits on top of the unchanged round-1 pair, touching only
`cmd/gc/cmd_session_wake.go` and `cmd/gc/cmd_session_wake_test.go`. No
configuration, HTTP/API schema, generated asset, dashboard, or CI workflow
files touched.

## Round 2 test evidence

```text
go build ./...
BUILD OK

go vet ./...
VET OK

go test ./cmd/gc/... -run 'TestDoSessionWake_StuckInFlightAgeGate' -v -count=1
--- PASS: TestDoSessionWake_StuckInFlightAgeGate (0.00s)
    --- PASS: .../fresh_creating_wakes_normally (0.00s)
    --- PASS: .../fresh_start-pending_wakes_normally (0.00s)
    --- PASS: .../stale_creating_rejects_wake (0.00s)
    --- PASS: .../stale_start-pending_rejects_wake (0.00s)
PASS
ok  	github.com/gastownhall/gascity/cmd/gc	0.252s

go test ./cmd/gc/... ./internal/session/... -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	329.384s
ok  	github.com/gastownhall/gascity/internal/session	0.680s
ok  	github.com/gastownhall/gascity/internal/session/sessiontest	0.019s

go test ./cmd/gc/... -run 'Wake|Creating' -v -count=1        (GC_FAST_UNIT unset)
263 PASS, 0 FAIL, 0 SKIP

GC_FAST_UNIT=0 go test ./cmd/gc/... -run 'Wake|Creating' -v -count=1
263 PASS, 0 FAIL, 0 SKIP

make test-fast-parallel
Running 10 fast job(s) with LOCAL_TEST_JOBS=15 inner_p=1
[fsys-darwin-compile] ok
[unit-core] ok
[push-gate-lock-selftest] ok
[local-concurrency-selftest] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed
EXIT:0

make test-cmd-gc-process-parallel        (includes TestTutorial01)
Running 7 cmd-gc-process job(s) with LOCAL_TEST_JOBS=2 inner_p=1
[cmd-gc-process-1-of-6] ok
[cmd-gc-process-2-of-6] ok
[cmd-gc-process-3-of-6] ok
[cmd-gc-process-4-of-6] ok
[cmd-gc-process-5-of-6] ok
[cmd-gc-process-6-of-6] ok
[productmetrics-testhook] ok
All cmd-gc-process jobs passed
EXIT:0
```

263 vs round 1's 262 on the `Wake\|Creating` scope reflects exactly the one
new top-level test (`TestDoSessionWake_StuckInFlightAgeGate`); its four
subtests are indented `--- PASS` lines and are not double-counted by the
`^--- (PASS|FAIL|SKIP)` anchor used for this count, consistent with round 1's
methodology. Identical counts with `GC_FAST_UNIT` unset vs `=0` again confirm
no skip-hazard in this scope. `local-concurrency-selftest` in the fast-parallel
output is a job added to `main` since round 1 (unrelated to this diff) — not
a regression in this branch.

All of the above was measured at `8eb3b4a11`. None of it was re-run at the
shipped head — see Round 3 for the evidence that stands there.

## Round 3: what shipped

The head this gate actually covers is
`cad296949ca2dd583ad7ff0a14df8006bdb0b56d`, not `8eb3b4a11`. The sections above
describe the round-2 candidate; this section records everything added on top of
it. Nothing in rounds 1-2 was reverted — the round-3 commits tighten the CLI
arms and extend their coverage.

### Round 3 history

```text
cad296949 fix(session): say the wake was recorded on the stuck-in-flight reject
cc96e759c fix(session): gate gc session wake's create arms on the pending-create lease
77afdfc9b Merge remote-tracking branch 'origin/main' (3db4ed265)
8ed9f21af fix(lint): spell `canceled` the way misspell wants in session-wake
10fa1122c fix(session): withdraw queued wait nudges on the stuck-in-flight wake reject
66548ecbc fix(session): age-gate the no-runnable-template arm of gc session wake
2a30cda3d Merge remote-tracking branch 'origin/main' (ad4d0ab4a)
8325bafca docs(release-gates): add round 2 gate evidence for ga-5hdwl6 (mayor rework)
8eb3b4a11 fix: green — age-gate gc session wake's stuck-in-flight rejection via isStaleCreatingInfo
```

Still only the same four files plus this gate doc:
`cmd/gc/cmd_session_wake.go`, `cmd/gc/cmd_session_wake_test.go`,
`internal/session/lifecycle_projection.go`,
`internal/session/wedge_repro_test.go`. No configuration, HTTP/API schema,
generated asset, dashboard, or CI workflow files touched.

### Round 3 changes

1. **Pending-create lease checked before staleness** (`cc96e759c`). Round 2
   gated the CLI arms on `isStaleCreatingInfo` alone. That rejects a create the
   reconciler still protects, because `pendingCreateNeverStartedTimeout` (10m)
   is deliberately longer than `staleCreatingStateTimeout` (1m). The gate is now
   factored into `sessionWakeCreateAbandonedInfo(info, startupTimeout)`, whose
   conjuncts run in the sweep's own order — lease first via
   `!pendingCreateClaimStillLeasedForSweepInfo(info, startupTimeout)`, staleness
   second — mirroring `cmd/gc/city_runtime.go:2853-2857`. `startupTimeout` comes
   from `deps.cfg.Session.StartupTimeoutDuration()`, or `0` when `cfg == nil`,
   which the lease helper clamps to the 1-minute config default.
2. **`leased never-started create wakes normally`** — a fifth subtest on
   `TestDoSessionWake_StuckInFlightAgeGate`, pinning exactly the window a
   staleness-only gate got wrong: past the 1-minute staleness bound, still
   inside the 10-minute never-started lease, no `last_woke_at`. The sweep skips
   that bead; the CLI now agrees.
3. **`TestDoSessionWake_NoRunnableTemplateAgeGate`** (`66548ecbc`, 5 subtests) —
   the mirror of the above on the *mutating* arm. The no-runnable-template arm
   clears `pending_create_claim`/`pending_create_started_at`, so an ungated
   version would yank the lease out from under a create a provider had just
   legitimately started. Subtests: fresh creating / fresh start-pending / leased
   never-started all keep their lease; stale creating / stale start-pending heal
   to asleep.
4. **Queued wait nudges withdrawn on reject** (`10fa1122c`). The reject arm now
   sets a `rejectStuck` flag and defers `return 1` until after the
   withdraw/poke block. `WakeSession` has already canceled the waits by the time
   the arm is reached, so an early return stranded their nudges in the queue
   with nothing left to withdraw them. Every subtest, including the rejecting
   ones, asserts `withdrawQueuedWaitNudges` fired.
5. **Reject message says the wake was recorded** (`cad296949`). `WakeSession`
   commits `wake_request=explicit` / `wake_requested_at` before this arm runs,
   so the message now reads "the wake request was recorded but cannot complete
   now" rather than implying nothing happened, and the test asserts the arm does
   not roll that record back.
6. **Lint** (`8ed9f21af`) — `cancelled` → `canceled` in a comment, for
   `misspell`. No behavior change.

### Round 3 gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Reviewed on PR #4772 and merged. The review covered the shipped head `cad296949`, including the round-3 commits above; it re-derived the lease-before-staleness ordering against `city_runtime.go:2853-2857` and confirmed the `cfg == nil` → `startupTimeout = 0` path is benign. This supersedes the **PARTIAL** entries in the round-2 table. |
| 2 | Acceptance criteria met | PASS | Mayor's four WHAT TO DO items remain addressed (round-2 table above), and the round-3 commits close the ordering gap that table's item 1 left open. |
| 3 | Tests pass | PASS | Green CI on `cad2969` — see Round 3 test evidence. Round 2's local sweeps were **not** re-run at this head. |
| 4 | No high-severity review findings open | PASS | The PR #4772 review opened no high-severity findings. Two acknowledged non-blocking gaps and the still-open root cause are recorded under "What this does NOT establish" below. |
| 5 | Final branch is clean | PASS | `git status --porcelain` empty at `cad296949`. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` was merged into the branch twice this round (`2a30cda3d`, `77afdfc9b`), most recently at `3db4ed265`, which is the current merge base. No conflicts. |
| 7 | Single feature theme | PASS | Same four source files as rounds 1-2 plus this gate doc; every round-3 commit is CLI-arm gating, its coverage, or lint on the same file. |

### Round 3 test evidence

```text
GitHub Actions run 30962072171, head cad2969 — GREEN
  https://github.com/gastownhall/gascity/actions/runs/30962072171
  8 workflows, ~50 executed check-runs green, including:
    - 12 cmd/gc process shards
    - integration
    - acceptance
    - CI / required
  CodeQL: 0 open alerts on the merge ref
  docs render: not applicable (no docs/ change)
```

**Not re-run at this head:** the local `make test-fast-parallel` and
`make test-cmd-gc-process-parallel` sweeps, and the `GC_FAST_UNIT` skip-hazard
double-check. Those numbers appear in the Round 1 and Round 2 evidence blocks
and belong to `eb87748d3` and `8eb3b4a11` respectively; they are **not**
evidence for `cad296949`. The CI run above is what covers the shipped head, and
it exercises the same `cmd_gc_process` shards and `TestTutorial01` path those
local targets do.

### What this does NOT establish (unchanged at this head)

- **The production wedge root cause is still open**, tracked under
  `ga-pofwv9.1`. Nothing in rounds 1-3 confirms the mechanism that produced the
  original 12-day `creating` bead.
- **The projection half remains inert defense-in-depth at this head.** Both
  non-test `RuntimeFacts{...}` construction sites set `Observed: true`, and the
  sole consumer of the fields the new `!input.Runtime.Observed` branch sets
  (`cmd/gc/session_reconcile.go`) sets `Observed: true` as well. The
  `internal/session` tests therefore drive an input shape no production caller
  constructs: they are legitimate contract pins for a future consumer that
  builds a projection without runtime facts, and they demonstrate nothing about
  the reported wedge. This is the disposition mayor already accepted in round 2
  ("harmless defense-in-depth and can stay"), restated here because it is still
  true of what shipped.
- **The start-in-flight lease branch is untested end to end.** No subtest drives
  `pending_create_claim=true` *with* `last_woke_at` set — the only branch where
  `startupTimeout` participates — so the `cfg → StartupTimeoutDuration()`
  plumbing added this round is exercised only through its never-started arm.
  Non-blocking.
- **`isStaleCreatingInfo` reads `time.Now()` directly**, bypassing the
  injectable `deps.now`; the round-3 tests use real wall-clock offsets to work
  with it. Pre-existing, shared with the sweep, out of scope here.
