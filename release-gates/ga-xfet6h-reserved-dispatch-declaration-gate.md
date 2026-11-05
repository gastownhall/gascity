# Release gate: reserved dispatch declaration

- Deploy bead: `ga-xfet6h`
- Implementation bead: `ga-xxyzgq`
- Review bead: `ga-p70gwk`
- Originally reviewed source: `84a6f8313edeb5d13ee8973243403d1a050fe3f4`
- Reviewed source at this gate (rebase + mayor-directed remediation): `8ede0fdba6ad80214d72f449d4ca0c0889488e01`
- Source branch: `deploy/ga-xfet6h-gate` (cut from the reviewed commit, rebased onto current `origin/main`, plus one remediation commit; `builder/ga-xxyzgq` is provenance-only and is not the PR source)
- Base checked at gate time: `origin/main@bff31d320f99301ba250ecc27ba4657f3545467c`
- Gate result: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from this revision. This checklist applies
the seven release criteria in the deployer protocol and the implementation
bead's recorded exit contract.

## Prior gate history (context, not re-litigated)

The reviewed source (`84a6f8313e`) was first gated and **FAILed** on criterion
3/3a — see `release-gates/ga-xfet6h-reserved-dispatch-declaration-gate.md` at
commit `11dfc117018da1599e2c51a9f07669777e5ba36b` on branch `gate/ga-xfet6h-fail`
(unpushed; left untouched; that file is not an ancestor of this branch). The
diff shipped four `t.Skip`-gated capacity/budget tests
(`TestOrderDispatchReservedCapacityDoesNotConsumeOrdinaryBudget`,
`TestOrderDispatchReservedCapacityIsCappedAtThree`,
`TestOrderDispatchReservedUnusedCapacityDoesNotIncreaseOrdinaryBudget`,
`TestOrderDispatchReservedOverflowRotatesAcrossTicks`) scoped to a follow-on
capacity-pool bead (`ga-1ocm3f`) that had not yet landed. Review `ga-p70gwk`
explicitly judged this scoping legitimate ("not diff-dodging") and PASSed the
remaining code on its merits, but the gate FAILed anyway: per
`engdocs/contributors/release-gate-criteria-conventions.md`, a diff-owned
`t.Skip` is an automatic hard FAIL on criterion 3/3a regardless of the
skip's scope-justification — the review's substantive judgment on the rest
of the diff was never in question.

