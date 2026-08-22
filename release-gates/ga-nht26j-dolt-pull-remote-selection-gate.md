# Release gate: deterministic Dolt pull remote selection

- Deploy bead: `ga-nht26j`
- Build/review work: `ga-fe5cva` / `ga-mdce9d` / `ga-g04htm`
- Reviewed commit: `74b539d94bfc0fed8ef47fef4a0fd151148795e8`
- Base: `origin/main@08ecb0585498a0a5464e78a3b5d122236ff0ac9d`
- Deploy mode: remote; push target would be `fork`
- Evaluated: 2026-08-22
- Verdict: **FAIL** — no branch was pushed and no pull request was opened

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-g04htm` records `verdict: pass` at the reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | Both SQL and CLI pull paths enumerate remotes deterministically and fail closed when multiple remotes are ambiguous. `GC_DOLT_REMOTE_<DB>` selects a named remote; a non-`file://` selection additionally requires `GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1`. Invalid and unknown overrides fail with actionable errors. The test resource census is updated consistently for the added subprocess tests. |
| 3 | Tests pass | **FAIL** | `LOCAL_TEST_JOBS=4 make test-local-full-parallel` completed all 40 required jobs with **29 PASS, 11 FAIL, 0 SKIP**. `TestCompactScriptExcludesUnversionedTableChurnFromVerification` failed in the candidate union but passed on an immediate candidate package rerun and on exact base. `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` likewise failed in the candidate union and passed on exact base. Both therefore miss attribution clause 3a(iii); the red union cannot be certified. |
| 3a | Pre-existing failures attributed | **FAIL** | `TestBdFlagManifestCurrent` → `ga-f0uceo`, and the two `TestGetKeyBinding_CapturesDefaultBinding*` failures → `ga-afqddr` / `ga-k3fxvj`, reproduced on this exact base with no path overlap. Six fixture-init failures match the standing `gastownhall/beads#4566` dirty-schema authorization recorded on `ga-6bnc42` / `ga-lpfjhc` and remain **FAIL-WAIVED**. The two candidate-union-only failures are tracked as `ga-dpo5w9` and `ga-yxgivi`, but their exact-base runs passed, so they remain hard FAILs. |
| 3b | Policy and static lanes | **PASS (attributed)** | `make test-ci-policy`, `make fmt-check`, and `make vet` passed. Fresh-cache `make lint` reported only the three generated `node_modules/flatted` findings tracked by `ga-4go623`; they reproduce on exact base and do not overlap this diff. `shellcheck -x pull/run.sh` reported only pre-existing SC1007/SC1091 findings at lines 12–13, outside every changed hunk. |
| 4 | No high-severity review findings open | **PASS** | Review records no security findings and no open high-severity findings. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at the reviewed commit before writing this gate artifact. |
| 6 | Branch diverges cleanly from main | **PASS** | With freshly fetched `origin/main@08ecb0585498a0a5464e78a3b5d122236ff0ac9d`, `git merge-tree --write-tree origin/main 74b539d94bfc0fed8ef47fef4a0fd151148795e8` produced tree `4fbf831a5149211ccef0e8252906a6da9a448f0d` without conflict. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One cohesive operator-safety theme: deterministic, explicit, fail-closed remote selection for Dolt pull, plus its tests and mechanically synchronized test-resource ledger. |
| A | Deploy-source ancestry scope | **FAIL** | `assert_deploy_ancestry_scope origin/main <reviewed-commit> ga-nht26j ga-mdce9d ga-fe5cva` returned 21. Commits `13501b7710` (`chore(testpolicy): bank pull-side dolt test subprocess census growth`) and `8a3b6419dd` (`refactor(dolt): drop task-referencing language from pull remote-selection comments`) cite none of the accepted bead IDs. Their paths look feature-related, but the mandatory provenance guard has no evidence tying those commits to the accepted work IDs and cannot be bypassed. |

## Test evidence

- Environment: rootless Podman via `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`; cached image `dolthub/dolt:2.1.7` matched the pinned tag.
- Required union: `LOCAL_TEST_JOBS=4 make test-local-full-parallel` — 29 PASS, 11 FAIL, 0 SKIP jobs. Candidate log: `/var/tmp/ga-nht26j-full-20260822.log`; job logs: `/var/tmp/gc-local-tests.5Vx9gW/`.
- Feature package: `go test -count=1 -json ./examples/bd/dolt/...` — **363 PASS, 0 FAIL, 0 SKIP**.
- Diff-owned tests executed: all **42 PASS**, 0 FAIL, 0 SKIP across `pull_remote_selection_test.go`, `pull_test.go`, and `sync_test.go`; all eight newly added remote-selection tests passed by exact name.
- Exact-base probes on `origin/main@08ecb0585498a0a5464e78a3b5d122236ff0ac9d`: `TestCompactScriptExcludesUnversionedTableChurnFromVerification` PASS in 0.43s; `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` PASS in 27.13s. Logs: `/var/tmp/ga-nht26j-base-logs-20260822/`.
- Policy/static: `make test-ci-policy` PASS; `make fmt-check` PASS; `make vet` PASS; fresh-cache `make lint` FAIL only with the three attributed generated-flatted findings.

## Failed gate disposition

The candidate is not deployable. The builder must return a freshly reviewed SHA whose complete commit range satisfies `assert_deploy_ancestry_scope`—in practice, reword/replay the two uncited commits with an accepted work ID—and must obtain a releasable full-suite result for the two candidate-union-only failures. Nothing was pushed and no PR, clearance status, or merge-request was created.
