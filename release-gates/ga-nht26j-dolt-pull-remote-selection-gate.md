# Release gate: deterministic Dolt pull remote selection

- Deploy bead: `ga-nht26j`
- Build/review work: `ga-fe5cva` / `ga-mdce9d` / `ga-g04htm`
- Reviewed source: `1b241399afe4adc6f57c7f429a859d16c2e69c86`
- Base: `origin/main@ed146d8d9f2fdf142b4b23540ff0412fd2eec33c`
- Deploy branch: `deploy/ga-nht26j-gate`
- Deploy mode: remote; push target: `origin`
- Evaluated: 2026-09-02
- Verdict: **PASS with three attributed, non-diff-owned test failures**

## Gate checklist

The target pre-flight ran before criterion 6. The reviewed source is not carried
by a pull request. Predecessor PR #5582 is closed without merging and its
`mpr/close-disposition` status records it as superseded by this round-5 source;
the mayor removed its stale remote branch after preserving the old head through
GitHub's immutable pull ref. Criterion 6 was then checked first against the
current base and passed, so the remaining criteria were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review-of-record `ga-g04htm` contains scoped delta re-review #5 with `verdict: pass`; both `metadata.commit` and `metadata.deploy_commit` resolve to and exactly equal the reviewed source. The review independently verified that the rebase did not change the three owned pull files and that the census reconciliation is mechanical. |
| 2 | Acceptance criteria met | **PASS** | SQL discovery uses deterministic `ORDER BY name`; CLI discovery keeps remote names paired with URLs; both feed one `select_remote` policy. Multiple remotes require `GC_DOLT_REMOTE_<DB>`. Every selected non-`file://` remote, including a sole remote, requires `GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1`; a sole `file://` remote remains zero-configuration. Invalid and unknown overrides fail closed. The 13 Pull tests and the broader diff-owned evidence below exercise these branches. |
| 3 | Tests pass | **PASS with attributed failures** | With the rootless Podman socket configured, the documented full-suite command completed all 40 jobs: **37 green / 3 red jobs**, with **48,347 PASS / 3 FAIL / 208 SKIP** top-level test results. All three failures are non-diff-owned and satisfy criterion 3a. All **46/46** top-level tests from the added or modified test files reported PASS; none failed or skipped. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PASS** | The three raw failures remain visible and are mapped to opened root-condition trackers below. `TestBdFlagManifestCurrent` has a deterministic mechanism proof; the Dolt sweep failure has the same live-server signature on unrelated candidates plus a source-path proof; `TestActivityLive` has a source-traced pre-transition readiness bug. No attribution is inconclusive. |
| 3b | Policy and static lanes | **PASS** | `make test-ci-policy` PASS; `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` PASS with 0 issues; the corresponding `make fmt-check-changed` PASS; `make vet` PASS; `make check-docs` PASS; `sh -n examples/bd/dolt/commands/pull/run.sh` PASS. Supplemental ShellCheck reports only pre-existing SC1007/SC1091 findings on unchanged header lines 12-13, already documented by review. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check change)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. Round-5 review records no security or correctness blockers and no new production-code delta beyond the already-reviewed source. |
| 5 | Final branch is clean | **PASS** | The exact reviewed source was clean before branch creation. `resolve_deploy_branch_target` checked out the isolated deploy branch at that SHA; the gate record is the only deployer-authored change and is committed separately. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree --messages origin/main 1b241399afe4adc6f57c7f429a859d16c2e69c86` returned tree `0de7b9ba8e4ea6ea19da5b7e5a337a22d5695ec5` with exit 0 and no conflict messages against `origin/main@ed146d8d9f2fdf142b4b23540ff0412fd2eec33c`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The source changes one subsystem and behavior: deterministic, explicit, locality-aware remote selection for `gc dolt pull`. Test fixtures and resource-census ledger updates only exercise and account for that behavior; the carried historical gate markdown concerns the same feature. |

## Full-suite test evidence

`test_cmd_scope: full-suite`

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GOFLAGS=-v \
LOCAL_TEST_LOG_DIR=/var/tmp/gc-gate-ga-nht26j.4EaOoZ \
make test-local-full-parallel
```

