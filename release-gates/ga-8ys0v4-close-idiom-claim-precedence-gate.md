# Release gate: prefer the live claim over the frozen trigger hint (`ga-8ys0v4`)

- Deploy bead: `ga-8ys0v4`
- Build bead: `ga-cre2wi`
- Review bead: `ga-fwev3f`
- Reviewed commit: `833bc6f3a99a674ebd371a785a30f138118d1f5e`
- Source branch (provenance only): `builder/ga-0frch4-round3`
- Base evaluated: `origin/main@88639e5949ede611e33a56fb328202795064dd29`
- Merge base: `6942953fe1c955d6839a1a82c01683fc364a2151`
- Deploy mode: remote
- Evaluation date: 2026-09-13
- Verdict: **PASS**

`docs/PROJECT_MANIFEST.md` and a matching `work-packages/` file are absent, so
this checklist applies the release criteria in the deployer protocol and the
source bead's done-when list. The remote pre-flight found no pull request
carrying the reviewed commit.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-fwev3f` records `verdict: pass` for the exact reviewed SHA, with no review carryover or SHA substitution. |
| 2 | Acceptance criteria met | **PASS** | The `gc hook current` help and generated CLI reference use `BEAD_ID="${GC_BEAD_ID:-$(gc hook current --id-only)}"`; the frozen trigger hint is no longer in that close chain. The existing formula drain path resolves `gc hook current --id-only` before its startup-trigger fallback. The regression test passed twice in the full union. |
| 3 | Tests pass | **PASS WITH ATTRIBUTION** | The documented full local CI union completed all 40 jobs: 36 jobs passed and 4 failed, with 50,756 top-level PASS, 4 FAIL, and 225 SKIP results. Every raw failure stopped during temporary-city initialization because external `bd` refused pending shared-server schema migrations; the diff-owned test passed twice. Mayor direction `gm-wisp-kh38p5` supplied the decisive comparison rule for `TestCleanInstallTutorialPath`. With pinned `bd v1.3.0-rc.2` and load1 below 20, the reviewed candidate passed 2/2 while exact base failed once with the same random-cursor refusal and passed once. Candidate passing at least once satisfies the Mayor's rule, so all four raw failures are attributed to `ga-lejnse` / `ga-nbza4d`. |
| 3b | Policy/lint lane | **PASS WITH ATTRIBUTION** | `make test-ci-policy`, `go build ./...`, `go vet ./...`, `make fmt-check-changed`, `make check-docs`, and `make check-hooks` passed. The first `make lint-affected` invocation replayed 249 cached diagnostics from deleted sibling `/var/tmp` worktrees, matching predating tracker `ga-039od0`; no candidate path was named. The same target with a fresh on-disk `GOLANGCI_LINT_CACHE` passed with `0 issues`, including its follow-on Go analysis. |
| 3c | CI-config lane run | **PASS / n/a** | The diff changes no CI job, matrix, timeout, required-check list, Makefile target, or runner policy. |
| 4 | No high-severity review findings open | **PASS** | The exact-head review reports no style, security, specification, blocker, or high-severity finding. |
| 5 | Final branch clean | **PASS** | `git status --porcelain=v1` was empty in the detached evaluation worktree at the exact reviewed commit before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 833bc6f3a99a674ebd371a785a30f138118d1f5e` exited 0 and produced tree `ce5cd1b8f10444117e34485e5a33f8c70333671e`; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits change only `cmd/gc` help text, its generated CLI reference, and one regression test for the single claim-before-trigger fallback theme. `assert_deploy_ancestry_scope` passed for the confirmed source/build/review bead chain. |

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/gascity-ga-8ys0v4.BkMOH4/full-logs make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 36/40 jobs PASS; 50,756 top-level PASS, 4 FAIL, 225 SKIP. Including subtests: 89,658 PASS, 4 FAIL, 315 SKIP.
- `diff_tests_executed`: `TestHookCurrentDocIdiomPrefersClaimOverTriggerHint` PASS in `cmd-gc-process-5-of-6` and `integration-packages-cmd-gc-6-of-6`; 0 diff-owned FAIL and 0 diff-owned SKIP.
- `skip_justification`: all skips come from unchanged suite-controlled platform, helper, or opt-in guards; the sole diff-owned test executed and passed in both owning lanes.
- `failure_attribution`: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestCleanInstallTutorialPath`, and `TestGCLiveContract_BeadsAndEvents` -> `ga-lejnse` / `ga-nbza4d`. Mayor direction `gm-wisp-kh38p5` identifies the random `vNN -> v66` cursor as a part-migrated fresh-`hq` race outside the diff and authorizes criterion 3 when the candidate passes at least once or both sides fail the same way. The counted pinned comparison produced candidate PASS/PASS and base FAIL(`v36 -> v66`)/PASS, satisfying the first arm.
- `waiver_ref`: none.
- `authorization_ref`: Mayor mail `gm-wisp-kh38p5` and matching `ga-8ys0v4` note dated 2026-09-13 21:58Z; `ga-lejnse` and `ga-nbza4d` carry the comparison evidence.
- `ci_lane_run`: n/a (no CI configuration change).
- Full runner log: `/var/tmp/gascity-ga-8ys0v4.BkMOH4/full-suite.log` (`sha256:d9192b8550dc2b4d409a889166a16e6c424079dfb03a5cac6618d0c7db4f4d14`).
- Shard logs: `/var/tmp/gascity-ga-8ys0v4.BkMOH4/full-logs` (manifest `sha256:4c34c8455ac44a5eb5b9c5532b09bf2b2f2477f1201905474246cb96b02e511d`).

Raw failures from the full union:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`: external `bd` refused 26 migrations (`v40 -> v66`).
- `TestAdoptPRFormulaRetriesTransientReviewerStep`: external `bd` refused 35 migrations (`v31 -> v66`).
- `TestCleanInstallTutorialPath`: external `bd` refused 56 migrations (`v10 -> v66`).
- `TestGCLiveContract_BeadsAndEvents`: external `bd` refused 20 migrations (`v46 -> v66`).

