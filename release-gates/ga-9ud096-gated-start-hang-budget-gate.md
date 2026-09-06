# Release Gate: gatedStartProvider.waitForStarts hangBudget deflake

Bead: ga-9ud096
Source bead: ga-hp32mm (closed builder bug, PASS review)
Branch under review (provenance only): builder/ga-hp32mm
Reviewed commit: 212cd3010 (pushed)
Deploy branch: deploy/ga-9ud096-gate
Gate SHA: 288dc0de3 (rebased red/green pair on top of origin/main@7a739e29b)
Gate date: 2026-07-26

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria and the repo testing guidance in TESTING.md.

## Background

ga-hp32mm's parent fix covers three sibling helpers on the gated{Start,Stop}Provider
test doubles in `cmd/gc/session_lifecycle_parallel_test.go` that raced under
sharded-host contention with a raw `time.After(3 * time.Second)` literal.
This deploy bead's reviewed slice is scoped to exactly one of the three:
`gatedStartProvider.waitForStarts`.

**Round 1 (stale, superseded).** An earlier gate attempt on local tip
`539839dff` failed criterion 3: the mandatory pre-push `make test-fast-parallel`
hit `TestCmdStopWallClockTimeoutBoundsDirectStop` (cmd_stop_test.go:218,
elapsed 1.65s against a 1s bound) — an unrelated pre-existing flake already
fixed on `origin/main` via the tracked ga-alwb9s scheduling-slack commit
(`25eb009e8`), just not yet in that attempt's base. Per the routed gate-FAIL
instruction, `deploy/ga-9ud096-gate` was rebuilt from current `origin/main`
(`7a739e29b`, which contains `25eb009e8`), reapplying the same red/green pair
as `958dfdf04`/`288dc0de3`. That stale attempt's own gate file (committed
locally at the old `539839dff` tip) was never pushed, per the FAIL rule, and
is superseded by this file.

**Round 2 (this gate, SHA 288dc0de3).** The first full `make test-fast-parallel`
run on the rebased SHA failed a *different*, unrelated test:
`TestFileRecorderConformance/RotationPreservesInvariants`
(`internal/events/eventstest/conformance.go:781`, `context deadline exceeded`
after 11.72s in the `w.Next()` post-rotate read). Root-caused as host CPU
contention, not a product defect, before accepting any retry (TESTING.md
"Flakes are defects" requires classification with attached evidence, not a
silent rerun):

1. This branch's diff is completely disjoint from `internal/events` — it
   touches only `cmd/gc/session_lifecycle_parallel_test.go` (see Commands).
2. Re-running the exact failing subtest in isolation 5x
   (`-run TestFileRecorderConformance -count=5`) passed 5/5, each in ~0.00s
   (vs. 11.72s in the failing run).
3. `pgrep -fal "go test"` at failure time showed a concurrent, independent
   `codex`-based `gascity/deployer` process also running on the same shared
   host — confirmed external contention, not a hypothesis.
4. A fully isolated re-run of the *entire* fast suite (not just the one
   test) on the same SHA passed cleanly end-to-end (see Commands): `EXIT:0`,
   all 9 jobs green.

Filed `ga-84ki3c` to track the underlying fragility (the conformance
harness's watcher wait uses a fixed deadline that isn't scaled for
contended hosts, the same failure shape ga-hp32mm's own fix addresses
elsewhere in this file) — it does not block this deploy. Per TESTING.md,
this gate reports both attempts rather than discarding the first: Round 2's
initial FAIL is a classified infra outage with attached evidence, superseded
by the clean isolated retry on the identical SHA, not a second, different
code change.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-hp32mm review verdict PASS on 212cd3010; deploy bead ga-9ud096 created by gascity/reviewer with that reviewed commit. |
| 2 | Acceptance criteria met | PASS | `gatedStartProvider.waitForStarts` now waits on `hangBudget` instead of a raw `3*time.Second` literal; new proof test `TestGatedStartProviderWaitForStartsSurvivesDelayPastOldFixedDeadline` demonstrates a start signal arriving after the old fixed deadline (but inside `hangBudget`) is still observed. |
| 3 | Tests pass | PASS | `go build ./...`, `go vet ./...`, `gofmt -l` all clean on 288dc0de3. Full `make test-fast-parallel`: Round 2 attempt 1 FAILED on an infra-classified, disjoint-scope flake (evidence above, tracked as ga-84ki3c); isolated full-suite retry on the identical SHA: 9/9 fast jobs passed, `EXIT:0` (see Commands). |
| 4 | No high-severity review findings open | PASS | Single-helper timeout-constant swap plus one new test; no interpolation, no new attack surface; ga-hp32mm review recorded no open findings for this slice. |
| 5 | Final branch is clean | PASS | `git status --short` empty before this gate file was added; this file is committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded, produced tree `91a1ce4494dd5100fda8576ed91c28464be0a9fb`; `git diff --check origin/main...HEAD` reported no conflict markers or whitespace errors. |
| 7 | Single feature theme | PASS | The two commits touch exactly one file, `cmd/gc/session_lifecycle_parallel_test.go` (1 file changed, 25 insertions, 1 deletion) — the `waitForStarts` deflake and its proof test only. |

