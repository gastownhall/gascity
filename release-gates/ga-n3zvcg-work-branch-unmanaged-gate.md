# Release gate: work-branch-only ownership evidence is unmanaged

- Bead: `ga-n3zvcg`
- Review bead: `ga-976rti`
- Build bead: `ga-ryeij1.1`
- Reviewed commit: `d1a9edf1a1af84fc0c4286e2caf8eda1f0a00e2e`
- Base: `origin/main@28fbaf81a8c8f9e6bb0cb67b6620aa80759d5dba`
- Deploy mode: `remote`; push remote resolved to `origin`
- Result: **PASS** — all candidate-owned tests and required lanes pass; unrelated full-suite and lint findings are attributed to pre-existing trackers under the non-diff-owned failure protocol.

## Pre-flight

- `git rev-parse --verify --quiet d1a9edf1a1af84fc0c4286e2caf8eda1f0a00e2e^{commit}` resolved the recorded value to the same full commit SHA.
- GitHub reports no pull request carrying the reviewed commit, so neither already-merged nor closed-without-merging reconciliation applies.
- The commits are internally authored. No contributor PR or contributor interaction is involved.
- The commit range is one coupled feature in `cmd/gc`: the pool ownership reader accepts the ambient `gc.work_branch`-only shape, while hook claims avoid creating that shape on partially published worktree evidence.
- `assert_deploy_ancestry_scope` passed for `ga-n3zvcg` and confirmed build bead `ga-ryeij1.1`; no unrelated or `.claude/**` content is present.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-976rti` records `verdict: pass` for the exact reviewed commit. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | Source inspection and the focused acceptance run verify that `gc.work_dir` plus only ambient `gc.work_branch` is treated as unmanaged; every other partial ownership shape remains a hard error; existing contaminated beads self-resolve on their next read without a sweep; no metadata key or work-record value semantics changed; and hook claims retain their zero-of-eight/all-eight behavior while refusing to stamp into one-of-eight through seven-of-eight partial evidence. |
| 3 | Tests pass | **PASS** | The documented full command `make test-local-full-parallel` ran on the exact reviewed commit with the rootless Podman socket configured through `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` and `TESTCONTAINERS_RYUK_DISABLED=true`. Runner result: 37/40 jobs PASS, 3 raw FAIL, 0 jobs SKIP or omitted; logs: `/var/tmp/ga-n3zvcg-full.ETW7Jw`. All diff-owned tests were selected by passing full-suite shards. A supplemental exact-name run emitted 7 PASS records, 0 FAIL, 0 SKIP. The three raw failures are independently attributed below. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. `policy_lane: PASS WITH ATTRIBUTED NON-DIFF DIAGNOSTICS` — `make test-ci-policy`, `make vet`, `go build ./...`, module/dependency/event-export/core-boundary guards, native DoltLite tests, and docs checks pass; full lint/format diagnostics occur only in byte-identical pre-existing files or ignored local `node_modules` and are attributed below. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-976rti` records no security findings and no unresolved HIGH finding. Its sole minor note is a non-blocking doc-comment completeness observation. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty before this gate artifact was written. `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | `origin/main...d1a9edf1a1af84fc0c4286e2caf8eda1f0a00e2e` is 2 behind / 2 ahead, and `git merge-tree --write-tree origin/main d1a9edf1a1af84fc0c4286e2caf8eda1f0a00e2e` returned 0 with tree `7dfff1e55e8429ea7e26ca8b6dc4da48997cc7e5`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Both commits are confined to `cmd/gc` pool worktree-ownership evidence and its hook-claim producer, plus their tests. |

## Diff-owned test evidence

All cases below were selected by passing full-suite shards and passed again in the supplemental exact-name run:

- `TestHookClaimIdentityPatchWorkBranchPartialEvidenceGuard/zero_of_eight_present`
- `TestHookClaimIdentityPatchWorkBranchPartialEvidenceGuard/one_of_eight_present`
- `TestHookClaimIdentityPatchWorkBranchPartialEvidenceGuard/seven_of_eight_present`
- `TestHookClaimIdentityPatchWorkBranchPartialEvidenceGuard/all_eight_present`
- `TestWorktreeSpecForBeadTreatsWorkBranchOnlyAsUnmanaged`
- `TestWorktreeSpecForBeadRejectsWorkBranchPlusOneOtherKey`

`diff_tests_executed: 7 PASS records (3 top-level tests plus 4 nested cases), 0 FAIL, 0 SKIP`

## Full-suite failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 1: not diff-owned; clause 2: tracker predates the run; clause 3(a): installed-binary/manifest mechanism is unreachable from cmd/gc and has exact origin/main reproductions; clause 4: no path overlap`
- `failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-vkhfnj | clause 1: not diff-owned; clause 2: tracker predates the run; clause 3(b): the same live-Dolt-directory signature is recorded on unrelated candidate ga-nht26j; clause 4: no examples/gastown or internal/doltorphan path overlap`
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dqd7gf | clause 1: not diff-owned; clause 2: tracker predates the run; clause 3(b/d): exact cross-PR and origin/main missing-citysus-report reproductions predate this gate; clause 4: no test/integration or suspend/resume-report path overlap`
- `waiver_ref: none` — no diff-owned failure or skip occurred, so no independently granted waiver is required.

## Policy/lint attribution

- `policy_attribution: unchanged gofumpt findings -> ga-d3m213 | diagnostic-bearing blobs are identical at merge base, candidate, and origin/main; no candidate path overlap`
- `policy_attribution: unchanged ResolvedProvider.Kind SA1019 uses -> ga-t88402 | diagnostic-bearing blobs are identical at merge base, candidate, and origin/main; no candidate path overlap`
- `policy_attribution: unchanged internal/api SA4006 -> ga-egkp4x | diagnostic-bearing blob is identical at merge base, candidate, and origin/main; no candidate path overlap`
- `policy_attribution: ignored dashboard node_modules govet/revive findings -> ga-039od0 | git confirms the scanned tree is ignored and untracked; the candidate changes no dashboard, dependency, or lint configuration path`
- Policy logs: `/var/tmp/ga-n3zvcg-policy.2f9uFf`; focused acceptance log: `/var/tmp/ga-n3zvcg-targeted.log`; build log: `/var/tmp/ga-n3zvcg-build.log`.

## Disposition

All seven criteria pass. Create the isolated `deploy/ga-n3zvcg-gate` branch from the reviewed SHA, commit this checklist, push it to `origin`, open the pull request, publish deploy clearance on the exact PR head, and route the merge-request to mayor. The deployer does not merge.
