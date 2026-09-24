# Release gate: mol-dog-stale-db actor guard re-gate

- Re-gate bead: `ga-t5adko`
- Original deploy bead: `ga-qhr85l`
- Review bead: `ga-je7i97` — PASS
- Build bead: `ga-b15hwm`
- Reviewed fix: `c895cb82b1f7b5f305fac31342a3195987fbd3cb`
- Prior cleared head: `fc86445c296620498067f0187649a3e6b585dfa7`
- Maintainer-review-updated PR head: `3ece21b728182ae4686be1268752c2d98d04c2cb`
- Current-main gated candidate: `d469cb007fb0b459070c2384e79ad972b17465f2`
- Base: `origin/main@2898f5467f807a469c3aa0358a78a23772f1f753`
- Pull request: `https://github.com/gastownhall/gascity/pull/6375`
- Deploy mode: `remote`; push remote: `origin`; existing PR branch: `deploy/ga-je7i97-gate`
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-je7i97` records PASS for `c895cb82b1f7b5f305fac31342a3195987fbd3cb`. Per the mayor's `ga-t5adko` ruling, maintainer-pr-review's test-only pin at `3ece21b728182ae4686be1268752c2d98d04c2cb` also counts as reviewed; the live-census bookkeeping is explicitly exempt from re-review. |
| 2 | Acceptance criteria met | **PASS** | The stale-DB formula closes its work bead with the nounset-safe `GC_ALIAS -> GC_SESSION_NAME -> GC_SESSION_ID -> empty` actor chain and without `--force`. The maintainer test pins both a populated alias and the blank-alias/session-name fallback. The fleet guard rejects raw guarded `bd` mutations lacking `--actor`, `--force`, or a reviewed acknowledgement. All 17 diff-owned tests passed twice in the full union. |
| 3 | Tests pass | **PASS with two attributed raw failures** | The documented isolated full local union completed all 40 jobs: **38 green / 2 attributed raw failures**, with **51,221 PASS / 2 FAIL / 227 SKIP** top-level test executions. Both failures stopped in fixture `bd init` on the pre-existing shared-server random-cursor migration-refusal condition tracked by `ga-lejnse` and root-fix bead `ga-e2z1zb`, before candidate behavior. They meet non-diff attribution clauses 1, 2, 3(a), and 4 as detailed below. All required build, vet, policy, boundary, census, formatting, and vulnerability lanes passed. `test_cmd_scope: full-suite`; `waiver_ref: none`; `authorization_ref: ga-lejnse standing root-cause protocol`; `ci_lane_run: n/a (no CI configuration change)`. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-je7i97` records no unresolved HIGH, security, or specification blocker. The required exact workflow vulnerability scan was rerun on the current-main candidate and found **0 reachable** and **0 imported-package** vulnerabilities; its three module-only advisories are informational under the workflow policy. |
| 5 | Final branch is clean | **PASS** | Immediately before this record, `git status --porcelain=v1`, `git diff --check origin/main...HEAD`, and the census repository-ledger test were clean/PASS. This record is the only subsequent file change and is committed on the existing isolated PR branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After the final fetch and merge from `origin/main@2898f5467f807a469c3aa0358a78a23772f1f753`, `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `4c3dd6ef08ba02ceee7ce05f5f670ab95f3b5a2a`. The only merge conflicts were the three expected census ledgers and were resolved to their live combined totals. |
| 7 | Single feature theme | **PASS** | The non-main delta is one formula-safety theme: the reviewed close actor fix and static guard, its prior gate record, the maintainer-review regression pin, this re-gate evidence, and the census counters required by those added tests plus current main. No unrelated production behavior was added. |

## Content identity and current-main ratchet

- `examples/bd/dolt/formulas/mol-dog-stale-db.toml` is byte-identical to its blob in reviewed fix `c895cb82b1` (`4d062f8090...`).
- `test/packlint/bd_raw_mutation_actor_test.go` is byte-identical to its blob in reviewed fix `c895cb82b1` (`8a6ee80ffa...`).
- `release-gates/ga-qhr85l-mol-dog-stale-db-actor-guard-gate.md` is byte-identical to its blob in prior cleared head `fc86445c29` (`640f70...`).
- `examples/bd/dolt/stale_db_formula_test.go` is byte-identical to its blob in maintainer-review head `3ece21b728` (`cb00420aae...`).
- Beyond those four authorized blobs, the only candidate changes are the live resource-census totals in `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, and `test/test-resources.toml`.
- Before ratcheting, the census reported three exact one-call drifts. After merging the final current main, the authoritative totals are: all-source subprocess **669 calls / 194 files**, untagged-source subprocess **451 calls / 130 files**, and small untagged subprocess **442 calls / 124 files**. `go test ./internal/testpolicy/resourcecensus` then passed.

## Criterion 3 evidence

