# Release gate: pre-claim molecule entry bridge

- Deploy bead: `ga-e9idpt`
- Source bead: `ga-y42bwf`
- Review bead: `ga-szq6h2`
- Reviewed source: `11ca6d2aebecee76409456af4f46754b9e2abb9d`
- Feature cut point: `9979bed7b795020803eee1815dbe77fe83032266`
- Base: `origin/main` at `d7e1c3aea47ebe910e7301c844ad488fa1142020`
- Deploy branch: `deploy/ga-e9idpt-gate`
- Gate result: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed source and current
main, so this checklist applies the seven release criteria supplied by the
deployer policy.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-szq6h2` records an unambiguous `Review verdict: PASS` for reviewed commit `11ca6d2aebecee76409456af4f46754b9e2abb9d`. |
| 2 | Acceptance criteria met | **PASS** | The two new pre-claim bridge tests pass; the claimed-bead lookup remains first; the fallback searches open source beads by `gc.routed_to` across agent identities with `TierBoth` and a limit of 10; lookup errors remain advisory/best-effort. All 17 focused resolver/reminder tests pass. `go build ./...` and `go vet ./...` pass. The PR review notes call out the pool-shared metadata behavior. |
| 3 | Tests pass | **PASS** | See detailed test evidence below. No diff-owned test failed or skipped. The documented suites were run with the repository-pinned `bd` v1.1.0 build (`8e4e59d39`) and the rootless podman environment configured before execution. |
| 4 | No high-severity review findings open | **PASS** | The PASS review reports no unresolved HIGH findings; count: 0. |
| 5 | Final branch is clean | **PASS** | The detached worktree at the exact reviewed source reported an empty `git status --short`, and `git diff --check` passed before this gate artifact was added. |
| 6 | Branch diverges cleanly from main | **PASS** | Pre-flight found no PR carrying the reviewed source. `git merge-tree --write-tree origin/main 11ca6d2aebecee76409456af4f46754b9e2abb9d` exited 0 and produced tree `5f1fd63600be0d74bdc14fcf354156cf5301e6bf`; merge base is `9979bed7b795020803eee1815dbe77fe83032266`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two reviewed commits touch only `cmd/gc/wisp_step_inject.go` and its focused test file, implementing one behavior: resolving a formula entry step during the pre-claim wake window. |

## Test evidence

- `test_cmd: make test-local-full-parallel`
  - Raw sharded result: 32 PASS jobs, 8 red jobs.
  - Six red jobs were local shared-store/startup collisions; all seven named
    tests from those jobs passed when rerun serially against the reviewed
    commit: 7 PASS, 0 FAIL, 0 SKIP.
  - Two red jobs were the known host tmux 3.7b default-binding incompatibility.
    The same two tests fail identically on current `origin/main`, so they are
    baseline-only and do not bear on this `cmd/gc` change.
  - Effective gate accounting: 38 PASS jobs, 0 feature FAIL, 2 justified
    baseline SKIP-equivalents.
- `test_cmd: make test-fast-parallel`
  - Raw result: 9 PASS jobs, 1 red job. The red job contained one SQLite
    SIGKILL boundary test whose child missed its 10-second startup deadline
    under shard saturation.
  - The exact test passed in isolation immediately afterward: 5 subtests PASS,
    0 FAIL, 0 SKIP (1.33s). Effective result: 10 PASS jobs, 0 FAIL.
- `test_cmd: go test -json -count=1 ./cmd/gc -run 'TestResolveActiveWispStep|TestFormatWispStepReminder|TestWispStepAssignees'`
  - Counts: 17 PASS, 0 FAIL, 0 SKIP.
  - `diff_tests_executed:`
    - `TestResolveActiveWispStep_PreClaimBridgeMisses` — PASS
    - `TestResolveActiveWispStep_PostClaimBridgeResolves` — PASS
  - `waiver_ref: none`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=9979bed7b795020803eee1815dbe77fe83032266 make lint-affected` — PASS, 0 issues
- `make fmt-check-changed` — PASS
- `make check-docs` — PASS
- `git diff --check` — PASS
- Git hooks are active: `core.hooksPath=.githooks`.

## Review focus

The fallback is deliberately advisory. Because `gc.routed_to` metadata is
single-valued and a pool route may identify multiple eligible agents, more
than one pool member can see the same read-only step reminder. Claiming remains
the arbitration mechanism; the deploy does not alter sling routing, reaping,
or prompt behavior.
