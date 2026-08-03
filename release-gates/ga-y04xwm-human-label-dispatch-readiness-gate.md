# Release Gate: Human-label exclusion in dispatch readiness

- Deploy bead: `ga-y04xwm`
- Source issue: `ga-7yfnyv`
- Review molecule: `ga-nenay8`
- Reviewed commit: `1ad0583cc7e7812732a2963742d21d72407e622d`
- Candidate base: `a091c35cf233feccdf579069d70c06b8cc125512`
- Main evaluated: `origin/main@21146d906ff70a9915d9f2ef26a5124ba9e7cc24`
- Deploy branch: `deploy/ga-y04xwm-gate`
- Evaluated: `2026-07-28T15:34:22Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed commit and
`origin/main`, and `git log --all -- docs/PROJECT_MANIFEST.md` has no entries.
This checklist therefore applies the deployer role's release-gate criteria and
the source bead's full done-when checklist.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first, then rechecked after `origin/main` advanced during the gate. The final `git merge-tree --write-tree origin/main 1ad0583cc7e7812732a2963742d21d72407e622d` exited 0 and produced tree `76f0db0bd45014247d36b8cf9887c40a839fe1ba` against `origin/main@21146d906ff70a9915d9f2ef26a5124ba9e7cc24`. No self-rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | Closed review molecule `ga-nenay8` records `verdict: pass`, no style or security findings, no uncovered acceptance criteria, and a deploy pointer for the exact reviewed commit. The dedicated deploy bead copies that verified PASS marker into its notes. |
| 2 | Acceptance criteria met | **PASS** | The two config-side `bd ready` pool-demand constructors and both `cmd/gc` routed/unassigned `emit_ready` calls add `--exclude-label human`; the legacy ephemeral pool-demand selector adds the equivalent labels-array filter. Assignee-scoped paths and the shared `legacyEphemeralReadyFilterJQ` body are unchanged. Three config/golden focused tests passed, 22 `cmd/gc` readiness tests passed, and all 18 changed golden fixtures become byte-identical to their base versions after removing only the intended native flag and jq clause. The three regression tests were independently observed RED with the production edits reverted during review. `gofmt -l` was empty, `git diff --check` passed, and the stale gate file from the superseded implementation was not included. |
| 3 | Tests pass | **PASS** | Documented gate `make test-fast-parallel` passed all 10 jobs: core unit, Darwin compile, two runner self-tests, and six `cmd/gc` shards. Sanitized JSON evidence using the same Makefile/shard wrappers counted **34,135 PASS, 0 FAIL, 169 SKIP** (19,108/0/73 core plus 15,027/0/96 `cmd/gc`). The skips are the repository's intentional `GC_FAST_UNIT=1`, platform-only, live-dependency, and characterization skips; none is in the three feature regressions, whose focused runs counted 25 PASS, 0 FAIL, 0 SKIP. The open internally-authored PR's required process/integration CI is also green. `go vet ./...` and `go build ./...` exited 0. An initial auxiliary count replay inherited `GC_DISABLE_USAGE_METRICS` outside `TEST_ENV` and invalidly failed one product-metrics environment expectation; the exact sanitized replay above passed and is the recorded evidence. |
| 4 | No high-severity review findings open | **PASS** | Review molecule `ga-nenay8` reports no style findings, no security findings, no uncovered criteria, and no blockers. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, detached `1ad0583cc` had an empty `git status --porcelain=v1`; `git diff --check a091c35cf233feccdf579069d70c06b8cc125512..HEAD` passed. `git config core.hooksPath` is `.githooks`. This checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | All 22 changed files implement or verify one dispatch-readiness rule: human-flagged beads are excluded from routed/unassigned pool demand while already-assigned work remains eligible. Production edits are confined to `internal/config/workquery.go` and `cmd/gc/dispatch_runtime.go`; the remaining files are their tests and generated golden expectations. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 1ad0583cc7e7812732a2963742d21d72407e622d
git diff --check a091c35cf233feccdf579069d70c06b8cc125512..HEAD
gofmt -l internal/config/workquery.go internal/config/config_test.go cmd/gc/dispatch_runtime.go cmd/gc/cmd_convoy_dispatch_test.go
go test -json ./internal/config/... -run '^(TestEffectiveWorkQueryExcludesHumanLabeledRoutedWork|TestEffectiveWorkQueryLegacyEphemeralExcludesHumanLabeledRoutedWork|TestWorkQueryGolden)$' -count=1
GC_FAST_UNIT=1 go test -json ./cmd/gc/... -run '^TestWorkflowServeControlReadyQuery' -count=1
make test-fast-parallel
GC_FAST_UNIT=1 ./scripts/test-go-test-shard ./cmd/gc <shard> 6
go vet ./...
go build ./...
```
