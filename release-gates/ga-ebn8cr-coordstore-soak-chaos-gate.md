# Release Gate: ga-ebn8cr - Coordstore Soak + Chaos Harness

- Gate run: 2026-05-31T10:59:32Z
- Deploy bead: ga-ebn8cr
- Review bead: ga-5n8ssk
- Prior review/fix bead: ga-54icnb
- PR: https://github.com/gastownhall/gascity/pull/2670
- Branch: feat/coordstore-soak-chaos-slim
- Evaluated head: 686b9199e03e0cd1106948beda79e186e342b6a2
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-5n8ssk is closed with `Review Verdict: PASS` for head 686b9199e03e0cd1106948beda79e186e342b6a2 on branch feat/coordstore-soak-chaos-slim. It explicitly re-reviewed and cleared ga-54icnb request-change findings. |
| 2 | Acceptance criteria met | PASS | The branch publishes the slim coordstore soak/chaos harness, keeps the HQStore scan benchmark, removes stale FullMatrix docs and the stale single-backend launcher, redacts Dolt DSN provenance, uses portable Go env defaults in launch scripts, and adds focused harness/unit coverage. |
| 3 | Tests pass | PASS | Local gates passed: `bash -n` for coordstore soak scripts, focused coordstore soak tests, `go test -short -timeout 60s ./internal/benchmarks/coordstore/...`, `GOTOOLCHAIN=auto go vet ./...`, `GOTOOLCHAIN=auto make test-fast-parallel`, `git diff --check origin/main...HEAD`, and no `go.mod`/`go.sum` diff. GitHub PR checks are green, including CodeQL, CI preflight, CI integration, and CI required. |
| 4 | No high-severity review findings open | PASS | ga-5n8ssk marks the prior HIGH stale README/launcher finding and MEDIUM DSN exposure finding resolved. It reports all checks passing including CodeQL. Unresolved HIGH review finding count: 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --porcelain=v1 -uno` was empty and HEAD matched `origin/feat/coordstore-soak-chaos-slim`. This gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | After fetching `origin/main`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree c1fcabca3116c07f642da18d51d69640fedf97e7. GitHub reports PR #2670 `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | The commit set is one feature theme: a slim coordstore soak and chaos harness plus its operator scripts, benchmark guard, tests, and documentation. No unrelated user-facing feature is bundled. |

## Acceptance Evidence

- `scripts/coordstore-soak/README.md` now documents only current tests and launchers; stale `TestBenchmarkSoakFullMatrix` and `launch-full-matrix.sh` references are gone.
- `scripts/coordstore-soak/launch-single-backend.sh` was deleted because it referenced a removed one-off test path.
- `scripts/coordstore-soak/launch-dolt-baseline.sh` records `dolt_dsn=[REDACTED]` instead of writing credential-bearing DSNs to launch artifacts.
- Launch scripts use environment defaults and `go env` instead of hardcoded developer-local Go paths.
- `internal/benchmarks/coordstore` includes the soak runner, chaos process/server/client, artifact writing, preflight checks, triage, workload, and regression tests.
- `internal/beads/hqstore_bench_test.go` adds the HQStore recent-scan benchmark guard without adding module dependencies.

## Test Evidence

| Command | Result |
|---------|--------|
| `bash -n scripts/coordstore-soak/launch-dolt-baseline.sh && bash -n scripts/coordstore-soak/setup-isolated-dolt.sh` | PASS |
| `GOTOOLCHAIN=auto go test ./internal/benchmarks/coordstore -run 'TestBenchmarkSoak(PhaseA\|Calibrate\|PhaseB\|Triage\|Dolt)$\|TestSoakConfigFromEnvParsesSeparateChaosDuration' -count=1` | PASS |
| `GOTOOLCHAIN=auto go test -short -timeout 60s ./internal/benchmarks/coordstore/...` | PASS |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |
| `GOTOOLCHAIN=auto make test-fast-parallel` | PASS: all fast jobs passed. |
| `git diff --check origin/main...HEAD` | PASS |
| `git diff --name-only origin/main...HEAD \| rg '(^\|/)(go\.mod\|go\.sum)$'` | PASS: no module file changes. |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `gh pr checks 2670 --watch=false` | PASS: CodeQL, CI preflight, CI integration, CI required, and PR check rollups are green. |

## Final Gate Result

PASS. The current PR head is suitable for human review and merge decision after this gate artifact is committed and pushed.
