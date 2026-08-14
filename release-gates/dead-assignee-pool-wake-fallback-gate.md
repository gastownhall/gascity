# Release Gate: Dead-Assignee Pool-Wake Fallback

Bead: ga-877ml4
Source bead: ga-nnjcuc.1
Deploy source: 5f977aa23e762e95bc556fd1dae8539df3734ebb
Branch: deploy/ga-877ml4-gate
Gate evaluated: 2026-07-24

`docs/PROJECT_MANIFEST.md` is not present on this branch or on current
`origin/main`; this gate uses the deployer prompt's release criteria.

## Summary

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git merge-tree --write-tree origin/main 5f977aa23e762e95bc556fd1dae8539df3734ebb` exited 0 and produced tree `7677296004721b01593f86fd640d33941415b845`. No self-rebase path used. |
| 1 | Review PASS present | PASS | Source bead `ga-nnjcuc.1` notes contain `REVIEWER VERDICT: PASS` and `Verdict: PASS`; deploy bead `ga-877ml4` records the reviewed commit in metadata. |
| 2 | Acceptance criteria met | PASS | Diff against merge base `bac288647e0bbbbe2e68bdbe588709eb2827f5ee` touches exactly the expected 8 `cmd/gc` files, +497/-22. Targeted tests cover dead assignee demand, live/idle/asleep/unknown exclusions, ambiguous dead assignees, route/template precedence, max-active-session path reuse, and ready-exclude/blocking-dependency guards. |
| 3 | Tests pass | PASS | `gofmt -l` on all 8 changed files: clean. `go vet ./...`: clean. `go build ./...`: clean. `git diff --check $(git merge-base origin/main HEAD) HEAD`: clean. Focused acceptance sweep `go test ./cmd/gc/... -run 'TestFilterAssignedWorkBeadsForPoolDemand|TestBuildDesiredState|TestDeadAssignee' -count=1 -v`: PASS (`ok github.com/gastownhall/gascity/cmd/gc 2.364s`). Final isolated pre-push run from `/var/tmp` passed all fast jobs: `unit-core`, `fsys-darwin-compile`, and `unit-cmd-gc` shards 1-6. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no blocking security findings. The only disclosed issue is a non-blocking scalability follow-up, `ga-cge2ii`, for an unbounded/doubled closed-session query. High-severity unresolved finding count: 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. This gate file is the only deployer-added change and will be committed on `deploy/ga-877ml4-gate`; status is rechecked before push. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem: `cmd/gc` pool desired-state and assigned-work demand computation. The independent push-ownership-guard regex fix from the source branch is absent from this deploy candidate and was released separately. |

## Acceptance Evidence

- Confirmed-dead assignees are treated as demand for the correct template:
  `TestBuildDesiredStateDeadAssigneeCountsAsPoolDemand`,
  `TestFilterAssignedWorkBeadsForPoolDemandUsesClosedSessionTemplateFallback`,
  and dead-assignee route/template precedence tests passed.
- Live, idle, asleep, unknown, and ambiguous identities do not trigger fallback:
  negative coverage passed in the focused acceptance sweep.
- Demand computation remains mechanical and config/object-model driven:
  the diff adds pure map/precedence logic and threads it through existing
  demand and pool-sizing paths.
- Existing ready-exclude and blocking-dependency gates are preserved:
  guard tests passed, and no ready-exclude logic changed beyond additive
  plumbing.
- No hardcoded role names were introduced in the deploy diff.

## Nested-Worktree Baseline Note

An earlier `make test-fast-parallel` run from the live nested deployer worktree
failed only `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`,
tracked as `ga-y4se3w`. Deployer reproduced the same failure on a detached
`origin/main` worktree at `89c96220a`:

```text
providers_test.go:1469: factory provider = *auto.Provider, want injected provider *runtime.Fake
ORIGIN_MAIN_TEST_RC=1
```

That confirmed the nested-worktree red as a pre-existing mainline/live-city
test failure, not a regression from the deploy diff. The final pre-push gate
was then rerun from an isolated `/var/tmp` worktree and passed all fast jobs.
