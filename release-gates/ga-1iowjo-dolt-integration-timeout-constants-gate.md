# Release Gate: Dolt integration timeout constants

Gate date: 2026-08-20

Deploy bead: `ga-1iowjo`

Review bead: `ga-thuouz`

Original reviewed source: `09c63aa404929089238c7499a762001d8892f999`

Gated source after bounded rebases: `fcf1009a8bdad843d6f6f0217178edfa80dc36fc`

Base checked: `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078`

Merge base: `3218c505668fb043a89531a4ec1299d15a7e68d7`

Clean merge tree: `84ac27b2a57ff6d7c2bc1d23a66708f6b519de06`

Overall result: **PASS — 7/7 criteria**.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-thuouz` is closed with a final PASS after merge-authority waiver `mayor-2026-08-20-ga-thuouz-c1`. Two bounded, force-with-lease rebases were recorded on the deploy bead: `09c63aa404929089238c7499a762001d8892f999` → `3410d0e4f4cc9c9ba3992c5d0855f6bb1f0e04cd` → `fcf1009a8bdad843d6f6f0217178edfa80dc36fc`. |
| 2 | Acceptance criteria met | PASS | The two hardcoded 15-second waits now use named timeout constants, and the AST guard prevents literal regression. On the gated source, `TestDoltConfigWiringExternalHost`, `TestDoltConfigTimeoutsUseNamedConstants`, and `TestBdStoreMailWispInsert` passed with the pinned Dolt image available through the rootless Podman socket. |
| 3 | Tests pass | PASS | The documented full command selected 40 jobs: 36 PASS, 4 FAIL, 0 SKIP. All four raw failures are independently attributed below; no candidate-owned test failed. Focused candidate-owned results were 3 PASS, 0 FAIL, 1 merge-authority-waived SKIP. `make test-ci-policy` and `go vet ./...` passed on the exact gated source. |
| 4 | No high-severity review findings open | PASS | Review records no style, security, or blocking code finding; the only review block was the now-resolved diff-file SKIP waiver. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before this checklist was authored; `git diff --check origin/main...HEAD` and `gofmt -l` on all changed Go files produced no findings. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 against `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078` and produced tree `84ac27b2a57ff6d7c2bc1d23a66708f6b519de06`. Ancestry scope passed for the deploy, build, and review bead IDs; no `.claude/**` path is introduced. |
| 7 | Single feature theme | PASS | The two candidate commits touch only three `test/integration` files and serve one behavior: replacing hardcoded Dolt startup waits with named hang budgets plus a static regression guard. |

## Test evidence

Environment:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- cached pinned images include `dolthub/dolt-sql-server:2.1.7` and
  `dolthub/dolt:2.1.7`

Full command:

```text
LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
40 jobs selected: 36 PASS, 4 FAIL, 0 SKIP
raw exit: 2 (attributed failures below)
logs: /var/tmp/gc-local-tests.wfGUMn
```

Focused candidate-owned command:

```text
go test -count=1 -tags integration -timeout 30m ./test/integration/ \
  -run '^(TestDoltConfigWiringExternalHost|TestDoltConfigTimeoutsUseNamedConstants|TestBdStoreConformance|TestBdStoreMailWispInsert)$' -v
```

`diff_tests_executed`:

- `TestDoltConfigWiringExternalHost` — PASS (3.65s)
- `TestDoltConfigTimeoutsUseNamedConstants` — PASS (0.00s)
- `TestBdStoreMailWispInsert` — PASS (4.48s)
- `TestBdStoreConformance` — SKIP (0.00s), covered by
  `waiver_ref: mayor-2026-08-20-ga-thuouz-c1`

`skip_justification`: `TestBdStoreConformance` begins with an unconditional
pre-existing `t.Skip`; the candidate's same-file change is confined to seven
constant-block lines. Mayor independently verified that mechanism and granted
the candidate-scoped waiver above.

`policy_lane`: `make test-ci-policy` — PASS.

`go_vet`: `go vet ./...` — PASS.

## Failure attribution

- `TestProviderLiveClaudeKindPath` (`agent_pane_busy` on `w1:p1`) → tracker
  `ga-fh1flg`; candidate has no `internal/runtime/herdr`, session, tmux, or
  pane-allocation path. Applied standing merge-authority disposition
  `waiver_ref: mayor-2026-08-20-herdr-pane-standing` / memory key
  `herdr-pane-busy-standing-disposition` at gated source
  `fcf1009a8bdad843d6f6f0217178edfa80dc36fc`; use is recorded on the deploy
  bead.
- `TestBdFlagManifestCurrent` → tracker `ga-f0uceo`; not diff-owned, no path
  overlap, and the integration-tagged focused test failed identically on exact
  current base `4f4a37b28c9cfaa1ebe1c587576b69663a47f078` because the installed `bd`
  exposes flags absent from the repository manifest.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → tracker `ga-afqddr`;
  neither is diff-owned, there is no `internal/runtime/tmux` overlap, and both
  failed identically on exact current base
  `4f4a37b28c9cfaa1ebe1c587576b69663a47f078` with empty host tmux default
  bindings.

All other full-suite jobs passed. The attributed failures have tracked IDs,
base/mechanism proof, and no candidate path overlap; criterion 3 therefore
passes under the repository's non-diff-owned failure policy.
