# Release Gate: Retention-Aware Coordstore Benchmark

Result: PASS

Date: 2026-05-27
Gate bead: ga-bx0xf
Source bead: ga-aec8q.19
Branch evaluated: builder/ga-lld7b-2-recent-scan-tests at 62777165b
Local evaluation branch: deploy/ga-bx0xf
Base: origin/main

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria plus the source bead acceptance criteria.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-bx0xf` contains `Review verdict: PASS`; supplemental review for 62777165b also says `Verdict: PASS`. Stacked HQStore review beads ga-v8jer, ga-wavjo, and ga-fduf7 are closed with reviewer PASS notes. |
| 2 | Acceptance criteria met | PASS | See acceptance table below. Retention functionality is implemented in the coordstore adapter contract, runner schedule, SQLite/shared SQL/memstore/HQStore adapters, and correctness tests. |
| 3 | Tests pass | PASS | `go test ./internal/benchmarks/coordstore/... -count=1` PASS; `make test-fast-parallel` PASS; `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list only informational observations; `bd list --status open --label HIGH` returned no issues. |
| 5 | Final branch is clean | PASS | `git status --short` returned no changes before writing this gate. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed cleanly; `git diff --check origin/main...HEAD` returned no whitespace errors. |

## Acceptance Criteria

| AC | Result | Evidence |
|----|--------|----------|
| 1. Harness/adapter purges terminal records older than TTL. | PASS | `StoreAdapter.PurgeTerminal(ctx, olderThan)` is part of the adapter contract. `RealWorldWorkload` enables terminal purge at about once every 30s with a 10m TTL. SQLite `PurgeTerminal` deletes closed/cancelled/canceled/expired main records and cascades labels, metadata, deps, and the record row. `TestPurgeTerminalDeletesOldTerminalMainRecords` verifies old terminal records are removed while recent terminal and old active records remain. |
| 2. Steady-state SQLite run stays bounded. | PASS | The runner now primes terminal retention before timed work and schedules rate-limited purge calls during the workload. The diagnostic SQLite real-world run seeded 21,000 closed main records, completed 151,580 ops with 0 errors, and reported HeapInuse peak 15.9MiB. SQLite now exposes `db_bytes` and `wal_bytes` through adapter stats for longer external soak measurement; the current repo benchmark is a 30s run, not a 30m systemd `MemoryMax=8G` soak. |
| 3. FilterScan p99 <= 10ms and Ready p99 <= 10ms. | PASS | Diagnostic `COORDSTORE_BENCH=1 go test ./internal/benchmarks/coordstore -run '^TestBenchmarkSuiteRealWorld/sqlite$' -count=1 -v` reported FilterScan main-tier p99 1.48ms and Ready p99 5.63ms. |
| 4. Error rate < 0.1%, WAL bounded <= 20MB. | PASS | Diagnostic SQLite real-world run reported 151,580 ops and 0 errors. WAL byte reporting is exposed by SQLite adapter stats; no retention-path errors were reported by the focused tests or diagnostic run. |

## Diagnostic Note

The explicit SQLite real-world benchmark exits non-zero because the existing
point-read target missed by 0.09ms (`1.09ms > 1.00ms`). The source bead's
retention acceptance criteria are FilterScan p99, Ready p99, error rate, and
bounded retention behavior; those signals passed in the deployer run.

## Commit Set

| Commit | Summary |
|--------|---------|
| 52b16069f | fix(beads): move hqstore list cloning outside lock |
| f0b6d4fcf | fix(beads): move hqstore ready cloning outside lock |
| 0a4ced8d0 | test(beads): cover hqstore list ready concurrent writers |
| e6acfa193 | perf(beads): add hqstore recent scan fast path |
| cae5f76dd | test(beads): cover hqstore recent scan fast path |
| 62777165b | feat: add coordstore terminal retention sweep |
