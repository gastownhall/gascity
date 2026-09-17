**Verdict:** **PASS**

# Release gate: Dolt orphan sweep deletion granularity

- Deploy bead: `ga-k9r169`
- Build bead: `ga-nh56ux`
- Review beads: `ga-qxhmrj`, `ga-k0rl0s`
- Reviewed commit: `63ce479412d4e4ed7876f0bf8a6c667dd46ffae2`
- Merge base: `581bae97cb0937d2a701f158f43d0e67a20edfa2`
- Base evaluated: `origin/main@d807c8e0dd6d071a79e821be5da73097dbf4ff96`
- Deploy mode: `remote`; push remote: `fork`
- Evaluated: 2026-09-17
- Criteria source: the authoritative deployer release-gate criteria fragment,
  `TESTING.md`, the Makefile, and
  `engdocs/contributors/release-gate-criteria-conventions.md`.
  `docs/PROJECT_MANIFEST.md` is absent from both the repository and reviewed
  source.

The already-merged preflight found no pull request carrying the reviewed
commit. Criterion 6 passed before the test run, and a final fetch confirmed the
same base and clean merge result, so no bounded self-rebase was needed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Round-two review `ga-k0rl0s` records an unambiguous PASS with no findings for the exact reviewed commit. The earlier integration-test finding in `ga-qxhmrj` was fixed by the final commit and independently re-reviewed. |
| 2 | Acceptance criteria met | **PASS** | `Sweep` now removes the deepest directory that directly owns a `.dolt` marker, leaving its top-level temporary container and unrelated siblings intact. A literal top-level `.no-orphan-sweep` file exempts the candidate. Age, marker-depth, and fail-closed `lsof` detection remain in place; the held-path check remains keyed to the top-level child, and no name/prefix filter or production call-site change was added. Unit coverage includes the incident-shaped binary/script siblings and sentinel behavior; the real-Dolt SIGKILL integration test confirms the nested store is removed while its enclosing data directory remains. |
| 3 | Tests pass | **PASS** (four attributed non-diff-owned failures) | `test_cmd: make test-local-full-parallel`; `test_cmd_scope: full-suite`; run through `isolated-test-run.sh` with the rootless Podman socket active and Ryuk disabled. `test_counts: 36 job PASS, 4 attributed raw job FAIL, 0 job SKIP` across all 40 documented jobs. All diff-owned tests passed, including the real Dolt integration test; details and attribution are below. No `TRIPWIRE` occurred. `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | **PASS** | The Herdr live-provider failure maps to predating tracker `ga-iepsvr`; the three fixture-initialization failures caused by shared-server schema migration refusal map to predating tracker `ga-lejnse`. None is diff-owned, none overlaps the three changed paths, and mechanism/import-boundary proof shows the candidate cannot cause either external condition. Each exact sighting was appended to and read back from its tracker. |
| 3b | Policy/lint lane | **PASS** (attributed full-fallback lint findings) | `policy_lane: make test-ci-policy` PASS (5 runner-policy tests, 15 CI-suite coverage tests, `scripts/cipolicy`, `scripts/prwatchdog`, and focused static-scope policy tests). `make vet`, `make check-hooks`, `git diff --check`, and changed-file formatting all PASS. The CI-equivalent `make lint-affected` widened to the full repository because this older reviewed head lacks a current-main dashboard asset; its 87 non-diff-owned findings include stale deleted-worktree cache entries and unchanged generated/baseline source, attributed to predating trackers `ga-u8z8j6` and `ga-tcdrnz`. A fresh-cache run from the candidate's actual merge base selected `./cmd/gc ./examples/gastown ./internal/doltorphan ./test/dolttest` and reported `0 issues`. |
| 3c | CI-config lane | **PASS** | `ci_lane_run: n/a`; the candidate changes no workflow, CI job, matrix, timeout, required-check list, test target, or resource policy. |
| 4 | No high-severity review findings open | **PASS** | Round-two review reports no findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | **PASS** | The reviewed source was evaluated in detached clean worktrees. `git status --short` was empty in the deployer worktree before this checklist was added; `git diff --check` and hook-ownership verification passed. This checklist is the only deploy-only change and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After the final fetch, `git merge-tree --write-tree origin/main 63ce479412d4e4ed7876f0bf8a6c667dd46ffae2` exited 0 against `origin/main@d807c8e0dd6d071a79e821be5da73097dbf4ff96` and produced tree `3de68b36e3ab0554d0c97865a8a24c7d4ae40b02`. The reviewed commit is not already on main, and preflight returned no associated pull request. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The three commits are one Dolt orphan-sweep safety change: RED tests, the deletion/sentinel implementation, and the deepest-marker integration fix. All three changed files belong to that behavior; there is no independently shippable second theme. |

## Criterion 3: full-suite evidence

The documented complete local CI union ran at the exact reviewed commit:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
make test-local-full-parallel
```

