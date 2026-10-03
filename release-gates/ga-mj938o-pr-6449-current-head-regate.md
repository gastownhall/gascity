**Verdict:** **PASS**

# Release gate: PR #6449 current-head re-gate (`ga-mj938o`)

- PR: `gastownhall/gascity#6449`
- Exact tested PR head: `122846fededbaa6e48fa0ecc7c3e95ecd9a7d0cd`
- Gate base: `origin/main@4e27de7011f9a5e7928bb9b27cb5fefd6349ae81`
- Prior gated head: `a86782de53fb9d1aa1c24a647d898c9f14d12e0c`
- Original reviewed source: `ad7fe8f245aa9363c3f396eb0b8a3dc2daba956a`
- Original review bead: `ga-3ejr8r`
- Re-gate bead: `ga-mj938o`
- Deploy mode: remote; existing PR head is on `quad341/gascity:deploy/ga-ztk6e9-gate`

This re-gate is required because the maintainer review applied a real correction
after the prior clearance was published. The prior SHA-bound status therefore
does not authorize the current head.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | The original change passed review in `ga-3ejr8r`. The maintainer PR-review ensemble then returned `fix-merge`, applied the routing-identity and graph-v2-parity corrections, and produced exact head `122846fededbaa6e48fa0ecc7c3e95ecd9a7d0cd`. Re-gate bead `ga-mj938o` records that provenance and explicitly assigns this exact current head for a fresh deploy gate. |
| 2 | Acceptance criteria met | **PASS** | Plain-route fallback now warns when a bead remains claimed by a third party and cannot reach the routed target. The warning is emitted in both legacy and graph-v2 formula paths, uses `agentutil.RoutedToIdentity`, and suppresses the false positive when a target pool's own session holds the bead. The assigned, unassigned-control, pool-session, legacy, and graph-v2 regressions all pass. |
| 3 | Tests pass | **PASS with two attributed raw failures** | The documented full local union scheduled all 40 jobs: **38 PASS / 2 raw FAIL / 0 omitted**, comprising **44,514 PASS / 2 raw FAIL / 186 SKIP** top-level executions. All candidate-owned unit, process-backed `cmd/gc`, integration `cmd/gc`, core, sling, and targeted tests passed. Both raw failures stopped during unrelated fixture city initialization on the known shared-Dolt random-cursor race and satisfy criterion 3a below. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. Current-head GitHub CI is green and reports the PR cleanly mergeable. |
| 4 | No high-severity review findings open | **PASS** | The maintainer review's two findings are resolved in `122846fededbaa6e48fa0ecc7c3e95ecd9a7d0cd`: routing identity now follows the canonical writer/reader identity, and graph-v2 matches the legacy warning behavior. No unresolved high-severity finding is recorded. |
| 5 | Final branch is clean | **PASS** | Before writing this record, `git status --short` was empty at exact head `122846fededbaa6e48fa0ecc7c3e95ecd9a7d0cd`. `gofmt -l` on all three changed Go files and `git diff --check origin/main...HEAD` produced no output. `make check-hooks` confirmed `.githooks` is active. |
| 6 | Branch diverges cleanly from main | **PASS** | After the full test run, `git merge-tree --write-tree --messages origin/main 122846fededbaa6e48fa0ecc7c3e95ecd9a7d0cd` exited 0 against `origin/main@4e27de7011f9a5e7928bb9b27cb5fefd6349ae81`, producing tree `2990d8a273563cc7e2639131176956f293357825`; no rebase or conflict resolution was needed. GitHub also reports `mergeable=MERGEABLE`, `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | **PASS** | Relative to current main, the PR changes the sling fallback warning and its tests in three Go files plus the prior release-gate record. The pool identity correction and graph-v2 parity are both necessary parts of the same undeliverable-handoff warning; there is no independent feature theme. |

## Criterion 3 evidence

`test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v isolated-test-run.sh -- make test-local-full-parallel`

- `test_cmd_scope: full-suite`
- `test_counts: 44,514 PASS / 2 raw FAIL / 186 SKIP`
- `job_counts: 38 PASS / 2 raw FAIL / 0 omitted`
- `diff_tests_executed: 10 PASS / 0 FAIL / 0 SKIP across the full union`
  - `TestDefaultFormulaFallbackOnAssignedBeadIsSilentlyUndeliverable` — PASS x2
  - `TestDefaultFormulaFallbackControlUnassignedBeadAttaches` — PASS x2
  - `TestDefaultFormulaFallbackPoolSessionClaimIsNotUndeliverable` — PASS x2
  - `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached` — PASS x2
  - `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttachedGraphV2Formula` — PASS x2
- `skip_justification: 186 suite-controlled pre-existing opt-in, live-provider, platform/privilege, persistence, and helper-process exclusions; none is diff-owned`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI configuration changed)`
- `make test-ci-policy` — PASS
- `go vet ./...` — PASS
- `go build ./...` — PASS
- `gofmt -l internal/sling/sling_core.go internal/sling/sling_core_test.go cmd/gc/cmd_sling_undeliverable_test.go` — no output
- `git diff --check origin/main...HEAD` — no output
- `make check-hooks` — PASS
- Full log: `/var/tmp/ga-mj938o-full-suite.log`
- Job logs: `/var/tmp/gc-local-tests.ial5pu`
- Policy log: `/var/tmp/ga-mj938o-policy.log`
- Vet log: `/var/tmp/ga-mj938o-vet.log`
- Build log: `/var/tmp/ga-mj938o-build.log`

Independent exact-head checks:

- `GC_FAST_UNIT=0 go test -count=2 -run 'DefaultFormulaFallback|TestOnFormulaExistingMolecule' -v ./cmd/gc/...` — PASS (4 tests x2, 0 FAIL)
- `go test -count=1 ./internal/sling/...` — PASS
- `go test -count=1 -run 'RoutedToIdentity' ./internal/agentutil/...` — PASS

## Criterion 3a attribution

Both failures map to the predating open condition tracker `ga-lejnse`. This
run's exact sightings were appended and read back as comment
`1dd59db6-6c0e-5035-84d6-3af1f6a19e06`. No raw-failure retry was used.

- `TestPersonalWorkFormulaCompileAndRun -> ga-lejnse | clause 1: test/integration/review_formula_test.go is not diff-owned | clause 2: predating open tracker inspected and updated | clause 3(a): fixture gc init refused 20 pending shared-server migrations (v46 -> v66) before formula compilation, execution, or changed sling code could run | clause 4: the failing integration fixture has no path overlap with internal/sling/sling_core.go, internal/sling/sling_core_test.go, or cmd/gc/cmd_sling_undeliverable_test.go`
- `TestHumaBinary_SessionMessageAsync -> ga-lejnse | clause 1: test/integration/huma_binary_test.go is not diff-owned | clause 2: predating open tracker inspected and updated | clause 3(a): city initialization refused 2 pending shared-server migrations (v64 -> v66) before session-message execution or changed sling code could run | clause 4: the failing integration fixture has no path overlap with internal/sling/sling_core.go, internal/sling/sling_core_test.go, or cmd/gc/cmd_sling_undeliverable_test.go`

The full first-run results remain preserved and are attributed under the
non-diff-owned gate-failure protocol.
