# Release Gate: SSH production constructor conformance

- Deploy bead: `ga-03qxrh`
- Review bead: `ga-uz5t3a.5`
- Reviewed source commit: `f189d1d889722f6b398f354e0551f6602445fb81`
- Base checked: `origin/main` at `2068171b4ad2c1a742babbaaffda20836de97d00`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist
applies the release criteria supplied in the deployer instructions and the
repository's documented test targets.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | The review bead records `verdict: pass` for the exact reviewed commit above. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `TestSSHConformance` directly runs the shared runtime-provider contract against `ssh.NewSeamBacked`. A test-owned `ssh` executable exercises the production `shellRunner` while per-subtest fixtures preserve one isolated `PATH` and `TMUX_TMPDIR` across repeated factory calls, explicitly clear and restore `TMUX`, and require no network, passwordless localhost, user SSH configuration, or real `known_hosts`. The `runtime.builtin.ssh` ledger row changes from waived to proved, names this exact test, retains the focused runner tests, and is synchronized into `TESTING.md`. The SSH package and provider-ledger synchronization tests passed. |
| 3 | Tests pass | PASS | The documented full-scope command below completed all 40 jobs: **38 PASS / 2 raw FAIL / 0 skipped jobs**; top-level test events were **49,912 PASS / 3 raw FAIL / 210 SKIP**. The diff-owned `TestSSHConformance` passed by name with 34 nested PASS events and no FAIL or SKIP. All three raw test failures satisfy criterion 3a's four clauses and are attributed below to trackers that predate this run. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and `TestCleanInstallTutorialPath` hit shared-server Beads schema-migration refusals tracked by `ga-esyijp`; the SSH test/ledger diff cannot affect the installed Beads schema or initialization path. `TestE2E_SuspendResume_City` hit the exact 94.51-second missing-`citysus.report` signature tracked by `ga-dc9utn`; that tracker's proven root cause is the untouched reconciler suspend/wake path. Each failing test file is outside the diff, each tracker is open and predates the run, mechanism proof landed, and there is no path overlap. Exact sightings were appended to both trackers and read back. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `go build ./...`, `go vet ./...`, `LINT_CHANGED_REF=origin/main make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exited 0. `policy_lane: make test-ci-policy — PASS`. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a (no CI configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded an exact-SHA PASS, no security findings, and no blocking findings. Unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate, detached `HEAD` was exactly the reviewed commit and `git status --porcelain=v1` produced no output. The gate file is the only deployer-authored change and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated before the test run and rechecked after fetching the newer base. `git merge-tree --write-tree origin/main f189d1d889722f6b398f354e0551f6602445fb81` exited 0 and produced `5aefe04db1ea8b485fe0b909f8fc15589e70b6ab` against the base above. The candidate is four commits behind and one ahead; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The three-file diff has one theme: prove the production SSH constructor through a hermetic client boundary and synchronize its provider-governance record. It changes integration tests, governance metadata, and generated test documentation only; no production path changes. |

## Full-suite evidence

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GO_TEST_TIMEOUT=30m \
LOCAL_TEST_JOBS=4 \
GOFLAGS=-v \
make test-local-full-parallel

test_cmd_scope: full-suite
runner jobs: 38 PASS / 2 raw FAIL / 0 SKIP
top-level test events: 49,912 PASS / 3 raw FAIL / 210 SKIP
all test events: 88,345 PASS / 3 raw FAIL / 305 SKIP
logs: /var/tmp/ga-03qxrh-full-suite-20260911.log
job logs: /var/tmp/gc-local-tests.F40CvC
```

The rootless Podman socket was live before the run. The Testcontainers module's
pinned `docker.io/dolthub/dolt-sql-server:1.32.4` tag was refreshed and resolved
to digest `sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d`;
the repository-pinned `docker.io/dolthub/dolt:2.1.7` image was present at digest
`sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`.
Ryuk remained disabled because this host uses the external testcontainer sweep.

The 210 top-level skips are suite-controlled platform, privilege, live-provider,
helper-process, and opt-in persistence cases. Examples include unsupported-OS
checks, helper-process sentinels, live catalog canaries, root-only permission
tests, and persistence tests requiring explicit opt-in. Process-backed cases
skipped by the fast lanes are exercised by the full command's corresponding
process and integration shards. The diff-owned test did not skip.

### Diff-owned tests

- `TestSSHConformance`: PASS in `integration-packages-core-1-of-4`, including
  34 nested PASS events and zero nested FAIL/SKIP events. The direct full SSH
  package run also passed without a run filter; the pre-existing real-localhost
  test correctly skipped because passwordless localhost SSH is unavailable.
- `TestCatalogMatchesProductionWiringAndDocumentation`: PASS in both the unit
  and integration core lanes and in the direct provider-ledger package run.
- Supporting waiver guards `TestRuntimeWaiverExpiriesDivergeByGap` and
  `TestRuntimeWaiverExpiriesAreWholeDaysInOrder`: PASS.

`diff_tests_executed: TestSSHConformance PASS`.

### Failure attribution

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-esyijp
  clause 3: mechanism — fresh managed init refused 28 shared-server schema
  migrations (v38 -> v66); the SSH/provider-ledger diff cannot reach or alter
  that installed Beads condition

failure_attribution: TestCleanInstallTutorialPath -> ga-esyijp
  clause 3: mechanism — tutorial rig init refused 8 shared-server schema
  migrations (v58 -> v66); the SSH/provider-ledger diff cannot reach or alter
  that installed Beads condition

failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn
  clause 3: mechanism — exact tracked missing-citysus.report timeout; the proven
  reconciler suspend/wake root-cause paths are untouched by this diff
```

For all three: clause 1 is clear because the failing test files are untouched;
clause 2 is satisfied by open trackers created before this run; clause 3 has a
mechanism proof; and clause 4 is clear because neither `cmd/gc/cmd_bd_test.go`
nor `test/integration/**` overlaps the candidate's files.

## Acceptance and static evidence

```text
go test -tags=integration ./internal/runtime/ssh/... -count=1 -v
PASS — full package, including TestSSHConformance and all contract subtests

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
TESTING.md                                      |   2 +-
internal/runtime/ssh/conformance_integration_test.go | 120 +++++++++++++++++++++
internal/testutil/providerledger/ledger.go      |   9 +-
3 files changed, 127 insertions(+), 4 deletions(-)
```