- `test_cmd`: rootless Podman environment (`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, `GOFLAGS=-v`, `GO_TEST_TIMEOUT=30m`, `LOCAL_TEST_JOBS=4`, `CMD_GC_PROCESS_TOTAL=6`, `LOCAL_TEST_LOG_DIR=/var/tmp/ga-t5adko-final-logs`) with `packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`.
- `test_cmd_scope: full-suite`
- `test_counts: 51,221 PASS / 2 raw FAIL / 227 SKIP`; job result `38 PASS / 2 raw FAIL / 0 omitted`.
- `required_job_coverage`: all 40 documented jobs started and completed, including unit, six `cmd/gc` process shards, product metrics, package and `cmd/gc` integration shards, all tmux shards, review-formula shards, bd-store, and all ten REST shards.
- `diff_tests_executed`: all 17 tests in the two diff-owned test files ran twice, for **34 PASS / 0 FAIL / 0 SKIP**:
  - `TestStaleDBFormulaRuntimeContract`
  - `TestStaleDBFormulaRenderedShellIsStrictAndValid`
  - `TestStaleDBFormulaApplyErrorsLeaveWorkOpen`
  - `TestStaleDBFormulaApplyCommandFailureAppendsApplyJSON`
  - `TestStaleDBFormulaDryRunFailureAppendsScanJSON`
  - `TestStaleDBFormulaCleanApplyClosesWorkAndUsesDBThreshold`
  - `TestStaleDBFormulaCloseUsesSessionNameWhenAliasBlank`
  - `TestStaleDBFormulaResolvesWorkBeadViaHookCurrentChain`
  - `TestStaleDBFormulaFailsLoudlyWhenNoWorkBeadIDResolvable`
  - `TestStaleDBFormulaPurgeOnlyScanApplies`
  - `TestStaleDBFormulaPurgeOnlyApplySQLFailureLeavesWorkOpen`
  - `TestStaleDBFormulaExitZeroMaxOrphanRefusalLeavesWorkOpenWithoutSuccessEvents`
  - `TestStaleDBFormulaDryRunForceBlockersLeaveWorkOpenBeforeApply`
  - `TestStaleDBFormulaFailurePathsDrainAck`
  - `TestStaleDBFormulaSuccessPathFailuresDrainAck`
  - `TestStaleDBOrderUsesParsedFieldsOnly`
  - `TestRawBdMutationRequiresActorOrForce`
- `skip_justification`: the 227 skips are existing suite-controlled platform, live-provider, opt-in, helper-process, and tier-selection exclusions. The full union ran the process and integration lanes referenced by unit-tier skip messages. No skip was diff-owned.
- `waiver_ref: none`
- `authorization_ref: ga-lejnse`, whose standing notes identify this random-cursor shared-Dolt refusal and direct gate attribution through root-fix bead `ga-e2z1zb`.
- `ci_lane_run: n/a (no workflow, matrix, timeout, or required-check configuration changed)`.
- `make test-ci-policy` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected fmt-check-changed` — PASS, 0 issues.
- `make build` — PASS.
- `go vet ./...` — PASS.
- `govulncheck -show verbose ./...` using workflow-matching `govulncheck v1.7.0` — PASS: 0 reachable vulnerabilities, 0 imported-package vulnerabilities, 3 module-only advisories.
- `make check-core-boundary` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-residency-boundary` (including its self-test) — PASS.
- `make check-gomod-replace` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-routed-test-rows check-split-topology-rows` — PASS.
- `make check-release-dist-ignore` — PASS.
- `make check-hooks` — PASS (`core.hooksPath` is `.githooks`).
- `git diff --check origin/main...HEAD` — empty/PASS.
- Full log: `/var/tmp/ga-t5adko-final-full.log`.
- Per-job logs: `/var/tmp/ga-t5adko-final-logs`.
- Final vulnerability log: `/var/tmp/ga-t5adko-final-govulncheck.log`.
- Final census log: `/var/tmp/ga-t5adko-census-post-main-green.log`.

### Criterion 3a failure attribution

Both raw failures satisfy all four required clauses:

1. Neither failing test is diff-owned.
2. `ga-lejnse` predates this run and specifically tracks the exact fresh-city shared-Dolt schema-ceiling refusal; its root-cause note points to `ga-e2z1zb`.
3. Mechanism proof **(a)** is conclusive: the only candidate production file is the example formula `examples/bd/dolt/formulas/mol-dog-stale-db.toml`; neither failing integration test imports or references it, and both failures occur in external `bd init` before candidate behavior.
4. The failing `test/integration` package/path does not overlap the candidate formula, `examples/bd/dolt` tests, `test/packlint` guard, or census-ledger paths. The conclusion is unconditional, so the added-test-load fallback guard is not needed.

| Failure | Attribution |
|---|---|
| `TestCleanInstallTutorialPath` | `failure_attribution: integration-rest-full-2-of-8 -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. `gc rig add` stopped in `bd init` when the shared-server guard refused migrations `v50 -> v66`, before tutorial or stale-DB formula behavior. |
| `TestGCLiveContract_BeadsAndEvents` | `failure_attribution: integration-rest-full-5-of-8 -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Rig creation stopped in `bd init` when the shared-server guard refused migrations `v53 -> v66`, before live-contract or stale-DB formula behavior. |

Both sightings were appended to `ga-lejnse` and the re-gate bead without rerunning the product failures. The direct candidate evidence remains clean: all 17 diff-owned tests passed twice.

## Vulnerability hard gate

The same scanner and invocation as `.github/workflows/govulncheck.yml` were used after the final current-main merge: `govulncheck v1.7.0 -show verbose ./...`. It exited 0 with no reachable or imported-package vulnerabilities. The only reports were module-only advisories in `golang.org/x/crypto@v0.55.0` (`GO-2026-6355`, `GO-2026-6354`, and `GO-2026-5932`), which the workflow treats as informational. No finding was waived.
