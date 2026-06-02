# Release Gate: ga-ebn8cr - Coordstore Soak + Chaos Harness

- Gate run: 2026-06-01T14:36:28Z
- Deploy bead: ga-ebn8cr
- Review bead: ga-5n8ssk
- Prior review/fix bead: ga-54icnb
- PR: https://github.com/gastownhall/gascity/pull/2670
- Branch: feat/coordstore-soak-chaos-slim
- Evaluated head: 3abeb277070d4f57eb77f2fb158be5868a4d7091
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-5n8ssk is closed with `Review Verdict: PASS` for head 686b9199e03e0cd1106948beda79e186e342b6a2 on branch feat/coordstore-soak-chaos-slim. It explicitly re-reviewed and cleared ga-54icnb request-change findings. |
| 2 | Acceptance criteria met | PASS | The branch publishes the slim coordstore soak/chaos harness, preserves current main's HQStore backend removal, removes stale FullMatrix docs and the stale single-backend launcher, redacts Dolt DSN provenance, uses portable Go env defaults in launch scripts, and adds focused harness/unit coverage. |
| 3 | Tests pass | PASS | Local gates passed: `bash -n` for coordstore soak scripts, focused coordstore soak tests, `go test -short -timeout 60s ./internal/benchmarks/coordstore/...`, `GOTOOLCHAIN=auto go vet ./...`, `GOTOOLCHAIN=auto make test-fast-parallel`, `git diff --check origin/main...HEAD`, and active-code HQStore grep. GitHub PR checks must rerun after this rebased branch is pushed. |
| 4 | No high-severity review findings open | PASS | ga-5n8ssk marks the prior HIGH stale README/launcher finding and MEDIUM DSN exposure finding resolved. It reports all checks passing including CodeQL. Unresolved HIGH review finding count: 0. |
| 5 | Final branch is clean | PASS | Before writing this gate refresh, `git status --porcelain=v1 -uno` was empty at 3abeb277070d4f57eb77f2fb158be5868a4d7091. This gate refresh is the only remaining change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | After fetching `origin/main`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 64309aa238bc87db306f0ff4e6067bb99e2f6022 before this gate refresh. The branch was rebased onto `origin/main` 757d16d25a7100d57c228a44af1733adc2cfeb0d. |
| 7 | Single feature theme | PASS | The commit set is one feature theme: a slim coordstore soak and chaos harness plus its operator scripts, tests, release gate, and documentation. No unrelated user-facing feature is bundled. |

## Acceptance Evidence

- `scripts/coordstore-soak/README.md` now documents only current tests and launchers; stale `TestBenchmarkSoakFullMatrix` and `launch-full-matrix.sh` references are gone.
- `scripts/coordstore-soak/launch-single-backend.sh` was deleted because it referenced a removed one-off test path.
- `scripts/coordstore-soak/launch-dolt-baseline.sh` records `dolt_dsn=[REDACTED]` instead of writing credential-bearing DSNs to launch artifacts.
- Launch scripts use environment defaults and `go env` instead of hardcoded developer-local Go paths.
- `internal/benchmarks/coordstore` includes the soak runner, chaos process/server/client, artifact writing, preflight checks, triage, workload, and regression tests.
- Current main's HQStore backend removal is preserved; the rebased branch does not re-add `internal/beads/hqstore_bench_test.go`, the HQStore benchmark adapter, or active HQStore references.

## Test Evidence

| Command | Result |
|---------|--------|
| `bash -n scripts/coordstore-soak/launch-dolt-baseline.sh && bash -n scripts/coordstore-soak/setup-isolated-dolt.sh` | PASS |
| `GOTOOLCHAIN=auto go test ./internal/benchmarks/coordstore -run 'TestBenchmarkSoak(PhaseA\|Calibrate\|PhaseB\|Triage\|Dolt)$\|TestSoakConfigFromEnvParsesSeparateChaosDuration' -count=1` | PASS |
| `GOTOOLCHAIN=auto go test -short -timeout 60s ./internal/benchmarks/coordstore/...` | PASS |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |
| `GOTOOLCHAIN=auto make test-fast-parallel` | PASS: all fast jobs passed via the pre-commit hook during 3abeb277070d4f57eb77f2fb158be5868a4d7091. |
| `git diff --check origin/main...HEAD` | PASS |
| `rg -n "HQStore\|hqstore" --glob '!release-gates/**' --glob '!docs/**' --glob '!**/*_test.go'` | PASS: no active-code matches. |
| `git diff --name-only origin/main...HEAD \| rg '(^\|/)(go\.mod\|go\.sum)$'` | PASS: no module file changes. |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `gh pr checks 2670 --watch=false` | Not rerun before push; GitHub checks must rerun on the pushed rebased head. |

## Final Gate Result

PASS. The current PR head is suitable for human review and merge decision after this gate artifact is committed and pushed.
