# Release gate: doctor custom-types HOME isolation

- Deploy bead: `ga-vn396k`
- Build bead: `ga-8pkpor`
- Review bead: `ga-88chom`
- Reviewed source: `e939c519073c6d95f515fb197889d2a7a4628591`
- Gate base: `origin/main@2c3b6d94835b201b839b32d3bc5f219f72e0e6ac`
- Feature merge base: `690675170a1a8b21afb61acb29e5f750a499d530`
- Evaluation date: 2026-07-31
- Disposition: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit. This
checklist applies the deployer role's release criteria, `TESTING.md`, and the
test-evidence requirements in
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after testing. `git merge-tree --write-tree origin/main e939c519073c6d95f515fb197889d2a7a4628591` exited 0 against `origin/main@2c3b6d94835b201b839b32d3bc5f219f72e0e6ac` and produced tree `6ac407c0ce8729a4d96f37384032834f5de91489`. The reviewed source is four commits ahead and two behind current main with no content conflict; no self-rebase was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-88chom` records `REVIEWER VERDICT: PASS` for exact source `e939c519073c6d95f515fb197889d2a7a4628591`. The deploy bead repeats the reviewed SHA and PASS handoff. |
| 2 | Acceptance criteria met | **PASS** | `TestCustomTypesCheck_TableDrift` pins a `t.TempDir` HOME before its first `bd` subprocess; both custom-types fixtures scrub the three shared-server environment selectors; and the new regression proves that HOME has no `.beads/config.yaml`, metadata selects embedded Dolt with a non-empty database, and command output contains no shared-server routing text. The exact `HOME=/home/jaword` feature smoke passed **7 PASS, 0 FAIL, 0 SKIP**. The resource-census mirror check passed **1 PASS, 0 FAIL, 0 SKIP**. The live server remained PID 142645 on port 3308, and its database list was byte-identical before and after: `beads_global`, `dolt`, `information_schema`, `mysql`. No operator config or shared-server database was modified. |
| 3 | Tests pass | **PASS** | `go build ./...` and `go vet ./...` passed. The documented fast CI baseline, `make test-fast-parallel`, reported **10 jobs PASS, 0 FAIL, 0 job-level SKIP**. Because this diff touches `internal/**`, the path-required `make test-cmd-gc-process-parallel` lane was also run with `GC_FAST_UNIT=0`: it selected 8,200 top-level tests across six shards plus the six-test product-metrics job and reported **4 jobs PASS, 3 FAIL, 0 job-level SKIP**. `TestTutorial01` was selected in passing shard 1. The only three failure markers were the already-documented pool/scale-check ambient-HOME set. A focused differential under `HOME=/home/jaword` produced the identical **0 PASS, 3 FAIL, 0 SKIP** set at both merge base and reviewed SHA, including `database "beads" not found ... 127.0.0.1:3308`; with an empty HOME, the same reviewed test binary passed those tests **3 PASS, 0 FAIL, 0 SKIP**. The red shard result is retained as diagnostic evidence, not relabeled green: the unchanged base/tip failure set plus the clean-HOME pass establishes that the reviewed diff adds no regression and matches the clean CI runner condition. |
| 4 | No high-severity review findings open | **PASS** | The reviewer found no security, correctness, compatibility, scope, or blocking issue. The sole non-blocking note suggests future consolidation of the cleanup-retry helper. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, `git status --porcelain=v1 --untracked-files=all` produced no output and `git diff --check origin/main...e939c519073c6d95f515fb197889d2a7a4628591` exited 0. The checklist is the sole deployer-authored source change and will be committed before push. `core.hooksPath` is `.githooks`. |
| 7 | Single feature theme | **PASS** | The four TDD commits change one adjacent doctor-test isolation path plus its mechanically required resource-census mirrors. All four files serve the same behavior: prevent machine-level Dolt shared-server configuration from influencing custom-types tests. No independent feature is bundled. |

## Test evidence

```text
make test-fast-parallel
10 jobs PASS, 0 FAIL, 0 job-level SKIP

make test-cmd-gc-process-parallel
4 jobs PASS, 3 FAIL, 0 job-level SKIP
8,200 selected top-level tests across six GC_FAST_UNIT=0 shards
productmetrics-testhook: PASS (6 selected tests)
TestTutorial01: selected in passing shard 1

Only failure markers:
TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession
TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork
TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork

Focused differential, HOME=/home/jaword:
merge base 690675170: 0 PASS, 3 FAIL, 0 SKIP
reviewed e939c5190: 0 PASS, 3 FAIL, 0 SKIP
identical failure names and 127.0.0.1:3308 signature

Reviewed test binary, empty temporary HOME:
3 PASS, 0 FAIL, 0 SKIP

HOME=/home/jaword go test -json ./internal/doctor \
  -run '^TestCustomTypesCheck' -count=1
7 PASS, 0 FAIL, 0 SKIP

go test -json ./internal/testpolicy/resourcecensus \
  -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1
1 PASS, 0 FAIL, 0 SKIP

go build ./...
PASS

go vet ./...
PASS
```

The process-shard runner reports job outcomes and selected top-level counts,
not per-test skip totals. No job was skipped. The focused feature, census, and
environment-differential runs used JSON or verbose terminal events and had
zero skips.

## Scope evidence

```text
TESTING.md                                   |  11 +--
internal/doctor/checks_custom_types_test.go  | 108 ++++++++++++++++++++++++++-
internal/testpolicy/resourcecensus/census.go |  27 +++++--
test/test-resources.toml                     |  27 +++++--
4 files changed, 150 insertions(+), 23 deletions(-)
```
