# Release Gate: ga-9us8ti - Coordstore Soak + Chaos Harness (Slim)

- Gate run: 2026-06-01T15:02:31Z
- Deploy bead: ga-9us8ti
- Review bead: ga-iatqga
- Source bead: ga-jbwx5u
- PR: https://github.com/gastownhall/gascity/pull/2670
- Branch: feat/coordstore-soak-chaos-slim
- Evaluated head: c6a2cf7bfaf26a67b1c8fb0e74075e52191450fc
- Base: origin/main @ 801fa89192110be29aa3b317388a71768133b89b
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-iatqga is closed with `Review Verdict: PASS` for head c6a2cf7bfaf26a67b1c8fb0e74075e52191450fc. PR comment https://github.com/gastownhall/gascity/pull/2670#issuecomment-4593751402 records re-review PASS on the rebased head. |
| 2 | Acceptance criteria met | PASS | The branch publishes the slim coordstore soak and chaos harness, preserves current main's HQStore backend removal, keeps soak/chaos tests gated behind `COORDSTORE_SOAK=1`, redacts Dolt DSN provenance, uses portable Go environment defaults in launch scripts, and avoids new module dependencies. |
| 3 | Tests pass | PASS | Local gates passed: coordstore soak shell syntax, focused coordstore soak tests, short coordstore package sweep, `go vet ./...`, `make test-fast-parallel`, docsync, `git diff --check`, and merge-tree check. GitHub PR checks for #2670 are all passing or intentionally skipped; merge state is CLEAN. |
| 4 | No high-severity review findings open | PASS | ga-iatqga reports no unresolved high findings. Remaining review notes are low/info only and explicitly acceptable for benchmark/operator tooling. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1 -uno` was empty before writing this gate artifact. This gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree e1d4a0849e37068af5b29eb7a586c8d1ae5a756e. GitHub reports `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | The diff is one feature theme: the slim coordstore soak/chaos harness, its operator scripts, tests, generated CLI reference ordering, and release-gate evidence. No unrelated production feature or module dependency change is bundled. |

## Acceptance Evidence

- `internal/benchmarks/coordstore` contains the soak runner, chaos process/server/protocol, recorder, triage, preflight checks, workload updates, and focused tests.
- `scripts/coordstore-soak` contains only `README.md`, `launch-dolt-baseline.sh`, and `setup-isolated-dolt.sh`; stale full-matrix and single-backend launchers are absent.
- `scripts/coordstore-soak/launch-dolt-baseline.sh` writes `dolt_dsn=[REDACTED]` to launch metadata while still requiring the real DSN via `COORDSTORE_DOLT_DSN` at runtime.
- `rg -n "HQStore|hqstore" --glob '!release-gates/**' --glob '!docs/**' --glob '!**/*_test.go'` returned no active-code matches.
- `git diff --name-only origin/main...HEAD | rg '(^|/)(go\.mod|go\.sum)$'` returned no module file changes.

## Test Evidence

| Command | Result |
|---------|--------|
| `bash -n scripts/coordstore-soak/launch-dolt-baseline.sh && bash -n scripts/coordstore-soak/setup-isolated-dolt.sh` | PASS |
| `GOTOOLCHAIN=auto go test ./internal/benchmarks/coordstore -run 'TestBenchmarkSoak(PhaseA\|Calibrate\|PhaseB\|Triage\|Dolt)$\|TestSoakConfigFromEnvParsesSeparateChaosDuration' -count=1` | PASS |
| `GOTOOLCHAIN=auto go test -short -timeout 60s ./internal/benchmarks/coordstore/...` | PASS |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |
| `GOTOOLCHAIN=auto make test-fast-parallel` | PASS: all fast jobs passed |
| `GOTOOLCHAIN=auto go test ./test/docsync` | PASS |
| `git diff --check origin/main...HEAD` | PASS |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `gh pr checks 2670 --watch=false` | PASS: all required checks passed; skipped jobs are optional or intentionally gated |

## Final Gate Result

PASS. PR #2670 is ready for merge-authority review. The deployer must not merge it; route the merge request to mayor/mpr.
