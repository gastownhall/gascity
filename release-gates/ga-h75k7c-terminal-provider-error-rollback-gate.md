# Release Gate: ga-h75k7c terminal-provider-error rollback

**Verdict:** **PASS**

- **Deploy bead:** ga-h75k7c
- **Build bead:** ga-rwrbzi
- **Review bead:** ga-oc045p
- **Reviewed SHA:** 1659ba0795729dc1fbcc73a6bd0af2278a86c536
- **Gated SHA after bounded self-rebase:** bcfb958734808662e74f88be44c48da4966dafdc
- **Base:** origin/main at 51eb05c894
- **Deploy mode:** remote
- **Gate run:** 2026-09-24

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead ga-oc045p is closed with `verdict: pass` and records deploy commit 1659ba0795729dc1fbcc73a6bd0af2278a86c536. Criterion-6 staleness was resolved by bounded self-rebase to bcfb958734808662e74f88be44c48da4966dafdc before deploy tests ran. |
| 2 | Acceptance criteria met | PASS | Build bead ga-rwrbzi done-when items were implemented: terminal provider-error marking now rides the fenced pending-create rollback transaction; two new RED-first tests were added and pass; the pre-existing tracked test `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback` passes unmodified. Targeted verification: `/var/tmp/ga-h75k7c-diff-owned-tests.log`, `go test -count=1 ./cmd/gc -run '^(TestCommitStartResult_TerminalProviderErrorRollsBackPendingCreate\|TestCommitStartResult_TerminalProviderErrorLeavesSupersededPendingCreateUntouched\|TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback)$'`, exit 0. |
| 3 | Tests pass | PASS | Full-scope command run via isolation wrapper in merge scratch `/var/tmp/gc-merge-validate.ga-h75k7c.pVxCdv`: `make test-local-full-parallel`, log `/var/tmp/ga-h75k7c-test-local-full-parallel.log`, start 2026-09-24T15:13:45-07:00, end 2026-09-24T15:44:16-07:00. Job counts: 39 PASS, 1 FAIL attributed, 0 SKIP. The only failure was non-diff-owned `internal/runtime/herdr.TestProviderLiveClaudeKindPath` in `integration-packages-core-2-of-4`, exact tracked `agent_pane_busy` / `w1:p1` condition. Tracker ga-iepsvr was opened and sighting was appended; fix bead ga-iwanrj is open and unlanded with `gc.fixes_tracker=ga-iepsvr`. Mechanism proof: diff files are only `cmd/gc/session_lifecycle_parallel.go`, `cmd/gc/session_lifecycle_parallel_test.go`, `cmd/gc/session_reconcile.go`; `go list -deps -test ./internal/runtime/herdr` does not include `github.com/gastownhall/gascity/cmd/gc` (`CMD_GC_UNREACHABLE`). This deploy is fix-carrying (`build_bead=ga-rwrbzi`, fixing ga-z8yi2j), so criterion 3a attributes the repeat tracked failure under the fix-carrying rule. `failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr (fix bead ga-iwanrj, mechanism proof, no path overlap)`. `diff_tests_executed`: three named cmd/gc tests above PASS. `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` via isolation wrapper passed, log `/var/tmp/ga-h75k7c-policy-lane.log`, exit 0. Changed static lane run from the deploy worktree with `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main`: `make lint-affected && make fmt-check-changed`, log `/var/tmp/ga-h75k7c-static-lane.log`, exit 0; output included `lint-affected: ./cmd/gc` and `0 issues`. |
| 3c | CI-config diff lane | PASS | No CI workflow, action, required-check, timeout, or matrix files changed. Diff is limited to three `cmd/gc` Go files. |
| 4 | No high-severity findings | PASS | Review bead ga-oc045p records `style_findings: none`, `security_findings: none`, and `verdict: pass`; deploy diff review found no new high-severity issue. |
| 5 | Clean branch/worktree | PASS | Deploy worktree is clean at bcfb958734808662e74f88be44c48da4966dafdc on `builder/ga-rwrbzi`; `git status --short` produced no output before writing this gate file. |
| 6 | Branch current with base / merge-validated | PASS | Initial reviewed SHA 1659ba0795729dc1fbcc73a6bd0af2278a86c536 was stale against origin/main; bounded self-rebase succeeded, producing AFTER_SHA bcfb958734808662e74f88be44c48da4966dafdc. Merge validation materialized `MERGE_TREE_SHA=fdd50af3abb185f0084ee85f52460fd54157c11c` at `/var/tmp/gc-merge-validate.ga-h75k7c.pVxCdv`; `go build ./...` and `go vet ./...` passed from 2026-09-24T15:12:32-07:00 to 2026-09-24T15:12:46-07:00. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem/theme: pending-create rollback behavior for terminal provider errors in `cmd/gc` session lifecycle/reconcile code plus its tests. Diff: 3 files, 152 insertions, 24 deletions. |

## Notes

- Preflight already-merged check found no PRs for the reviewed SHA.
- `DEPLOY_MODE=remote`; origin is GitHub, push target resolved to `fork` because dry-run push to origin failed.
- No waiver was used.
