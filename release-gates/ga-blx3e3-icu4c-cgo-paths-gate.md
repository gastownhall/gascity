# Release Gate: ga-blx3e3 icu4c CGO path forwarding

Date: 2026-06-05

PR: https://github.com/gastownhall/gascity/pull/3130
Deploy bead: ga-blx3e3
Source review bead: ga-3e79wa
Reviewed commit: 28e60b1654609f85bb59b584b0b77b03a2d3267e
Branch: feat/ga-4s465g-mac-icu-cgo-paths

Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout or
on `origin/main`; this gate uses the deployer release criteria from the role
prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-3e79wa` is closed with `VERDICT: PASS (fix-merge applied)`. PR comment https://github.com/gastownhall/gascity/pull/3130#issuecomment-4633957022 records the reviewer PASS and fixup commit. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `scripts/test-go-test-shard` and `scripts/test-integration-shard`. Both scripts now detect Homebrew `icu4c` on Darwin, append `CGO_CPPFLAGS`/`CGO_LDFLAGS` through the `env -i` boundary, and avoid duplicate include-path injection when nested. A targeted fake Darwin smoke passed for both scripts. |
| 3 | Tests pass | PASS | `bash -n scripts/test-go-test-shard scripts/test-integration-shard` passed. `go vet ./...` passed. Targeted fake Darwin icu4c smoke passed. `LOCAL_TEST_JOBS=8 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel` passed. PR checks are green after rerunning a cancelled `Integration / rest-smoke-2-of-2` job; `gh pr checks 3130` shows `CI / required`, `CI / preflight`, `CI / integration`, CodeQL, and integration shards passing. |
| 4 | No high-severity review findings open | PASS | Reviewer notes contain one low finding, fixed in commit `28e60b1`. PR reviews list is empty and the only PR review-process comment is PASS. No unresolved HIGH findings found. |
| 5 | Final branch is clean | PASS | Detached gate worktree was clean before writing this gate file; after committing this file, final status is verified clean before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed successfully before the gate commit. The branch touches only release-gate markdown after this file is added, so the clean merge result is preserved. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem theme: test runner scripts forwarding macOS icu4c CGO paths through clean test environments. No unrelated feature or package behavior is bundled. |

## Test Commands

```text
bash -n scripts/test-go-test-shard scripts/test-integration-shard
go vet ./...
targeted fake-Darwin icu4c smoke for test-go-test-shard and test-integration-shard
LOCAL_TEST_JOBS=8 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel
gh pr checks 3130
```

## Notes

The first local `make test-fast-parallel` run used the default high local
parallelism and failed two unrelated timing-sensitive tests:

- `internal/beads`: `TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand`
- `cmd/gc`: `TestFindPortHolderPIDUsesProcBeforeLsof`

Both exact tests passed when rerun directly, and the lower-parallel full fast
baseline passed cleanly. The GitHub CI failure observed during gate evaluation
was a cancelled `rest-smoke-2-of-2` job with no test assertion; rerunning failed
jobs made the required CI checks pass.
