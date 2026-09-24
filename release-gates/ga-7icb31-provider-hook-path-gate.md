**Verdict:** **PASS**

# Release Gate: provider hook PATH and GC_BIN lookup

Bead: ga-7icb31
Build bead: ga-5korc0
Review bead: ga-difxk1
Reviewed commit: 6e58758993fbf9590ffab79270a2a7288b1367ae
Base ref: origin/main
Base tip at gate: 51eb05c8946697103665c53dc96649277e3bab69
Merge base: f9b8b2ebf02220d11e0e8e1b9d9faafa38715334
Materialized merge commit: 234b6f7c519d237d30840984f698c1c1c38d989a
Gate date: 2026-09-24

## Summary

PASS. This is a single-theme provider hook fix: the codex, copilot, and
antigravity managed hook overlays now append user bin directories after the
current PATH and invoke `${GC_BIN:-gc}` instead of a bare `gc`, preventing a
stale host-installed binary from downgrading a fresh city's `gc-beads-bd` shim.

The gate was evaluated on a materialized merge of the reviewed commit over the
current `origin/main`. The full `make test` lane still hits one known red-main
failure, `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`;
that failure is tracked by ga-z8yi2j, has unlanded fix bead ga-rwrbzi, and is
not in this deploy's diff-owned paths. All diff-owned tests and the acceptance
critical integration test passed on the merge tree.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-difxk1 closed with PASS for reviewed commit `6e58758993fbf9590ffab79270a2a7288b1367ae`; reviewer recorded build/vet/lint clean, focused hook and packlint tests passing, `TestCleanInstallTutorialPath` passing, and no blocking findings. |
| 2 | Acceptance criteria met | PASS | The reviewed diff updates all codex, copilot, and antigravity managed hook command strings to append PATH tails and use `${GC_BIN:-gc}`. Structural check on the merge tree listed 11 managed commands in the new shape and found no old `PATH="$HOME/go/bin...` prepend or bare `gc prime` / `gc hook` pattern in those three overlay files. Doctor goldens and hook tests were updated. The acceptance-critical command `go test -tags integration -timeout 25m ./test/integration -run '^TestCleanInstallTutorialPath$' -v` passed on the materialized merge tree in 111.152s. |
| 3 | Tests pass | PASS | `go build ./...` PASS and `go vet ./...` PASS on materialized merge commit `234b6f7c519d237d30840984f698c1c1c38d989a`. Full lane `make test` ran through `isolated-test-run.sh` with podman testcontainers setup; the only failing test was `github.com/gastownhall/gascity/cmd/gc TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`, attributed under criterion 3a to red-main tracker ga-z8yi2j with unlanded fix bead ga-rwrbzi. Diff-owned hook, doctor, and packlint tests passed. Policy lane `make test-ci-policy` PASS. No CI config files changed, so criterion 3c is N/A. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on ga-difxk1 report no blocking findings and no security issue; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Role worktree and merge scratch were clean before writing this gate record. `.githooks` ownership verified with `make check-hooks` on the merge scratch: `core.hooksPath OK: .githooks gates are active.` The deploy branch will contain only the reviewed commits plus this gate record commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 6e58758993fbf9590ffab79270a2a7288b1367ae` succeeded with tree `996a51b8226843e2cddc99b35756d0d2fbc741d8`; materialized merge commit `234b6f7c519d237d30840984f698c1c1c38d989a` built and vetted cleanly. |
| 7 | Single feature theme | PASS | Diff is scoped to one behavior: managed provider hook command lookup/PATH handling plus directly related doctor, hook, and packlint test coverage. Files changed: the three provider overlay JSON files, `internal/hooks`, `cmd/gc/doctor_codex_hooks_test.go`, and `test/packlint/managed_prompt_hook_timeout_test.go`. |

## Test Details

- PASS: `go build ./...` on `/var/tmp/gc-merge-validate.jsG7Br`
- PASS: `go vet ./...` on `/var/tmp/gc-merge-validate.jsG7Br`
- PASS with attributed red-main failure: `make test` via `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test'`
- PASS: `make test-ci-policy` via the isolation wrapper
- PASS: `go test -tags integration -timeout 25m ./test/integration -run '^TestCleanInstallTutorialPath$' -v`
- PASS: `make check-hooks`

## Criterion 3a Attribution

The full `make test` lane produced exactly one failing test:

`github.com/gastownhall/gascity/cmd/gc TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`

Attribution evidence:

- Tracker ga-z8yi2j predates this gate and covers the same test and signature:
  `row gc-2 status "open" after a confirmed teardown, want closed`.
- Fix bead ga-rwrbzi is stamped `gc.fixes_tracker=ga-z8yi2j`, but its commit
  `1659ba0795729dc1fbcc73a6bd0af2278a86c536` is not landed on `origin/main`;
  it is only on `origin/builder/ga-rwrbzi`, with review bead ga-oc045p still in
  progress at gate time.
- This deploy is itself a fix-carrying deploy for tracker ga-vkhfnj via build
  bead ga-5korc0, so criterion 3a uses the fix-carrying repeat-signature rule
  rather than failing on an already-tracked, unrelated red-main condition.
- The failing test file and pool/session provider rollback behavior are not in
  this deploy's diff. The only changed `cmd/gc` file is
  `cmd/gc/doctor_codex_hooks_test.go`.
- The tracker was updated during this gate with the log paths
  `/var/tmp/ga-7icb31-make-test.log` and `/var/tmp/gascity-test.jsonl.kuM8ir`.

## Diff-Owned Test Evidence

The full test JSON contains PASS events for the changed hook and doctor tests,
including:

- `github.com/gastownhall/gascity/internal/hooks TestOverlayProviderHookCommandsAppendPathAndUseGCBinVar`
- `github.com/gastownhall/gascity/internal/hooks TestCodexHooksMissingManagedPreCompact`
- `github.com/gastownhall/gascity/internal/hooks TestCodexHooksNeedManagedUpgrade`
- `github.com/gastownhall/gascity/cmd/gc TestCodexHooksConvergeWithSkipStaging`
- `github.com/gastownhall/gascity/cmd/gc TestCodexHooksDriftCheckReportsManagedMissingPreCompact`
- `github.com/gastownhall/gascity/cmd/gc TestCodexHooksDriftCheckFixUpgradesManagedHooks`
- `github.com/gastownhall/gascity/cmd/gc TestCodexHooksDriftCheckFixBindsAgentWorkDirToCityRoot`
- `github.com/gastownhall/gascity/cmd/gc TestCodexHooksMissingPreCompactRequiresManagedCommand`
- `github.com/gastownhall/gascity/test/packlint TestManagedPromptHookTimeoutExceedsWrapper`

## CI Config

No `.github`, `*.yml`, or `*.yaml` files changed in
`origin/main...6e58758993fbf9590ffab79270a2a7288b1367ae`, so no CI-config
specific lane was required.
