# Release Gate: ga-5o2x5n TMPDIR Redirect

Bead: ga-5o2x5n
Branch: builder/ga-ntbpyb.4-tmpdir-redirect
Candidate commit: b24b0be0d3798d957980d07deb30ed2a1a2b6b92
Base: origin/main b8818d945b502ddd84e6d627dead657dca9b639c
Gate worktree: /var/tmp/gascity-deployer-ga-5o2x5n.Y1dcNU
Gate date: 2026-07-16

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git fetch origin main` succeeded. `git rev-list --left-right --count origin/main...origin/builder/ga-ntbpyb.4-tmpdir-redirect` returned `0 1`. `git merge-tree --write-tree origin/main origin/builder/ga-ntbpyb.4-tmpdir-redirect` exited 0 with tree `3e43cabbfa339bd052ce7d6091964fae6cb7b5f9`. |
| 1 | Review PASS present | PASS | Review bead ga-73wnph is closed and its notes contain `Review verdict: PASS`. Reviewer verified the diff and filed only non-blocking fast-follow ga-q5qhta. |
| 2 | Acceptance criteria met | PASS | Diff is scoped to the intended TMPDIR fallback change: Makefile TEST_ENV uses `${TMPDIR:-/var/tmp}`; `scripts/go-test-observable`, `scripts/test-go-test-shard`, `scripts/test-integration-shard`, and both `scripts/test-local-parallel` fallback sites use `/var/tmp`; `scripts/tmpdir_default_test.go` covers defaulting off `/tmp`, respecting caller-supplied TMPDIR, socket-path headroom, and exact fallback-site counts. `rg` found no remaining `${TMPDIR:-/tmp}` in the touched files. |
| 3 | Tests pass | PASS | `gofmt -l scripts/tmpdir_default_test.go` produced no output. `bash -n scripts/go-test-observable scripts/test-go-test-shard scripts/test-integration-shard scripts/test-local-parallel` passed. `HOME=/home/jaword TMPDIR=/var/tmp go test ./scripts/... -run 'TestMakefileTestEnvDefaultsTMPDirOffSharedTmpTmpfs|TestMakefileTestEnvRespectsCallerSuppliedTMPDir|TestMakefileTestEnvTMPDirDefaultLeavesSocketPathHeadroom|TestShardScriptsDefaultTMPDirOffSharedTmpTmpfs'` passed. `HOME=/home/jaword TMPDIR=/var/tmp go vet ./...` passed. `HOME=/home/jaword TMPDIR=/var/tmp make test-fast-parallel` passed all 8 jobs. |
| 4 | No high-severity review findings open | PASS | Review notes contain one non-blocking fast-follow, ga-q5qhta, priority P3/open. No HIGH or critical finding is recorded in ga-73wnph or ga-5o2x5n notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, scratch worktree status was clean at candidate commit b24b0be0d. This gate file is the only deployer-added change and will be committed as the branch tip. |
| 7 | Single feature theme | PASS | The commit set is one feature theme: test-runner TMPDIR defaults for the local parallel test harness. The diff touches Makefile plus scripts test-runner wrappers and one test file only. |

## Changed Files

```text
Makefile
scripts/go-test-observable
scripts/test-go-test-shard
scripts/test-integration-shard
scripts/test-local-parallel
scripts/tmpdir_default_test.go
```

## Test Summary

```text
ok  	github.com/gastownhall/gascity/scripts	0.124s
ok  	github.com/gastownhall/gascity/scripts/cipolicy	0.003s [no tests to run]
All fast jobs passed
```
