# Release Gate: ACP default shared-directory constructor

- Deploy bead: `ga-t1vq5b`
- Review bead: `ga-uz5t3a.10`
- Reviewed source commit: `134eabbfef3cb0705ec3999235ac5ce32e8c4839`
- Base checked: `origin/main` at `3d268c5849ca2ef49392c6e4adb68e9a10d0b59d`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist
applies the release criteria supplied in the deployer instructions and the
repository's documented test targets.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | The review bead records `verdict: pass` for the exact reviewed commit above. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | The city-less `acp` registration still constructs `NewSeamBacked`; the city-backed route still constructs `NewSeamBackedWithDir`. `TestACPDefaultDirConformance` directly runs the full provider contract against `NewSeamBacked` with the existing fake ACP fixture, PID-plus-counter names, no injected directory, no environment mutation, and no skip/options waiver. The ledger remains waived pending a clean Darwin run, names the still-open blocker `ga-csh74h`, and is synchronized into `TESTING.md`. The shared owner and 2026-10-08 expiry implement the mayor's recorded 2026-09-10 amendment, avoiding the stale owner override and earlier expiry from the pre-rebase version. |
| 3 | Tests pass | PASS | The documented full-scope command below completed all 40 jobs: **37 PASS / 3 raw FAIL / 0 skipped jobs**; top-level test events were **49,912 PASS / 3 raw FAIL / 210 SKIP**. Both diff-owned tests passed by name. All three raw failures satisfy criteria 3a's four clauses and are attributed below to trackers that predate this run. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and `TestGCLiveContract_BeadsAndEvents` hit the shared-server Beads schema-migration refusal tracked by `ga-esyijp`; the candidate cannot affect the installed Beads schema or its initialization path. `TestE2E_SuspendResume_City` hit the exact 93-second missing-`citysus.report` signature tracked by `ga-dc9utn`; that tracker's proven root cause is the untouched reconciler suspend/wake path. Each failing test file is outside the diff, each tracker is open and predates the run, mechanism proof landed, and there is no path overlap. Both tracker sightings were appended and verified by their increased comment counts. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `go build ./...`, `go vet ./...`, `LINT_CHANGED_REF=origin/main make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exited 0. `policy_lane: make test-ci-policy — PASS`. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a (no CI configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no security or blocking findings and an exact-SHA PASS verdict. Unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate, detached `HEAD` was exactly the reviewed commit and `git status --porcelain=v1` produced no output. The gate file is the only deployer-authored change and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated before the test run and rechecked after it. `git merge-tree --write-tree origin/main 134eabbfef3cb0705ec3999235ac5ce32e8c4839` exited 0 and produced `a2d856a932cb38a537b23a21f27d5c1995d85c60` against the base above. The candidate is three commits behind and two ahead; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The four-file diff has one theme: directly exercise the ACP default shared-directory constructor and keep its provider-ledger waiver evidence synchronized. It changes tests, governance metadata, and generated test documentation only; no production path changes. |

## Full-suite evidence

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GO_TEST_TIMEOUT=30m \
LOCAL_TEST_JOBS=4 \
GOFLAGS=-v \
make test-local-full-parallel

test_cmd_scope: full-suite
runner jobs: 37 PASS / 3 raw FAIL / 0 SKIP
top-level test events: 49,912 PASS / 3 raw FAIL / 210 SKIP
all test events: 88,304 PASS / 3 raw FAIL / 300 SKIP
logs: /var/tmp/ga-t1vq5b-full-suite-20260911.log
job logs: /var/tmp/gc-local-tests.rDggih
```

The rootless Podman socket was live before the run. The Testcontainers module's
pinned `docker.io/dolthub/dolt-sql-server:1.32.4` tag was refreshed and resolved
to digest `sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d`;
the repository-pinned `docker.io/dolthub/dolt:2.1.7` image was also present.
Ryuk remained disabled because this host uses the external testcontainer sweep.

The 210 top-level skips are suite-controlled platform, privilege, live-provider,
helper-process, and opt-in persistence cases. Examples include unsupported-OS
checks, external watchdog helpers, the unset Kubernetes conformance script,
root-only permission tests, and persistence tests requiring an explicit opt-in.
Neither diff-owned test skipped.

### Diff-owned tests

- `TestACPDefaultDirConformance`: PASS in `integration-packages-core-1-of-4`;
  the direct acceptance rerun also passed all provider-contract subtests.
- `TestCatalogBindsACPWithDirAndDefersDefaultConstructor`: PASS in both the
  unit-core and integration-core lanes; the direct full provider-ledger package
  rerun also passed.
- Supporting synchronization and expiry guards
  `TestCatalogMatchesProductionWiringAndDocumentation`,
  `TestRuntimeWaiverExpiriesDivergeByGap`, and
  `TestRuntimeWaiverExpiriesAreWholeDaysInOrder`: PASS.

`diff_tests_executed: TestACPDefaultDirConformance PASS;
TestCatalogBindsACPWithDirAndDefersDefaultConstructor PASS`.

### Failure attribution

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-esyijp
  clause 3: a — fresh managed init refused 14 shared-server schema migrations
  (v52 -> v66); the ACP/provider-ledger diff cannot reach or alter that condition

failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-esyijp
  clause 3: a — real-world app city init refused 7 shared-server schema migrations
  (v59 -> v66); the ACP/provider-ledger diff cannot reach or alter that condition

failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn
  clause 3: a — exact tracked citysus.report timeout; the proven reconciler
  suspend/wake root-cause paths are untouched by this diff
```

For all three: clause 1 is clear because the failing test files are untouched;
clause 2 is satisfied by open trackers created before this run; clause 3 has a
mechanism proof; and clause 4 is clear because neither `cmd/gc/cmd_bd_test.go`
nor `test/integration/**` overlaps the candidate's files.

## Acceptance and static evidence

```text
go test -tags=integration ./internal/runtime/acp \
  -run 'TestACP(Conformance|DefaultDirConformance)' -count=1 -v
PASS — both conformance entry points and all subtests

go test ./internal/testutil/providerledger/... -count=1 -v
PASS — full package, including ledger/document synchronization

go build ./...
PASS

go vet ./...
PASS

make test-ci-policy
PASS

LINT_CHANGED_REF=origin/main make fmt-check-changed
PASS

git diff --check origin/main...HEAD
PASS
```

## Scope evidence

```text
TESTING.md                                      |  2 +-
internal/runtime/acp/conformance_test.go        | 28 +++++++++++++++++++++++++
internal/testutil/providerledger/ledger.go      | 10 ++++++++-
internal/testutil/providerledger/ledger_test.go | 18 +++++++++++++++-
4 files changed, 55 insertions(+), 3 deletions(-)
```
