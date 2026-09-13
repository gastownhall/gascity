# Release gate: molecule-attached sling re-route

**Disposition:** **PROCEED — criterion 3 contains one raw FAIL that is WAIVED
under the Mayor's standing beads#4566 authorization; all unwaived criteria
PASS.**

**Evaluated:** 2026-08-22 (America/Los_Angeles)

**Deploy bead:** `ga-e71kz1`

**Build/review bead:** `ga-9azvzg`

**Reviewed commit:** `2a0bbfeb34898ccd48b31f64d1803944e6602c77`

**Base:** `origin/main@08ecb0585498a0a5464e78a3b5d122236ff0ac9d`

**Deploy mode:** remote; push-capability probe selected `origin`

**Deploy branch:** `deploy/ga-e71kz1-gate`

`docs/PROJECT_MANIFEST.md` is absent from both this checkout and
`origin/main`. This checklist therefore applies the active seven release
criteria in the deployer contract and the CI-equivalent commands documented in
`TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-9azvzg` records `REVIEWER 2026-08-22 -- VERDICT: PASS` for the exact reviewed commit, with independent build, vet, formatting, package-test, acceptance, and security evidence. |
| 2 | Acceptance criteria met | PASS | The implicit default-formula route falls back to plain bead routing on a pre-existing non-workflow molecule in both legacy and graph-v2 compiler paths, records a warning, and closes the unused graph-v2 synthetic input convoy. Explicit `--on`/`--formula` attachment remains a hard failure. The two new regression tests and both cross-package guards pass by name. |
| 3 | Tests pass | **FAIL — WAIVED** | `LOCAL_TEST_JOBS=4 make test-local-full-parallel` reported 36 PASS / 4 FAIL / 0 SKIP jobs. Three failures satisfy all four pre-existing-failure attribution clauses. The fourth is the exact `gastownhall/beads#4566` dirty-schema signature and is preserved as FAIL-WAIVED under the Mayor's standing authorization on `ga-6bnc42`; the required sighting was logged on `ga-lpfjhc`. Both diff-owned tests report PASS. Full evidence is below. |
| 3a | Pre-existing failures attributed | PASS | `TestBdFlagManifestCurrent` and both tmux default-binding tests failed identically on exact base and have tracked beads with no diff-path overlap. The remaining dirty-schema failure is separately covered by the signature-specific standing authorization; it is not rewritten as green. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, affected-package lint, full build, full vet, gofmt, and `git diff --check` all pass on the reviewed commit. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no security, blocking, or HIGH-severity finding. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the reviewed commit immediately before this checklist was written. The checklist is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed commit contains current `origin/main`; `git merge-tree --write-tree origin/main <reviewed-sha>` exited 0 and produced tree `cb42792bae60174be821fd9fe719263e1543b5fc`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Five commits modify only three `internal/sling` files for one behavior: routing an already-molecule-attached bead when the target supplies an implicit default formula. The legacy and graph-v2 paths are coupled implementations of that same feature. |

## Pre-flight and source integrity

The recorded commit resolves to the full SHA above. The GitHub
commit-to-pull-request query returned no associated pull requests, so there is
no already-merged, closed, or superseded PR to reconcile.

Deploy mode is remote because the repository has GitHub `origin` and `fork`
remotes, and GitHub authentication is active. A no-hook dry-run of the exact
candidate to the isolated branch ref succeeded against `origin`, so `origin`
is the selected push remote.

`assert_deploy_ancestry_scope` passed for `ga-e71kz1`, review/build bead
`ga-9azvzg`, and confirmed same-feature predecessor `ga-ueugmi`. The latter
accounts for the inherited legacy-compiler red/green pair described in the
review record; the graph-v2 pair and follow-up commit cite `ga-9azvzg`. The
range introduces no `.claude/**` or independent-theme path.

## Acceptance evidence

The reviewed diff is limited to:

- `internal/sling/sling_attachment.go`
- `internal/sling/sling_core.go`
- `internal/sling/sling_core_test.go`

Direct inspection confirms:

- `MoleculeAttachedError` distinguishes the non-workflow attachment case from
  graph-v2 workflow conflicts and unrelated metadata/probe failures.
- Only the implicit `default_sling_formula` caller enables fallback; explicit
  formula attachment passes `false` and retains fail-closed behavior.
- Legacy and graph-v2 paths both warn and call the ordinary `finalize` route,
  ensuring `gc.routed_to` is still set.
- Graph-v2 fallback closes the synthetic input convoy created before the
  attachment check instead of leaking open, claim-attracting work.
- Batch fan-out behavior is intentionally unchanged and tracked separately by
  `ga-6htqom` because it needs a distinct partition/report design.

Focused commands and results:

