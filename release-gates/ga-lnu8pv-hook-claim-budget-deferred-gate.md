# Release gate: hook claim budget deferral

- Deploy bead: `ga-lnu8pv`
- Implementation bead: `ga-818ery`
- Review bead: `ga-ij4q4y`
- Originally reviewed source: `406500790b05ddfa90216bd8f87b671fd818991e`
- Reviewed source at this gate (rebase, content-identical diff): `35e26dee2143aac0fe3adfedcd607ceb2eb032a3`
- Source branch: `deploy/ga-lnu8pv-gate` (cut from the reviewed commit, rebased onto current `origin/main`; `builder/ga-818ery` is provenance-only and is not the PR source)
- Base checked at gate time: `origin/main@bf787a119f3f3c8387905d3716291df3df9c4339`
- Gate result: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from this revision. This checklist applies
the seven release criteria in the deployer protocol and the implementation
bead's recorded exit contract.

## Prior gate history (context, not re-litigated)

The reviewed source (`406500790b`) was first gated on 2026-09-01/02 and
**FAILed** on criterion 3/3a — see
`release-gates/ga-lnu8pv-hook-claim-budget-deferred-gate.md` at commit
`4c0b0764ba` on branch `builder/ga-818ery` (left untouched; that file is not
an ancestor of this branch). `make test-local-full-parallel` failed in
`cmd-gc-process-3-of-6`: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
failed with the beads#4566 signature (`schema migration: pre-existing dirty
tables changed during schema migration: dolt_schemas`) during `bd init`
fixture bootstrap. Criterion 3a refused to attribute/waive because the
failing test shares a package (`cmd/gc`) with the diff.

Mayor standing authorization `ga-6bnc42` (2026-08-18) permits a
builder/deployer to proceed past this exact FAIL signature without a fresh
ask, given: (a) signature match, (b) no plausible Dolt-migration/bootstrap
mechanism in the diff, (c) the occurrence logged with bead/build/test ids,
and (d) the record preserves the FAIL as WAIVED rather than rewritten green.
All of (a)-(c) were independently satisfied and logged on the sighting-log
bead `ga-vkhfnj` before this rebase: signature matched exactly, the diff
(`cmd/gc/cmd_hook_claim.go`, `cmd/gc/cmd_hook_claim_budget_deferred_test.go`,
`internal/beadmeta/keys.go`) is a pure in-memory metadata filter with no
Dolt/schema/bootstrap code path, and an isolated `-count=3` rerun of the
failing test passed clean 3/3 that day. Under (d), this waiver would have
applied to a re-gate of the *original* FAIL record.

That waiver was not needed here: gating this rebased branch required a fresh
full-suite run regardless (a new HEAD, and the criteria-conventions doc's
"Tests pass" standard requires the actual CI-required job, not a targeted
rerun), and that fresh run came back **completely clean** — see criterion 3
below, including a clean pass of the previously-flaky test itself. Per (d),
the original FAIL record is left untouched as the historical evidence; this
is a distinct, later, independently-executed test run on a different
(rebased) commit, and it is reported honestly as the PASS it actually was,
not force-labeled "FAIL — WAIVED." The non-reproduction is itself a useful
data point for the upstream beads#4566 investigation: it is consistent with
a load-dependent race that manifests under concurrent full-suite/shard
bootstrap contention, not a deterministic regression from this diff.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ij4q4y` records corrective `review_round: 2` with `verdict: pass` for the reviewed source. It independently reports clean style, security, specification, vet, and eight diff-owned tests. Round 1's contradictory request-changes verdict is explicitly corrected in the same review record. The rebase changes no reviewed line: `git show HEAD` / `git show HEAD~1` on this branch reproduce the exact reviewed diff in `cmd/gc/cmd_hook_claim.go`, `cmd/gc/cmd_hook_claim_budget_deferred_test.go`, and `internal/beadmeta/keys.go`, content-identical to what `ga-ij4q4y` reviewed. |
| 2 | Acceptance criteria met | **PASS** | The diff checks `gc.budget_deferred_until` at both claim paths using one injected clock: ready assignments are skipped before promotion, and fresh route-matched candidates are rejected by `hookCandidateClaimable`. RFC3339 future values defer; absent, past, and malformed values fail open. The change reads already-hydrated bead metadata and adds no query or schema path. |
| 3 | Tests pass | **PASS** | `make test-cmd-gc-process` (the actual CI-required job for `cmd/gc/**`/`internal/**` changes per `engdocs/contributors/release-gate-criteria-conventions.md`, `GC_FAST_UNIT=0`) run clean on this branch's HEAD (`35e26dee21`). Full log: `ga-lnu8pv-rebased-full-cmdgcprocess.log` (19,326 lines). Zero `--- FAIL` anywhere; event-tag counts 187 `pass` / 19116 `run` / 12 `skip` / 0 `fail`. `TestTutorial01` ran with 15+ subtests and passed (top-level `pass TestTutorial01`). `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` ran once and passed (~9.47s) — the previously-recorded flake did not reproduce. The chained sub-target `test-productmetrics-testhook` also ran and passed. All 8 diff-owned tests additionally re-confirmed individually via targeted `-run`/`-v` rerun (1.164s) and the flaky test alone re-confirmed 3/3 clean in isolation (log: `ga-lnu8pv-rebased-flaketest.log`, 9.99s/7.86s/9.29s). |
| 3a | Failure attribution | **PASS / n/a** | No failure occurred on this run; no attribution or waiver is being invoked. (Context: had the historical FAIL signature reproduced here, it would have qualified for the `ga-6bnc42` waiver per conditions a-c above — see "Prior gate history.") |
| 3b | Policy/lint lane | **PASS** | `go vet ./...` clean. `make lint-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main` (scope override required — default `worktree` scope diffs only uncommitted changes against `HEAD`, which is empty on a clean, fully-committed branch) reports `0 issues`, scoped to exactly `./cmd/gc ./internal/beadmeta`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, matrix, timeout, required-check list, or other CI configuration changed. |
| 4 | No unresolved HIGH review findings | **PASS** | The review record reports no style, security, specification, or coverage blocker and no HIGH finding. Unchanged by the rebase — no reviewed line differs. |
| 5 | Final branch clean | **PASS** | `git status --porcelain=v1` is empty on `deploy/ga-lnu8pv-gate` at HEAD `35e26dee2143aac0fe3adfedcd607ceb2eb032a3`. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated after a fresh fetch. `git merge-base --is-ancestor origin/main HEAD` confirms `origin/main` (`bf787a119f3f3c8387905d3716291df3df9c4339`) is an ancestor of HEAD. `git rev-list --count origin/main..HEAD` reports 2 commits ahead. `git merge-tree --write-tree --messages origin/main HEAD` exited 0 with tree `ac46e841e8f2d5be253ea8fc3b58037d529d67dd` and zero conflict messages. |
| 7 | Single feature theme | **PASS** | The two commits touch only hook-claim budget-deferral filtering and its centralized metadata key/test coverage: `cmd/gc/cmd_hook_claim.go`, `cmd/gc/cmd_hook_claim_budget_deferred_test.go`, and `internal/beadmeta/keys.go`. Unchanged by the rebase. |