## Acceptance Checks

- PASS: `gatedStartProvider.waitForStarts` no longer races under sharded-host
  contention with a fixed 3s deadline.
- PASS: `TestGatedStartProviderWaitForStartsSurvivesDelayPastOldFixedDeadline`
  proves the new behavior (survives a delay past the old fixed deadline,
  still inside `hangBudget`).
- PASS: `deploy/ga-9ud096-gate` is built from current `origin/main`
  (`7a739e29b`), so the previously-blocking `TestCmdStopWallClockTimeoutBoundsDirectStop`
  flake fix (`25eb009e8`) is included in this gate SHA.
- PASS: `builder/ga-hp32mm` (provenance branch) was not pushed to or
  otherwise touched by this deploy.
- PASS: The unrelated Round 2 flake was classified with attached evidence
  (disjoint diff, isolated single-test 5/5, confirmed concurrent host
  process, clean isolated full-suite retry) and tracked as ga-84ki3c, not
  silently discarded.

## Commands

```text
git diff --stat origin/main HEAD
go build ./...
go vet ./...
gofmt -l cmd/gc/session_lifecycle_parallel_test.go
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD

# Round 2, attempt 1 (fast-suite, contention-classified FAIL):
LOCAL_TEST_JOBS=16 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast
#   ...
#   --- FAIL: TestFileRecorderConformance (12.24s)
#       --- FAIL: TestFileRecorderConformance/RotationPreservesInvariants (11.72s)
#           conformance.go:781: Next post 0: context deadline exceeded
#   FAIL
#   EXIT:123

# Isolation diagnostic (same SHA, no contention):
go test ./internal/events/ -run TestFileRecorderConformance -v -count=5
#   5/5 PASS, ~0.00s each

# Round 2, attempt 2 (fast-suite, fully isolated retry, same SHA):
LOCAL_TEST_JOBS=16 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast
#   All fast jobs passed
#   EXIT:0
```

All non-test-suite commands above were run directly on gate SHA 288dc0de3.
The fast-suite retry's clean result (9/9 jobs, `EXIT:0`) is the evidence
cited for gate criterion 3.

## Round 3 (maintainer test-bound)

Rounds 1 and 2 above are retained as written; this section appends the
changes that landed after them rather than restating them.

**What changed.** After Round 2, the proof test
`TestGatedStartProviderWaitForStartsSurvivesDelayPastOldFixedDeadline` was
rewritten to run inside a `testing/synctest` bubble (`179c91bb`, "test(cmd/gc):
run waitForStarts hangBudget fence under synctest"), and `t.Parallel()` was
dropped — a bubbled test may not be parallel. A subsequent maintainer
test-only follow-up gives the bubble's sender goroutine a
`context.WithCancel` escape, so that on a real regression `waitForStarts`'s
`t.Fatalf` exits the bubble root and the parked sender returns instead of
leaving the bubble with a blocked goroutine (which `synctest` reports as
`panic: deadlock`, aborting the rest of the package binary and taking that
shard's other results with it). The green path is unchanged.

**Why `synctest`.** As originally written the fence slept a real 4s to step
past the old 3s literal. TESTING.md (§ hang budgets) is explicit that a
safety deadline must not set a normal test's duration: these waits "return the
instant their condition is met, so raising the budget does not slow a passing
run." A fence that spends 4s of real wall-clock to prove a 60s budget is in
force violates that in the other direction. Under `synctest` the 4s is fake
time, so the test proves the same property in ~0s real. Confirmed still
load-bearing: restoring `time.After(3 * time.Second)` in `waitForStarts` turns
the test red (`timed out waiting for 1 starts, got []`), so the bubble did not
retire the fence.

**Criterion 3 evidence, corrected.** The Round 2 local
`test-local-parallel fast` run cited above was made on `288dc0de3`, which
predates the `synctest` rewrite and is therefore stale as evidence for what
merges. It is superseded by green GitHub CI on `179c91bb`. The maintainer
follow-up on top of that SHA is test-only and was verified with:

```text
gofmt -l cmd/gc/session_lifecycle_parallel_test.go
go vet ./cmd/gc/
go test ./cmd/gc/ -run 'TestGatedStartProviderWaitForStartsSurvivesDelayPastOldFixedDeadline|TestHangBudget' -count=2
```

**Criterion 7, corrected.** The Round 2 table asserts the branch "touch[es]
exactly one file … 1 file changed, 25 insertions, 1 deletion." That was true
of the original red/green pair and is no longer true of the branch: it now
touches **two** files — `cmd/gc/session_lifecycle_parallel_test.go` (the
`waitForStarts` deflake, its proof test, and the `synctest` wrap) and this
gate document. The single-feature-theme finding still holds; only the file
count was wrong. No production code is touched by any commit on this branch.
