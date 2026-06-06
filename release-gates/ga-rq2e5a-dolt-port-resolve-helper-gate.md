# Release gate - Dolt port resolve helper (ga-rq2e5a / ga-5ju0nf)

Verdict: PASS

- Source bead: `ga-rq2e5a` - `feat(packs/dolt): port_resolve.sh helper kills :=3307 fallback (ga-lsois slice 1/3)`
- Review bead: `ga-5ju0nf` - `Review: Dolt port resolve helper (ga-rq2e5a)`
- PR: https://github.com/gastownhall/gascity/pull/2282
- Branch: `quad341:builder/ga-rq2e5a-1`
- Source commit: `2d9681ac35d8c1045ff3745fc6dc7f7151e51f39`
- Release criteria source: deployer prompt criteria. `docs/PROJECT_MANIFEST.md` was not present in this repository snapshot.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-5ju0nf` contains `Review verdict: PASS`. Reviewer synthesis lists findings F1-F5 as informational only. |
| 2 | Acceptance criteria met | PASS | The helper `examples/dolt/assets/scripts/port_resolve.sh` exists with `resolve_dolt_port_or_die`; `runtime.sh` and maintenance `dolt-target.sh` route through it; current source-closure script `GC_DOLT_PORT.*3307` fallbacks were removed; packlint and helper tests cover the pinned behavior; `gc dolt sync --db` reserved-name validation still runs before runtime port resolution. |
| 3 | Tests pass | PASS | Focused acceptance tests, shell syntax checks, `go vet ./...`, `go build ./...`, and `make test-fast-parallel` passed on the PR branch. |
| 4 | No high-severity review findings open | PASS | Review notes contain five informational findings and no HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty before the gate commits and rechecked clean after push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned 0 after the gate commit; `git merge-tree` conflict scan found no conflict markers; PR #2282 reports `mergeable: MERGEABLE`. GitHub `mergeStateStatus: BLOCKED` is from queued/in-progress CI and branch protection, not a merge conflict. |

## Acceptance Evidence

- Source bead acceptance required all ga-u0lx9p helper cases, packlint coverage, clean vet, and broad tests.
- `go test ./test/packlint -run TestNoDolt3307FallbackInScripts -count=1` - PASS
- `go test ./examples/dolt -run 'TestPortResolveOrDie|TestRuntimeShUsesPortResolve|TestDoltTargetShUsesPortResolve|TestRuntimeScriptPortPrecedence' -count=1` - PASS
- `go test ./examples/gastown -run 'TestMaintenanceDoltScriptsUseManagedRuntimePorts|TestMaintenanceDoltScriptsFallbackToManagedRuntimePortsWithInconclusiveLsof|TestMaintenanceDoltScriptsUsePsConfirmedManagedRuntimePorts' -count=1` - PASS
- `sh -n examples/dolt/assets/scripts/port_resolve.sh examples/dolt/assets/scripts/runtime.sh examples/gastown/packs/maintenance/assets/scripts/dolt-target.sh examples/dolt/assets/scripts/mol-dog-backup.sh examples/dolt/assets/scripts/mol-dog-doctor.sh examples/dolt/commands/sync/run.sh` - PASS
- `go test ./cmd/gc -run 'TestEmbed|TestBuiltin|TestMaterialize|TestFormulasUseGcDoltSqlNotRawPort|TestDoltSyncRejectsManagedProbeDatabaseFilter' -count=1` - PASS
- `go test ./examples/dolt -run 'TestPortResolveOrDie|TestRuntimeShUsesPortResolve|TestRuntimeScriptPortPrecedence|TestDoltSync' -count=1` - PASS
- `go vet ./...` - PASS
- `go build ./...` - PASS
- `make test-fast-parallel` - PASS (`All fast jobs passed`)

## Review Findings

| Finding | Severity | Gate disposition |
|---------|----------|------------------|
| F1 | Informational | Spec path correction accepted: lint targets source pack directories under `examples/`. |
| F2 | Informational | Extra allowlist entry accepted because both formula literals are sibling-slice scope. |
| F3 | Informational | Unit isolation accepted; managed runtime fixtures are already exercised in `health_test.go`. |
| F4 | Informational | Companion validation reorder accepted and covered by `TestDoltSyncRejectsManagedProbeDatabaseFilter`. |
| F5 | Informational | Test-only `$SCRIPT_DIR` fallback accepted; production path still uses `GC_SYSTEM_PACKS_DIR`. |

## Push Target

PR #2282 already exists with head `quad341:builder/ga-rq2e5a-1`. The gate commit is pushed to `fork` so the open PR head updates directly.