## Test evidence

- `test_cmd`: `make test-cmd-gc-process`
- `test_cmd_scope`: `cmd/gc/**`, `internal/**` required-job lane (`GC_FAST_UNIT=0`)
- `test_counts`: 0 FAIL / 187 PASS-tagged events / 12 SKIP across the full job (main `test-cmd-gc-process` sub-invocation plus chained `test-productmetrics-testhook`)
- `full_log`: `ga-lnu8pv-rebased-full-cmdgcprocess.log` (scratchpad)
- `diff_owned_log`: `ga-lnu8pv-rebased-diffowned-tests.log` (scratchpad) — all 8 diff-owned tests, targeted rerun, 1.164s
- `flake_isolation_log`: `ga-lnu8pv-rebased-flaketest.log` (scratchpad) — `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` alone, `-count=3`, 3/3 PASS
- `failure`: none on this run
- `failure_attribution`: n/a — no failure to attribute
- `waiver_ref`: not invoked (this run passed clean); prior FAIL waiver-eligibility documented above for the record only
- `ci_lane_run`: n/a — no CI-config change
- `lint_lane`: `go vet ./...` clean; `make lint-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main` → `0 issues` (`./cmd/gc ./internal/beadmeta`)
- `diff_tests_executed`: `TestHookCandidateBudgetDeferredFutureTimestamp`, `TestHookCandidateBudgetDeferredPastTimestamp`, `TestHookCandidateBudgetDeferredAbsentMetadata`, `TestHookCandidateBudgetDeferredMalformedTimestamp`, `TestHookCandidateClaimableFalseForFutureBudgetDeferred`, `TestHookCandidateClaimableTrueForPastBudgetDeferred`, `TestClaimFirstReadyHookAssignmentSkipsFutureBudgetDeferredCandidate`, `TestClaimFirstEligibleHookCandidateSkipsFutureBudgetDeferredCandidate` — all 8 PASS

## Release disposition

**Gate PASS.** Push `deploy/ga-lnu8pv-gate` and open a PR from it (not from
`builder/ga-818ery`, which is provenance-only and may carry other beads'
commits). Route the merge-request to mayor/mpr; no rig agent merges
directly. Report the gate result and PR URL back to mayor, noting that the
historical flake did not reproduce on this rebased full-suite run — a
corroborating data point for the ongoing beads#4566 load-dependent-race
investigation.
