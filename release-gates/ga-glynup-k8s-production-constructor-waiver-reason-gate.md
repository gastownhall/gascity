# Release gate: precise K8s production-constructor waiver evidence

- Deploy bead: `ga-glynup`
- Reviewed work bead: `ga-uz5t3a.2`
- Reviewed commit: `b3f78c3c40d15ea75464206ed781e4f176cbd36b`
- Source branch: `builder/ga-uz5t3a.2` (provenance only)
- Base: `origin/main@48d68d13a77647eedeeabe772f362a32954a53b2`
- Planned deploy branch: `deploy/ga-glynup-gate`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-11
- Verdict: **PASS with attributed, non-diff-owned raw test failures**

The target pre-flight ran before criterion 6. GitHub's commit-to-pulls API
returned no pull request for the reviewed commit, so there was no merged or
closed PR to reconcile. The repository does not contain
`docs/PROJECT_MANIFEST.md`; this checklist applies the release criteria carried
by the deployer formula and the repository's release-gate conventions.

## Gate checklist

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | Deploy bead `ga-glynup` records `REVIEW VERDICT: PASS` at the exact resolved commit `b3f78c3c40d15ea75464206ed781e4f176cbd36b`. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The live registry still selects `internal/runtime/k8s.NewSeamBacked`. The entry remains `waivedRuntime`, owned by `ga-80po0c.3`, through 2026-11-12. Its reason now names the exact missing proof: no runnable harness exercises `NewSeamBacked()` against a live Kubernetes API plus pod-exec lifecycle; package tests use `newProviderWithOps(fake)` and there is no kind/integration-tagged harness in `internal/runtime/k8s`. The generated `TESTING.md` row matches the ledger. `TestCatalogMatchesProductionWiringAndDocumentation` passed twice in the full-suite output. |
| 3 | Tests pass | **PASS with attribution** | The documented full-scope 40-job command completed **36 PASS / 4 raw FAIL / 0 SKIP jobs**. The diff modifies no test file, and the existing acceptance-specific docsync test passed twice. The four raw failures are attributed under criterion 3a. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributable | **PASS** | Every failure is outside the two-file candidate diff, maps to a record opened before this run, has no failing-path overlap, and is structurally unreachable from a provider-ledger reason string plus its rendered Markdown row. Two shared-server schema refusals map to `ga-esyijp`; the city suspend/resume report timeout maps to `ga-dc9utn`; and the dynamic-order initial-reconcile timeout maps to `ga-bf90h9`. This run's sightings were appended to and read back from all three records. |
| 3b | Policy and static lanes | **PASS** | `go build ./...`, `go vet ./...`, `make test-ci-policy`, `make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exited 0. The repository hook path is `.githooks`. `policy_lane: make test-ci-policy — PASS`. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, required-check list, Makefile, runner script, or test-resource census changed)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The reviewer recorded an unconditional PASS after independently checking scope, security, the constructor path, and the exact harness gap. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty after the full suite and all static lanes, before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, the reviewed commit is 0 behind and 1 ahead of `origin/main`. `git merge-tree --write-tree origin/main b3f78c3c40d15ea75464206ed781e4f176cbd36b` exited 0 and produced tree `7648ad75a1fed3b7be7a32c381caa996a85380ca`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One commit changes one K8s provider-ledger waiver reason and its generated `TESTING.md` mirror. It introduces no independent behavior or unrelated subsystem change. |

## Source and acceptance evidence

The recorded SHA resolved to the full reviewed commit:

```text
git rev-parse --verify --quiet 'b3f78c3c40d15ea75464206ed781e4f176cbd36b^{commit}'
b3f78c3c40d15ea75464206ed781e4f176cbd36b
```

The reviewed range is one commit and two one-line substitutions:

```text
b3f78c3c40d15ea75464206ed781e4f176cbd36b fix(providerledger): name the K8s API/pod-exec harness gap in the waiver reason (ga-uz5t3a.2)
```

Paths are confined to `internal/testutil/providerledger/ledger.go` and the
generated `TESTING.md` ledger table. The change leaves the waiver owner,
expiry, disposition, constructor reference, runtime registration, and every
test target unchanged. A repository search confirmed that neither `cmd/gc/**`
nor `test/integration/**` imports `internal/testutil/providerledger`; the four
failing test paths therefore cannot execute the changed ledger value.

## Full-suite test evidence

The rootless Podman socket was active before the run. The repository-pinned
`docker.io/dolthub/dolt-sql-server:2.1.7` image was pulled and verified in the
local cache, and Testcontainers Ryuk remained disabled as required by the host
cleanup contract.

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GO_TEST_TIMEOUT=30m \
LOCAL_TEST_JOBS=4 \
GOFLAGS=-v \
make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 36 PASS jobs / 4 raw FAIL jobs / 0 SKIP jobs (40 total)`
- Top-level Go test events: 49,910 PASS / 4 FAIL / 210 SKIP.
- All Go test and subtest events: 88,268 PASS / 4 FAIL / 300 SKIP.
- Full command log: `/var/tmp/ga-glynup-full-suite.log`
- Per-job logs: `/var/tmp/gc-local-tests.uv7i2n`
- `diff_tests_executed: none (no test files in diff)`
- Acceptance-specific existing test: `TestCatalogMatchesProductionWiringAndDocumentation` — PASS twice, in `unit-core` and `integration-packages-core-3-of-4`.
- `skip_justification`: expected platform, privilege, opt-in live-provider, helper-process, persistence-contract, and shard-control skips. In particular, `TestK8sSessionConformance` skips without `GC_SESSION_K8S_SCRIPT`; this change accurately retains a waiver precisely because no direct runnable production-constructor harness exists. No diff-owned test was added or modified.
- `waiver_ref: none`

## Failure attribution

| Raw result | Test | Tracker and proof |
| --- | --- | --- |
| **FAIL — ATTRIBUTED** | `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `ga-esyijp`, open since 2026-08-29. Clause 3(a), mechanism: external `bd init` refused shared-server migration `v57 -> v66`. The candidate's provider-ledger reason is not imported by `cmd/gc/**` and cannot affect schema initialization. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestGCLiveContract_BeadsAndEvents` | `ga-esyijp`. Clause 3(a), same mechanism: external `bd init` refused shared-server migration `v48 -> v66` before the live-contract assertion. The candidate is unreachable from `test/integration/**`. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestE2E_SuspendResume_City` | `ga-dc9utn`, open before this run with the root cause and fix owner recorded. Clause 3(a), mechanism: the 93.62-second missing `citysus.report` timeout exercises session suspend/resume; neither failing path imports or reads the changed provider ledger. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestControllerDiscoversAddedCronOrderWithoutRestart` | `ga-bf90h9`, open since 2026-08-19 and dedicated to this exact initial-reconcile timeout. Clause 3(a), mechanism: order discovery timed out after 5.90 seconds waiting for the initial controller reconcile; the test path does not import or read `internal/testutil/providerledger`. No path overlap. |

`inconclusive-guard: n/a — a clause-3 mechanism proof landed for every raw
failure, and the candidate changes neither the test-resource census nor any
test target.`

## Disposition

All applicable release criteria pass. Commit this checklist as the sole
deploy-only addition on the isolated `deploy/ga-glynup-gate` branch, push the
exact gated head, open an internal pull request, publish
`release-gate/deploy-clearance=success` on that exact PR head, and route merge
authority to the mayor. The deployer does not merge.