Initial pinned candidate comparison with `bd version 1.3.0-rc.2 (dev)` first
on `PATH`:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`: PASS (9.90s).
- `TestGCLiveContract_BeadsAndEvents`: PASS (57.62s).
- `TestAdoptPRFormulaRetriesTransientReviewerStep`: PASS (139.84s).
- `TestCleanInstallTutorialPath`: **FAIL** (13.51s), still reaching shared database `hq` and refusing 26 migrations (`v40 -> v66`).

That final result activated `ga-lejnse`'s original stop condition and produced
the correct hold. Mayor then diagnosed the random migration cursor as a
part-migrated fresh-`hq` race tracked by `ga-nbza4d` and directed this exact
comparison, with all counted runs started below load1 20:

- Candidate `833bc6f3a99a674ebd371a785a30f138118d1f5e`, run 1: PASS, 23.60s, load1 17.26.
- Candidate `833bc6f3a99a674ebd371a785a30f138118d1f5e`, run 2: PASS, 24.62s, load1 15.05.
- Base `88639e5949ede611e33a56fb328202795064dd29`, run 1: FAIL, 10.26s, refusal `v36 -> v66`, load1 15.28.
- Base `88639e5949ede611e33a56fb328202795064dd29`, run 2: PASS, 27.33s, load1 17.73.

The candidate's 2/2 PASS satisfies the Mayor's rule. A candidate setup-error
run that pointed writable Git configuration at `/dev/null`, and a supplementary
base PASS begun at load1 25.92, are preserved but excluded from the comparison.
Counted logs are
`/var/tmp/gascity-ga-8ys0v4.BkMOH4/mayor-{candidate-valid-1,candidate-valid-2,base-1,base-valid-2}.log`.

Policy evidence after the hold was removed:

- `make test-ci-policy`: PASS.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed`: PASS.
- `make check-docs`: PASS.
- `make check-hooks`: PASS; `.githooks` owns `core.hooksPath`.
- `GOLANGCI_LINT_CACHE=/var/tmp/ga-8ys0v4-lint.Ug1ape LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected`: PASS, `0 issues`.
- The preceding shared-cache lint attempt is attributed to `ga-039od0`: all 249 diagnostics named deleted sibling worktrees, chiefly `/var/tmp/ga-wwlwa9-eval.F9dZgd` and `/var/tmp/ga-82s3eo-eval`; no candidate path appeared.

## Disposition

Gate PASS. Proceed on a fresh isolated `deploy/ga-8ys0v4-gate` branch cut from
the exact reviewed SHA; the deployer does not merge.