- Full-suite result: 36 PASS jobs, 4 attributed raw FAIL jobs, 0 SKIP jobs.
- Full runner log: `/var/tmp/ga-k9r169-full.log`.
- Preserved per-job logs: `/var/tmp/ga-k9r169-gate-jobs`.
- `diff_tests_executed`: 15 top-level `internal/doltorphan` unit tests plus
  `TestSweep_ReapsRealDoltDataDirAfterSIGKILL`; all PASS, 0 FAIL, 0 SKIP.
  The marker-depth table contributes three passing subtests, for 19 passing
  test executions in the explicit verbose evidence.
- Explicit unit log: `/var/tmp/ga-k9r169-unit-doltorphan.log`.
- Explicit real-Dolt integration log:
  `/var/tmp/ga-k9r169-integration-doltorphan.log`.
- `skip_justification: n/a` for diff-owned tests; none skipped.
- `waiver_ref: none`.

The explicit diff-owned run covers:

- old vs. young candidates, missing and depth-bounded markers;
- all allowed marker depths and deepest-marker preference;
- live `lsof` holds and fail-closed `lsof` errors;
- isolated removal errors, non-directory entries, root-read errors, default
  age, injected clock, and mixed candidates;
- the incident shape where binary/script siblings survive nested store
  removal;
- the literal `.no-orphan-sweep` opt-out; and
- a real Dolt SQL server terminated with SIGKILL, followed by removal of only
  its abandoned nested database store.

### Criterion 3a: attributed raw failures

1. `internal/runtime/herdr.TestProviderLiveClaudeKindPath` failed because the
   external Herdr provider returned `agent_pane_busy` for unavailable pane
   `w1:p1`. Tracker `ga-iepsvr` predates this run and carries the same test and
   signature. The failing package cannot import or reach
   `internal/doltorphan`, the candidate adds no Herdr resource load, and there
   is no path overlap. This is clause 3(a) mechanism proof.
2. `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` stopped during
   fixture `gc init` because the external `bd` client refused 29 pending
   shared-server migrations (v37 to v66).
3. `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` stopped
   during fixture initialization because `bd` refused 15 pending migrations
   (v51 to v66).
4. `TestAdoptPRFormulaCompileAndRun` stopped during fixture `gc init` because
   `bd` refused 7 pending migrations (v59 to v66).

The three schema refusals map to predating tracker `ga-lejnse`, which covers
temporary test cities reaching the shared Dolt server and records the same
failure condition across unrelated candidates. They fail before candidate
behavior executes. The candidate changes neither `gc init`, beads/schema
handling, test topology, nor resource settings, and adds no new suite target.
There is no path overlap. This is clause 3(a) mechanism/cross-PR proof.

`failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr;
TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-lejnse;
TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-lejnse;
TestAdoptPRFormulaCompileAndRun -> ga-lejnse`.

## Acceptance evidence

1. `findDoltStoreDir` returns the directory that directly owns the selected
   `.dolt` marker and fully explores nested directories before falling back to
   a shallower marker. This handles Dolt SQL server bookkeeping and a nested
   database marker without deleting the enclosing server data directory.
2. Removal and `SweepResult.Removed` now use that store directory, while
   `lsofHeldChildren` continues to guard the top-level child because its
   one-segment scan cannot safely classify deeper paths.
3. `hasSentinel` checks only for the literal `.no-orphan-sweep` entry directly
   inside a top-level candidate. No directory naming heuristic was introduced.
4. The production change is confined to `internal/doltorphan/sweep.go`; the
   remaining changes are adjacent unit and integration coverage. No caller,
   API, configuration, workflow, or generated artifact changed.

## Decision

**Gate PASS.** Cut `deploy/ga-k9r169-gate` from the exact reviewed commit,
commit this checklist, push it to the configured fork, open a pull request
against `gastownhall/gascity:main`, publish deploy clearance on the exact gated
PR head, and route the merge request to the merge authority. The deployer does
not merge.
