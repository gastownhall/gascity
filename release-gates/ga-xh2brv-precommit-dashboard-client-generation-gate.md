# Release Gate: ga-xh2brv

Bead: ga-xh2brv
Source bead: ga-u8h4ql
Reviewed commit: 7dc6d862e839fec6df83f21d213ac4dc684c1378
Deploy branch: deploy/ga-xh2brv-gate
Deploy source commit: b0713c362698f4463d9704d0b0fcac0afb9fcf06
Base checked: origin/main c967f1eebef64fe1ad4d9d287fd778fcd796f640
Project manifest: docs/PROJECT_MANIFEST.md not present in this repository checkout; evaluated against the deployer prompt's gate table, TESTING.md, and the Makefile fast-suite target.

Prior attempt: this gate file previously FAILed at local (unpushed) commit
`ac2a8ce0d6978c1d15431a3bfc8076fb117f8d07` on criteria 2 and 3. Criterion 2:
with only `internal/api/openapi.json` staged, the reviewed hook exited at the
existing go/web/docs early guard before `spec_changed` was computed, so
client generation and staging never ran for a spec-only commit; the original
static test missed this control-flow path. Criterion 3: `make
test-fast-parallel` passed 7/8 shards but `TestCmdStopWallClockTimeoutBoundsDirectStop`
exceeded its near-100ms cap (1.230021478s) under sharded load. No deploy
branch was pushed and no PR was opened for that attempt; the failure was
routed to `gascity/builder` (sling auto-convoy ga-dlknrr). Both are fixed on
this branch (commits `18f8521ed`, `8b321e034`, `cb4051f0d`) and independently
re-verified below.

Content-identity check: the originally reviewed change
(`.githooks/pre-commit`, `scripts/precommit_contract_test.go`) is
byte-for-byte identical between reviewed commit `7dc6d862e` and this
branch's re-derived commits (`d9c19d1be`, `f89f890b7`) — confirmed via
`diff <(git show 7dc6d862e:<file>) <(git show f89f890b7:<file>)`, empty for
both files. The review's PASS verdict covers the exact code being deployed;
it was only rebased onto a later `main` tip, not reimplemented.

Stable patch-id for the feature files:

`ddb48469362e668722491ea931e4c4dd11c86a9a`

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source bead ga-u8h4ql notes contain `REVIEW VERDICT: PASS (reviewer: gascity/reviewer, 2026-07-25T14:41:13Z)` for reviewed commit `7dc6d862e839fec6df83f21d213ac4dc684c1378`. Content-identity check (above) confirms this branch carries that exact reviewed diff. |
| 2 | Acceptance criteria met | PASS | Prior FAIL fixed by `8b321e034` (computes `spec_changed` before the early-guard branch so a spec-only-staged commit still regenerates the client). Regression covered by new test `TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged` (`18f8521ed`), re-run standalone: PASS (0.38s). Sibling test `TestPreCommitRegeneratesDashboardClientOnSpecChange` re-run: PASS. |
| 3 | Tests pass | PASS | Prior FAIL fixed by `cb4051f0d` (widens `TestCmdStopWallClockTimeoutBoundsDirectStop`'s latency bound). Re-run 3x standalone: PASS each time (0.15s, 0.14s, 0.14s). `go vet ./...` clean. `go build ./cmd/gc/` exit 0. `make test-fast-parallel`: all 8 fast jobs passed, confirmed via 3 independent fresh runs this session against the current working tree (including the census-ledger fix). |
| 4 | No high-severity review findings open | PASS | ga-u8h4ql reviewer notes: OWASP walk finds no findings (A08 improved, no injection surface, only hardcoded commands). One minor non-blocking observation (no end-to-end hook-execution test) explicitly flagged "not a blocker." CI-fix-integrity check confirmed the reviewer's own pre-push flake dismissal was legitimate (diff scope excludes cmd/gc). No HIGH findings anywhere in the notes. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v2 --branch` clean before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0, produced tree `f11a10ce5c2d0846158921a0be0b1c4e5b598d0f`. |
| 7 | Single feature theme | PASS | Full diff vs `origin/main` is 6 files: `.githooks/pre-commit`, `scripts/precommit_contract_test.go`, `cmd/gc/cmd_stop_test.go`, `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, `TESTING.md`. All directly serve one theme: land the dashboard-client-generation hook fix, its regression test, the flaky-test bound that test's related suite run surfaced, and the resource-census ratchet the new test's 2 `exec.Command` call sites require. |

## Commands

- `git merge-tree --write-tree origin/main HEAD`
- `git diff --stat origin/main...HEAD`
- `git diff origin/main...HEAD -- .githooks/pre-commit scripts/precommit_contract_test.go cmd/gc/cmd_stop_test.go internal/testpolicy/resourcecensus/census.go test/test-resources.toml TESTING.md | git patch-id --stable`
- `go test ./scripts/... -run 'TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged|TestPreCommitRegeneratesDashboardClientOnSpecChange' -count=1 -v`
- `go test ./cmd/gc/... -run 'TestCmdStopWallClockTimeoutBoundsDirectStop|TestDefaultStopWallClockTimeoutScalesWithConfiguredStopTargets' -count=3 -v`
- `make test-fast-parallel` (3 independent fresh runs this session)
- `go vet ./...`
- `go build ./cmd/gc/`