This was escalated to mayor. Mayor's ruling affirmed the hard-FAIL reading of
diff-owned `t.Skip` and directed disposition (a): remove the four skipped
capacity-pool tests and land the declaration-only feature now, leaving
`ga-1ocm3f` to bring its own tests (and the capacity/budget enforcement they
cover) later as a separate, independently-gated change. Commit
`8ede0fdba6ad80214d72f449d4ca0c0889488e01` ("fix(cmd/gc): remove diff-owned
skipped reserved-dispatch tests") implements that directive.

A targeted diff confirms the remediation is purely subtractive and touches
nothing else: `git diff --stat 84a6f8313edeb5d13ee8973243403d1a050fe3f4..HEAD
-- cmd/gc/order_dispatch_reserved_test.go internal/orders/order.go
internal/orders/order_test.go` reports exactly one file changed —
`cmd/gc/order_dispatch_reserved_test.go | 128 deletions(-)` — removing only
the four skipped test functions, their shared helper (`countCallsWithPrefix`),
and the now-unused `"strings"` import. Zero lines added; `internal/orders`
and every other file under `cmd/gc` that review `ga-p70gwk` covered are
byte-identical to the reviewed commit. Review `ga-p70gwk`'s PASS verdict
therefore substantively covers 100% of the code that remains after
remediation — nothing unreviewed was introduced by this gate's remediation
step.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-p70gwk` records `verdict: pass` for reviewed source `84a6f8313e` (`base_commit: f3610ae8e69a05a72c2f9639be8a5754907b99cb`), with clean style, security, and specification findings. The only change since review is the pure subtractive remediation described above (128 deletions, 0 additions, 1 file) — see "Prior gate history." No unreviewed line exists on this branch. |
| 2 | Acceptance criteria met | **PASS** | `internal/orders/order.go` (unchanged since review) adds `ReservedDispatch bool` (`toml:"reserved_dispatch,omitempty"`) to `orders.Order`, letting a pack author opt an order into bounded reserved dispatch declaratively instead of via name-based Go logic. Three built-in order definitions (`examples/bd/dolt/orders/dolt-health.toml`, `internal/bootstrap/packs/core/orders/beads-health.toml`, `internal/bootstrap/packs/core/orders/gate-sweep.toml`) each adopt the new `reserved_dispatch = true` flag. The four surviving dispatcher-level tests in `cmd/gc/order_dispatch_reserved_test.go` (all PASS — see criterion 3) prove a reserved order still composes correctly with the pre-existing generic dispatch policies: suspension (city/rig), the open-work gate (default blocks; explicit `no_work_gate` opt-out still fires), trigger/cooldown eligibility, and single-flight tracking (no double-dispatch on an immediate repeat tick, whether in-flight or completed). Capacity/budget enforcement is explicitly out of scope for this declaration-only change and is deferred to `ga-1ocm3f` per mayor's ruling. |
| 3 | Tests pass | **PASS** | `make test-cmd-gc-process` (`GC_FAST_UNIT=0`, the CI-required job for `cmd/gc/**`/`internal/**` changes per `engdocs/contributors/release-gate-criteria-conventions.md`) run clean on this branch's HEAD (`8ede0fdba6a` — confirmed by the log's own `HEAD before run:` stamp). Full log: `ga-xfet6h-rebase2-full-cmdgcprocess.log` (19,285 lines, `EXIT_CODE=0`, zero word-boundary `FAIL` matches, zero `panic:` anywhere). The chained `test-productmetrics-testhook` sub-target also ran and passed. All four surviving diff-owned tests re-confirmed via targeted rerun (`ga-xfet6h-rebase2-diffowned-tests.log`, 0.845s, 4/4 PASS with subtests). A broader 197-package sweep (`ga-xfet6h-rebase2-integration-packages.log`) passed everywhere except `internal/bdflags` (non-attributable — see 3a); `internal/orders` itself is clean (1.592s) and `cmd/gc` itself is independently clean within that same sweep (461.323s). Flake isolation: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` (the historically flaky beads#4566-signature test) re-run 3x in isolation, 3/3 PASS (`ga-xfet6h-rebase2-flaketest.log`, 8.62s/5.55s/5.39s). |
| 3a | Failure attribution | **PASS / non-attributable** | `internal/bdflags`: `TestBdFlagManifestCurrent` fails 5 subtests (`create`, `list`, `ready`, `show`, `update`) reporting the installed `bd`'s `--help` output has drifted from the flag manifest — e.g. `create` is missing `--allow-empty-description --storage-class`, `list` is missing `--brief --deps --external-contains --external-ref --max-rows`, `ready` is missing `--brief --label-pattern --label-regex --max-rows`, `show` is missing `--brief-deps`, `update` is missing `--force` (full detail in `ga-xfet6h-rebase2-integration-packages.log`). This diff touches zero files in `internal/bdflags` and carries no `bd`/manifest change of any kind. The installed `bd` binary and the manifest are both identical to `origin/main` — this diff cannot affect either — so the failure is logically guaranteed to reproduce identically, unmodified, on `origin/main` itself: a pre-existing repo-wide manifest-staleness bug, not a regression this diff introduced. |
| 3b | Policy/lint lane | **PASS** | `go vet ./...` clean (`ga-xfet6h-rebase2-govet.log`, empty output). `make lint-changed` scoped to `./cmd/gc ./internal/orders` reports `0 issues` (`ga-xfet6h-rebase2-lintchanged.log`). |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, matrix, timeout, required-check list, or other CI configuration changed (`ga-xfet6h-rebase2-ciconfig-diff.log` is empty). |
| 4 | No unresolved HIGH review findings | **PASS** | Review `ga-p70gwk` reports no style, security, specification, or coverage blocker and no HIGH finding. The mayor-level ruling on criterion 3/3a's SKIP-tolerance was a gate-procedural override, not a review-level finding, and it is fully resolved by this remediation. Unchanged by the rebase — no reviewed line differs. |
| 5 | Final branch clean | **PASS** | `git status --porcelain=v1` is empty on `deploy/ga-xfet6h-gate` at HEAD `8ede0fdba6ad80214d72f449d4ca0c0889488e01`. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated after a fresh fetch. `git merge-base HEAD origin/main` equals `origin/main`'s own tip (`bff31d320f99301ba250ecc27ba4657f3545467c`) exactly, i.e. `git merge-base --is-ancestor origin/main HEAD` confirms `origin/main` is an ancestor of HEAD. `git rev-list --count origin/main..HEAD` reports 3 commits ahead. `git merge-tree --write-tree --messages origin/main HEAD` exited 0 with tree `5237a95d9ff44d6771e00f474c088a068b896b52` and zero conflict messages. |
| 7 | Single feature theme | **PASS** | `git diff --stat origin/main...HEAD` touches exactly 6 files, all one theme (declarative reserved-dispatch eligibility): `cmd/gc/order_dispatch_reserved_test.go` (new, +243), `internal/orders/order.go` (+the `ReservedDispatch` field, struct realignment), `internal/orders/order_test.go` (+83), and one-line `reserved_dispatch = true` adoptions in the three built-in order TOMLs named under criterion 2. The three commits are `test(feat): red`, `feat: green`, and the mayor-directed `fix(cmd/gc): remove diff-owned skipped reserved-dispatch tests` — a single TDD arc plus its own remediation, nothing else. |

## Test evidence

- `test_cmd`: `make test-cmd-gc-process`
- `test_cmd_scope`: `cmd/gc/**`, `internal/**` required-job lane (`GC_FAST_UNIT=0`)
- `test_counts`: 0 `FAIL` (word-boundary) / 0 `panic:` across 19,285 lines; `EXIT_CODE=0`; chained `test-productmetrics-testhook` sub-target also PASS
- `full_log`: `ga-xfet6h-rebase2-full-cmdgcprocess.log` (scratchpad)
- `diff_owned_log`: `ga-xfet6h-rebase2-diffowned-tests.log` (scratchpad) — 4/4 surviving diff-owned tests, targeted rerun, 0.845s
- `flake_isolation_log`: `ga-xfet6h-rebase2-flaketest.log` (scratchpad) — `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` alone, `-count=3`, 3/3 PASS
- `integration_sweep_log`: `ga-xfet6h-rebase2-integration-packages.log` (scratchpad) — 197 packages; only `internal/bdflags` failed (non-attributable, see 3a); `internal/orders` and `cmd/gc` both independently clean within the same sweep
- `failure`: `internal/bdflags` / `TestBdFlagManifestCurrent` (5 subtests: create, list, ready, show, update)
- `failure_attribution`: non-attributable — zero file overlap with this diff; installed `bd` and manifest both identical to `origin/main`; pre-existing repo-wide manifest-staleness bug, not a diff regression
- `waiver_ref`: not invoked — the failure is fully non-attributable on its own evidence, no waiver needed
- `ci_lane_run`: n/a — no CI-config change
- `lint_lane`: `go vet ./...` clean; `make lint-changed` → `0 issues` (`./cmd/gc ./internal/orders`)
- `diff_tests_executed`: `TestOrderDispatchReservedOrdersRemainSubjectToSuspension` (city, rig), `TestOrderDispatchReservedOrdersPreserveOpenWorkPolicy` (default gate blocks, no-work-gate opt-out fires), `TestOrderDispatchReservedOrdersRemainSubjectToTriggerEligibility`, `TestOrderDispatchReservedOrderDoesNotDoubleDispatchOnRepeatTick` (in-flight tracking bead, completed cooldown) — all PASS

## Release disposition

**Gate PASS.** Push `deploy/ga-xfet6h-gate` and open a PR from it (not from
`builder/ga-xxyzgq`, which is provenance-only and may carry other beads'
commits). Route the merge-request to mayor/mpr; no rig agent merges
directly. Report the gate result and PR URL back to mayor, along with the
`internal/bdflags` non-attributable-failure finding as a corroborating data
point (the manifest-vs-installed-`bd` drift is a pre-existing, repo-wide
condition independent of this diff, reproducible identically on unmodified
`origin/main`).
