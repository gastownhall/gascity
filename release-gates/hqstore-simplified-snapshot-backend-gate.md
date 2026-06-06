# Release Gate: HQStore Simplified Snapshot Backend

Superseded 2026-05-31: SQLite-CGo is the selected coordination-store backend;
HQStore code was removed under `ga-r1jzbn`. This file remains as historical
release-gate evidence.

Date: 2026-05-24

## Target

- Release bead: `ga-caqkj` — Review: HQStore simplified snapshot backend
- Implementation bead: `ga-cidt5` — HQStore: implement simplified design (AsyncSnapshotter, no WAL) + RAM benchmarking
- Branch: `builder/ga-cidt5-1`
- Reviewed commit: `da8ee46a9fe05577740a2fe17ab2db5eb9c2e7c3`
- Base: `origin/main` at `fa116f0f2`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-caqkj` notes contain `Claude Reviewer Verdict: PASS` and `Verdict: PASS -> needs-deploy` for branch `builder/ga-cidt5-1` at `da8ee46a9`. |
| 2 | Acceptance criteria met | PASS | `internal/beads/hqstore*.go` implements a dormant snapshot-backed HQStore with async snapshot flush/load, TTL expiry, and closed-task retention. `internal/beads/hqstore_test.go` covers conformance, crash recovery, snapshot round trip, periodic flush, TTL expiry, retention skip with open children, disabled retention, and concurrent create/update. `internal/benchmarks/coordstore/` adds the benchmark harness, HQStore adapter, memory sampler, `MemReport`, and 256MB `HeapInusePeak` target. |
| 3 | Tests pass | PASS | `go test ./internal/beads ./internal/benchmarks/coordstore/...` passed. `go test -race ./internal/beads -run TestHQStoreConcurrentCreateUpdate` passed. `go vet ./...` passed. `make test-fast-parallel` passed on rerun. The first broad run hit a transient `internal/orders` heartbeat timeout; the failing test passed immediately with `go test ./internal/orders -run TestCheckTriggerConditionKillsProcessGroupOnTimeout -count=1 -v`. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list one LOW performance finding, one LOW spec-gap note, and INFO observations. No HIGH findings are present. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` reported `## builder/ga-cidt5-1...origin/builder/ga-cidt5-1` with no uncommitted files. This gate file is the only deployer change to commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed successfully and returned tree `444aa1fab0015c84a124688240b08933639120ae`, with no conflicts. |

## Review Notes

- The shipped code is dormant and opt-in; it does not replace the live city bead store.
- The reviewer called out a harmless extra clone in `Ready()` and a future import-path consideration around closed-task timestamps. Neither blocks release.
- `docs/PROJECT_MANIFEST.md` is absent in this checkout, so the release gate used the deployer prompt criteria plus the repository's `TESTING.md` guidance for local gates.

## Decision

PASS. Open a PR from `builder/ga-cidt5-1` to `main`.
