# Release gate: work-record coverage analysis

- Deploy bead: `ga-o6o6pq`
- Build bead: `ga-lom03n`
- Review bead: `ga-l4s813`
- Reviewed source: `7c482056c04390ddf43262394494888c932336c2`
- Base checked: `origin/main@3b6ab2351615c95d6b2f00e63911a14dd55fe67c`
- Deploy mode: remote

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-l4s813` is closed with an explicit PASS verdict on the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | `internal/workrecord` owns the gated-bead and valid-outcome predicates plus coverage analysis/table formatting; the existing close gate delegates to it; `gc analyze work-record` is read-only and supports `--city`, `--limit`, table output, and `--json`; all requested unit/CLI tests are present. The command census and generated CLI reference include the command. |
| 3 | Tests pass | PASS | See full-suite evidence and raw-failure attribution below. All diff-owned tests executed and passed; there were no diff-owned FAILs or SKIPs. Six unrelated raw failures are preserved and attributed under the standing gate policy. |
| 4 | No high-severity review findings open | PASS | Review bead records no security, style, spec, or other blocking findings; unresolved HIGH count is zero. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty after the full suite, generators, policy, vet, lint, and formatting checks. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 7c482056c04390ddf43262394494888c932336c2` succeeded against the current base. The pre-flight found no PR carrying the reviewed source. No self-rebase was required. |
| 7 | Single feature theme | PASS | The three source commits all reference `ga-lom03n` and implement one operator-facing theme: measuring ADR-0009 work-record coverage without mutating beads. Generated command/docs artifacts are part of that same command surface. |

## Test evidence

`test_cmd_scope: full-suite`

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-o6o6pq-full-gate \
make test-local-full-parallel
```

- `test_counts: 45652 PASS, 6 FAIL, 187 SKIP`
- `skip_justification:` all SKIPs were pre-existing suite-declared platform, optional-provider, or real-infrastructure cases. None belongs to a test added or modified by this diff.
- `waiver_ref: ga-6bnc42` applies only to the exact Beads #4566 dirty-schema raw failure described below; no waiver was needed for any diff-owned test.
- `diff_tests_executed:`
  - `cmd/gc/cmd_analyze_workrecord_test.go`: `TestAnalyzeWorkRecordFromStoreTable` PASS, `TestAnalyzeWorkRecordFromStoreJSON` PASS, `TestAnalyzeWorkRecordFromStoreOnlyScansClosed` PASS, `TestNewAnalyzeCmdRegistersWorkRecordSubcommand` PASS.
  - `internal/workrecord/workrecord_test.go`: `TestIsGatedBead` PASS, `TestValidOutcome` PASS, `TestAnalyzeCoverage` PASS, `TestAnalyzeCoverageEmpty` PASS, `TestAnalyzeCoverageAllCovered` PASS.
  - `internal/workrecord/format_test.go`: `TestFormatTable` PASS, `TestFormatTableNoMissing` PASS, `TestFormatTableZeroTotal` PASS.
  - `internal/productmetrics/event_test.go`: `TestInjectedImmutableCommandCatalogRoundTripsWithoutExpandingProduction` PASS.
  - `internal/workrecord/testenv_import_test.go`: generated test-environment import guard; contains no test function. Its package executed successfully in both the unit and integration package lanes.

### Raw failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestCatalogMatchesProductionWiringAndDocumentation` (unit and integration package lanes) | `ga-1s16pf` | Clause 3(a), mechanism: the runtime-seam waivers owned by dead `ga-80po0c.3` expired on 2026-08-26. The candidate does not change the provider ledger, waiver catalog, or `cmd/gc/runtime_registry.go`, the only `cmd/gc` source this test reads. No path overlap. |
| FAIL: `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clause 3(a), mechanism: the installed `bd` exposes flags absent from the repository manifest. The candidate does not touch `internal/bdflags` or its manifest mechanism. No path overlap. |
| FAIL: `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-afqddr` | Clause 3(a), mechanism: host tmux 3.7b returns an empty filtered default keytable. The candidate cannot alter the host keytable and does not touch `internal/runtime/tmux`. No path overlap. |
| FAIL-WAIVED: `TestCleanInstallTutorialPath` | `ga-lpfjhc`; standing authorization `ga-6bnc42` | Exact Beads #4566 fixture-bootstrap signature: schema migration found pre-existing dirty `issues`. The candidate does not change Dolt migration or store bootstrap, and the failure occurs during `gc init` before tutorial assertions. No failing-test path overlap. |

All trackers above were opened and verified to predate this run. Occurrences were appended to the trackers and deploy bead before any push.

## Required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go vet ./...` — PASS
- `go run ./cmd/gen-command-census --check` — PASS
- `scripts/check-generated-docs-drift.sh` — PASS; `docs/reference/cli.md` and all generated schemas reproduce without drift.
- `make lint-affected` with a fresh `/var/tmp` cache and the merge-base scope — PASS
- `make fmt-check-changed` with the same scope — PASS
- `git diff --check origin/main...HEAD` — PASS

## Acceptance audit

1. The pure `internal/workrecord` package exposes the requested predicates, `CoverageReport`, `AnalyzeCoverage`, and `FormatTable`: PASS.
2. `cmd/gc/work_record_gate.go` delegates to the package while its pre-existing tests remain unchanged and pass: PASS.
3. `gc analyze work-record` opens the normal store, lists only closed beads in most-recent-first order with a default limit of 500, performs no writes, and supports table/JSON output: PASS.
4. The requested work-record and CLI tests exist and execute in the full suite: PASS.
5. Full-suite, policy, vet, generated-artifact, lint, formatting, and documentation freshness evidence is recorded above: PASS.