The runner selected `LOCAL_TEST_JOBS=11` from the documented CPU/memory bound.
All 40 jobs completed. Raw logs are retained under
`/var/tmp/gc-gate-ga-nht26j.4EaOoZ`.

`diff_tests_executed: 46 PASS, 0 FAIL, 0 SKIP`

- `pull_remote_selection_test.go`: 11/11 PASS.
- `pull_test.go`: 2/2 PASS.
- `sync_test.go`: 33/33 PASS.

The 208 top-level skips are all pre-existing tests outside those three changed
test files. Their logged preconditions cover unsupported platforms, unavailable
or deliberately opt-in live infrastructure/providers, helper-process modes,
and non-applicable fixture states. None is diff-owned, and no acceptance branch
for this change was skipped.

## Raw failures and attribution

| Raw result | Test | Tracker / proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` | Open tracker `ga-f0uceo`, created 2026-08-15 and opened during this evaluation. Mechanism proof: the test compares the host-installed `bd` help surface with `internal/bdflags`; the candidate changes neither. The current failure is the established newer-installed-binary signature across 17 subcommands. No path overlap. Current sighting was appended and read back. |
| **FAIL — ATTRIBUTED** | `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` | Open root-condition tracker `ga-vkhfnj`, created 2026-08-29 and opened during this evaluation. The failure is the established parallel-load signature: the sweep removed a directory still held by a live Dolt server. Identical signatures are recorded on unrelated candidates including `ga-zron27` and `ga-9c55c6`; the reviewed Dolt-orphan fix is not in `origin/main`. The candidate changes no `examples/gastown` or `internal/doltorphan` source; its `examples/bd/dolt` test files are not compiled into this separate test binary, and no source in the failing path references the changed pull script or census ledgers. No path overlap. Current sighting was appended and read back. |
| **FAIL — ATTRIBUTED** | `TestActivityLive` | Open tracker `ga-fua7nj`, created from this discovering run under criterion 3a's tracker-timing escape after the mechanism proof landed. The test reports `idle`, then reports `working`, but waits only for a nonzero timestamp younger than 100ms. The just-seeded idle timestamp already satisfies that predicate before the asynchronous state transition; the log then compares that exact same timestamp twice. The candidate cannot reach `internal/runtime/herdr`, and no path overlaps. |

`failure_attribution`:

- `TestBdFlagManifestCurrent -> ga-f0uceo | mechanism: installed-binary/source-manifest skew wholly outside the diff`
- `TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-vkhfnj | cross-PR identical signature plus separate-path mechanism proof`
- `TestActivityLive -> ga-fua7nj | mechanism: pre-transition idle stamp falsely satisfies working-readiness predicate`

`inconclusive-guard: n/a — all three attributions have decisive mechanism or
cross-PR proof.` The candidate declares `added_test_load=yes` (+11 subprocess
call sites and one test file in the census), but no attribution relies on an
inconclusive base-only result. `waiver_ref: none`.

## Pre-flight, ancestry, and publication evidence

- The recorded reviewed SHA passed a hex-only guard and resolves to the full
  commit above. `origin/builder/ga-nht26j-footprint-fix` points to that exact
  commit.
- `gh api repos/gastownhall/gascity/commits/<reviewed-sha>/pulls` returned no
  pull request.
- Closed predecessor PR #5582 has `mpr/close-disposition=success`, description
  `superseded ... successor: builder/ga-nht26j-footprint-fix round-5 tip`.
- `assert_deploy_ancestry_scope origin/main <reviewed-sha> ga-nht26j ga-mdce9d
  ga-fe5cva` passed.
- `resolve_deploy_branch_target` created `deploy/ga-nht26j-gate` at exactly the
  reviewed source. `assert_safe_push_target` and
  `assert_reviewed_sha_present` both passed.
- The superseded remote branch was deleted by the mayor and verified absent via
  `git ls-remote`; publishing this branch is a normal new-ref push, not a force
  update.
