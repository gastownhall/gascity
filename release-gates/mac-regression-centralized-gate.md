# Release gate: centralized macOS regression routing

- Deploy bead: `ga-dvo3mn`
- Build bead: `ga-n7ef4e`
- Review bead: `ga-99n5nd`
- Reviewed source: `c5ff3129389ff708a8c4899567b1d48e41b8403f`
- Gate base: `origin/main@e6135a435098a70f20081d1d88a03b6742002d9a`
- Evaluation date: 2026-07-30
- Disposition: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Independent review bead `ga-99n5nd` records round-2 `verdict: pass` at the reviewed source SHA after the builder addressed all three round-1 findings. |
| 2 | Acceptance criteria met | **PASS** | The workflow has one always-run `gate` job that emits the three tier booleans and a reason; all tier jobs consume those outputs; the summary uses bare `always()` and reads the gate result. The corrected checkout has explicit repository/ref and `persist-credentials: false`, the diagnostic path filter contains exactly `cmd/gc/**`, `internal/pathutil/**`, and `internal/fsys/**`, and unknown manual-dispatch suites fall back to smoke. Dedicated Go contract tests cover each invariant. |
| 3 | Tests pass | **PASS** | At the reviewed source SHA: `go build ./...` and `go vet ./...` passed; `go test ./scripts/... -count=1 -v` reported 351 PASS, 0 FAIL, 0 SKIP; `make test-fast-parallel` passed all 10 jobs; and `make test-cmd-gc-process-parallel` passed all six `GC_FAST_UNIT=0` shards plus `productmetrics-testhook`, reporting 15,243 PASS, 0 FAIL, and 11 intentional skips. The process skips are existing helper-only, opt-in live-canary, unsupported-OS, unavailable optional prompt-fixture, or ambient-cwd cases explicitly disabled inside test binaries; none bears on workflow routing. Nine additional required CI outputs induced by the workflow-file/shared-OR path filters were not locally re-run: six have no file overlap with this diff, two worker/integration surfaces have no code overlap, and one is Windows-only. GitHub CI remains the authoritative execution of those checks before merge. |
| 4 | No high-severity review findings open | **PASS** | Round 2 reports no blocking security, style, or specification findings; all three prior findings are fixed and directly tested. Unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after testing at the reviewed source SHA; only this gate record was then added. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after tests. `git merge-tree --write-tree origin/main c5ff3129389ff708a8c4899567b1d48e41b8403f` exited 0 and produced tree `5abfc57c279b35786da86938d816602e04940c9e`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | The reviewed three-commit set changes one GitHub Actions workflow and its contract test file to centralize macOS regression tier routing and make skipped-workflow outcomes visible. |

## Acceptance evidence

- Scheduled runs enable smoke, full, and review-formula tiers.
- Manual `full`, `needs-mac`, `smoke`, and unknown suite values route deterministically, with unknown values retaining smoke coverage.
- Same-repository, non-draft pull requests require the `needs-mac` label; fork and draft pull requests remain skipped with explicit reasons.
- Every tier job reads the centralized gate outputs rather than duplicating trigger expressions.
- The summary always runs and fails closed when the gate or any selected tier fails.
- No new permissions, dependencies, action versions, secrets, or trigger events are introduced.