```text
go test ./internal/sling -run '^(TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached|TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttachedGraphV2Formula)$' -count=1 -v
test_counts: 2 PASS, 0 FAIL, 0 SKIP

go test ./cmd/gc -run '^TestOnFormulaExistingMoleculeErrors$' -count=1 -v
test_counts: 1 PASS, 0 FAIL, 0 SKIP

go test ./internal/testenv -run '^TestLegacyFormulaV2MechanismFrozen$' -count=1 -v
test_counts: 1 PASS, 0 FAIL, 0 SKIP
```

`diff_tests_executed`:

- `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached` — PASS
- `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttachedGraphV2Formula` — PASS

`skip_justification`: none required; zero diff-owned or job-level skips.

`waiver_ref`: Mayor's 2026-08-18 standing authorization recorded verbatim on
`ga-6bnc42`, scoped specifically to the `ga-lpfjhc` /
`gastownhall/beads#4566` dirty-table schema-migration signature.

## CI-equivalent test evidence

The rootless Podman socket was configured before testing:
`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` and
`TESTCONTAINERS_RYUK_DISABLED=true`. Cached images include the pinned
`dolthub/dolt:2.1.7` tag. The shared Go build cache was neither cleaned nor
relocated.

```text
test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel
test_counts: 36 PASS jobs, 4 FAIL jobs, 0 SKIP jobs (40 total)
full_logs: /var/tmp/gc-local-tests.Ek5Nbq
```

All unit-core coverage, five of six `cmd/gc` process shards, all six
integration `cmd/gc` package shards, all review-formula shards, bdstore, both
REST-smoke shards, and all eight REST-full shards passed. The sole process
failure was the waived dirty-schema signature described below.

### Raw failures and disposition

| Failing test | Candidate job | Disposition |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `cmd-gc-process-4-of-6` | **FAIL — WAIVED** — exact beads#4566 text: `pending schema migrations alter pre-existing dirty tables: issues`. Candidate only changes `internal/sling`, with no plausible schema-migration/store-bootstrap mechanism. Occurrence logged on `ga-lpfjhc`; standing authorization is on `ga-6bnc42`. Exact-base isolated rerun passed, consistent with the authorized nondeterministic race rather than contradicting it. |
| `TestBdFlagManifestCurrent` | `integration-packages-core-1-of-4` | FAIL — attributed to `ga-gqxh5s`. The installed-`bd` manifest-skew signature failed identically on exact base. `internal/bdflags` has no path overlap with this diff. |
| `TestGetKeyBinding_CapturesDefaultBinding` | `integration-packages-runtime-tmux-2-of-3` | FAIL — attributed to `ga-sxinl6`. Exact base failed with the same empty default `prefix-n` binding. `internal/runtime/tmux` has no path overlap with this diff. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `integration-packages-runtime-tmux-3-of-3` | FAIL — attributed to `ga-sxinl6`. Exact base failed with the same empty `choose-tree` binding. `internal/runtime/tmux` has no path overlap with this diff. |

```text
failure_attribution: TestBdFlagManifestCurrent -> ga-gqxh5s + exact-base matching failure + no path overlap
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding -> ga-sxinl6 + exact-base matching failure + no path overlap
failure_attribution: TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-sxinl6 + exact-base matching failure + no path overlap
failure_waiver: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-lpfjhc signature + ga-6bnc42 standing authorization + occurrence logged + no plausible mechanism/path overlap
```

Exact-base commands at
`origin/main@08ecb0585498a0a5464e78a3b5d122236ff0ac9d`:

```text
go test -tags integration ./internal/bdflags -run '^TestBdFlagManifestCurrent$' -count=1 -v
result: FAIL, same installed-bd flag-manifest skew

go test -tags integration ./internal/runtime/tmux -run '^(TestGetKeyBinding_CapturesDefaultBinding|TestGetKeyBinding_CapturesDefaultBindingWithArgs)$' -count=1 -v
result: 0 PASS, 2 FAIL, same empty default bindings

GC_FAST_UNIT=0 go test ./cmd/gc -run '^TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix$' -count=1 -v -timeout 5m
result: PASS in 8.85s; standing waiver applies because this failure class is nondeterministic and the candidate matched its exact signature and scope conditions
```

## Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `gofmt -l` on all three changed Go files — PASS, no output
- `git diff --check origin/main...HEAD` — PASS

## Waiver disposition

The dirty-schema test's raw result remains **FAIL — WAIVED**; this record does
not call the 40-job suite green. Conditions (a)-(d) of the Mayor's standing
authorization are satisfied: the failure text matches the tracked signature,
the diff cannot reach schema migration or store bootstrap, the occurrence is
logged on `ga-lpfjhc` with the deploy/build identifiers and failing test name,
and this checklist preserves the raw failure. That authorization permits the
deployer to proceed with the isolated branch and pull request without a new
mayor round trip. Merge authority remains mayor/mpr; the deployer does not
merge.
