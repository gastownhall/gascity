# Release Gate: events CLI validation test deflake

- Deploy bead: `ga-d4c2in`
- Source review bead: `ga-edh5gn`
- Originating bug bead: `ga-m1uo4w`
- Branch: `builder/ga-m1uo4w-fix-events-json-deprecation-test`
- Reviewed commit: `b62dc17b065efc675836f61111f0d15c439a2ae5`
- Base checked: `origin/main@044a49b7d21ba012d02034b70ed9acd5d7ecb6fe`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the active deployer release criteria and the repository testing guidance in `TESTING.md`.

## Gate Criteria

Criterion 6 was evaluated first per deployer instructions.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-edh5gn` is closed with `Reviewer verdict: PASS`. The reviewer independently verified the root cause, deterministic replacement failure path, formatting, static checks, and coverage intent. |
| 2 | Acceptance criteria met | PASS | The branch changes only `cmd/gc/metrics_lifecycle_test.go`, replacing the ambient `gc events --json` failure case with mutually exclusive `gc events --after 1 --after-cursor x` flags. Code inspection confirmed `cmd/gc/cmd_events.go` checks that mutual exclusion at the start of `RunE`, before seq/follow/watch/plain branches can touch city or supervisor state. `GC_CITY=/home/jaword/projects/gc-management go test ./cmd/gc -run '^TestProductMetricsLifecycleCommandPathMatrixAttemptsOnce$' -count=5` passed. `gofmt -l cmd/gc/metrics_lifecycle_test.go` produced no output. `git grep -n -E 'jsonl failure|jsonl_failure' -- '*.go'` produced no output. |
| 3 | Tests pass | PASS | `TMPDIR=/var/tmp/gd4 make test-fast-parallel` passed all fast jobs. `TMPDIR=/var/tmp/gd4 go vet ./...` passed. A prior fast run with a long deployer TMPDIR failed from Unix socket path length (`bind/connect: invalid argument`); rerunning with the short `/var/tmp/gd4` path passed. |
| 4 | No high-severity review findings open | PASS | Review bead notes contain a PASS verdict and no open HIGH findings. The deployer inspection found no additional high-severity issue in this test-only change. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` in `/var/tmp/gascity-builder-ga-m1uo4w` showed a clean `builder/ga-m1uo4w-fix-events-json-deprecation-test` branch tracking `origin/builder/ga-m1uo4w-fix-events-json-deprecation-test`. The only pending change before commit is this release-gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main origin/builder/ga-m1uo4w-fix-events-json-deprecation-test` returned `rc=0` and tree `1841cb3761b30e20ad599a7e7640bb03798a60f5`. |
| 7 | Single feature theme | PASS | The effective diff from `origin/main` is a one-line test-case replacement in `cmd/gc/metrics_lifecycle_test.go`, scoped to the product-metrics lifecycle matrix for the `gc events` command path. |

## Test Commands

```bash
TMPDIR=/var/tmp/gascity-deployer-ga-d4c2in-tmp GC_CITY=/home/jaword/projects/gc-management go test ./cmd/gc -run '^TestProductMetricsLifecycleCommandPathMatrixAttemptsOnce$' -count=5
TMPDIR=/var/tmp/gd4 make test-fast-parallel
TMPDIR=/var/tmp/gd4 go vet ./...
gofmt -l cmd/gc/metrics_lifecycle_test.go
git grep -n -E 'jsonl failure|jsonl_failure' -- '*.go'
```
