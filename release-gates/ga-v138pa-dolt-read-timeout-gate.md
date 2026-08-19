# Release gate: ga-v138pa — managed Dolt read-timeout default

**Deploy bead:** `ga-v138pa`  
**Review bead:** `ga-id7z3d`  
**Build bead:** `ga-lfcx72`  
**Reviewed commit:** `9c6ccc8537f5a5cf91b5d67f1dc33c0c55ed4cf7`  
**Source branch:** `builder/ga-lfcx72` (provenance only)  
**Evaluation branch:** `gate-fail/ga-v138pa`  
**Base:** `origin/main` at `a565081fb87c13de8366594ad40ddfd731469539`  
**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Verdict:** **FAIL — the managed shell fallback still emits the old 15-second read timeout**

The reviewed change raises the canonical Go/config default from 15,000 ms to
120,000 ms, but it does not update the materialized `gc-beads-bd` shell
fallback. The required process suite compares both producers and fails with
`ReadTimeoutMillis:120000` from Go versus `ReadTimeoutMillis:15000` from the
shell fallback. This is a deterministic, diff-owned ownership-boundary
regression. No environmental waiver applies.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Reviewer bead `ga-id7z3d` records PASS for exact commit `9c6ccc8537f5a5cf91b5d67f1dc33c0c55ed4cf7`, including config, command, doctor, build, vet, and generated-doc verification. |
| 2 | Acceptance criteria met | **FAIL** | The 120,000 ms default and rationale are present in the canonical Go/config path and generated docs, but the managed shell fallback remains at 15,000 ms. Gas City's managed config behavior therefore depends on which producer is used. |
| 3 | Required tests pass | **FAIL** | `cmd/gc/**` and `internal/**` require the process-tier coverage described by `engdocs/contributors/release-gate-criteria-conventions.md`. `make test-local-full-parallel` ran that coverage and `cmd-gc-process-5-of-6` failed `TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics`. The focused package invocation skips by design because the test requires the materialized shell fallback; the process shard executed it and exposed the mismatch. |
| 4 | No HIGH-severity reviewer findings open | PASS | The reviewer recorded no blocking findings. The release gate independently found the blocking process-test regression above. |
| 5 | Final branch clean | PASS | The exact reviewed commit had a clean worktree before this gate record was added; `git diff --check` passed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main 9c6ccc8537f5a5cf91b5d67f1dc33c0c55ed4cf7` completed without conflict and produced tree `8b15b75388b686a9f4eceb8b58a2b28a9edc7ecf`. |
| 7 | Change is cohesive and reviewable | PASS | Eight files form one theme: the Dolt read-timeout default, its rationale/tests, doctor expectation, and generated config documentation. The reviewer confirmed the doctor fixture synchronization is in scope. |

## Verification evidence

Focused, generation, and static gates passed:

- `go test -count=1 ./internal/config/...`
- focused `cmd/gc` tests for managed config writing, read-timeout rationale,
  disabled `wait_timeout`, and listener overrides
- `go test -count=1 ./internal/doctor/...`
- `make check-docs`
- `go run ./cmd/genschema && git diff --exit-code`
- `make test-ci-policy`
- affected-path lint: 0 issues
- `make fmt-check-changed`
- `go build ./...`
- `go vet ./...`

The broad required sweep used the pinned Dolt tool and rootless container
socket:

```text
PATH=/var/tmp/gc-gate-ga-f8it32-tools/bin:$PATH \
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true TMPDIR=/var/tmp LOCAL_TEST_JOBS=4 \
make test-local-full-parallel
```

Result: **34 jobs passed, 6 jobs failed**. Logs are under
`/var/tmp/gc-local-tests.rJiXxT`.

### Blocking, diff-owned failure

`cmd-gc-process-5-of-6`:

```text
--- FAIL: TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics
Go:    ReadTimeoutMillis:120000
Shell: ReadTimeoutMillis:15000
```

Evidence:
`/var/tmp/gc-local-tests.rJiXxT/cmd-gc-process-5-of-6.log`.

The standalone focused invocation passed only by explicitly skipping with the
message that the materialized shell fallback requires the full
`make test-cmd-gc-process` coverage. The required shard did not skip and
failed. `waiver_ref`: none.

### Non-diff-owned failures retained in the record

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and
  `TestGraphWorkflowFailureRunsCleanup` reproduced the known
  `gastownhall/beads#4566` dirty-table migration signature, tracked and logged
  on `ga-lpfjhc`.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` reproduced the host tmux
  3.7b key-table issue tracked by `ga-afqddr`.
- `TestCleanInstallTutorialPath` again received circuit-breaker diagnostics on
  parsed stdout. The recurrence is tracked by `ga-hrdd3h` (prior chain:
  `ga-rsktma`).
- `TestEnsureSessionFresh_ZombieSession` misclassified a fresh shell during
  the tmux shard; follow-up `ga-kmwwcx` records the untracked recurrence.

None of those background failures can change this verdict because the managed
config parity failure is directly caused by the reviewed change.

## Disposition

- Do not push `gate-fail/ga-v138pa`.
- Do not open a pull request.
- Return `ga-v138pa` to `gascity/builder` with the existing molecule preserved.
- Required repair: update the shell fallback's managed Dolt
  `read_timeout_millis` default to 120,000 and rerun the process-tier release
  gate from the newly reviewed commit.
