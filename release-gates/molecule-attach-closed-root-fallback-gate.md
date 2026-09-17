# Release gate: self-root re-attached work after a closed molecule

- Deploy bead: `ga-rgujcq`
- Review bead: `ga-7ca42n`
- Source build bead: `ga-yov1rr`
- Reviewed commit: `c50080ab90243fa45199a0a3e2d0911aa45f7e06`
- Base evaluated: `origin/main@4f1ae35cded38da71a18cab17bf536393a09bc65`
- Deploy mode: `remote`

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-7ca42n` is closed with `verdict: PASS` for exact commit `c50080ab90243fa45199a0a3e2d0911aa45f7e06`. The deploy bead records the same reviewed SHA. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `molecule.Attach` now checks the resolved upstream root and falls back to the attach bead itself when that root is closed or cannot be read. The new regression test closes a prior molecule root, re-attaches the content bead, and verifies the new graph self-roots instead of inheriting the stale foreign root. Existing live-root propagation coverage remains green. The change is confined to the Go molecule boundary; no formula/template, dependency, or schema change is present. |
| 3 | Tests pass | PASS | The documented full local CI aggregate ran through the isolation wrapper with real rootless Podman. Four raw failures all stopped in the same tracked concurrent-initializer schema condition before molecule behavior; they satisfy criterion 3a attribution below. The diff-owned regression test ran twice and passed twice with no FAIL or SKIP. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make fmt-check`, `make check-hooks`, `go build ./...`, and `go vet ./...` passed. `make lint` passed with 0 issues in a clean disposable worktree at the reviewed commit. |
| 3c | CI-config lane | PASS | Not applicable: the diff changes no workflow, matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | PASS | Review notes record no blockers or high-severity findings. The reviewer filed the non-blocking cross-store-root edge as follow-up `ga-yg3vst`; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty after gate evaluation. |
| 6 | Branch diverges cleanly from main | PASS | After refreshing `origin/main`, `git merge-tree --write-tree origin/main c50080ab90243fa45199a0a3e2d0911aa45f7e06` exited 0 and produced tree `9e6a9ee37e836897ba1607bd8275d64cb090d302`. The bounded self-rebase exception was not invoked. |
| 7 | Single feature theme | PASS | The two-commit, two-file range changes one behavior in one subsystem: molecule root selection when re-attaching work whose prior run chain is closed. |

## Criterion 2 evidence

- Closed-chain recovery: the resolved run root is read before child steps are created; a closed root causes `rootBeadID` to fall back to the attach bead.
- Regression coverage: `TestAttachFallsBackToSelfWhenRunChainRootIsClosed` creates and closes the prior root, re-attaches the content bead, and asserts self-rooting.
- Live-chain preservation: the pre-existing `TestAttachPropagatesStoreRef` coverage remains green, preserving the behavior required by the earlier live-upstream-root fix.
- Scope: only `internal/molecule/molecule.go` and `internal/molecule/attach_test.go` change. No formula/template, module dependency, storage schema, CI configuration, or test-resource census changes.
- Review follow-up: `ga-yg3vst` separately tracks how a live root in a different store should be resolved. The reviewed change's failure direction is conservative self-rooting and the follow-up is not part of this fix's acceptance contract.

## Criterion 3 evidence

### Full CI-equivalent aggregate

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- isolation: `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- container environment: rootless Podman at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`
- pinned images present: `dolthub/dolt-sql-server:1.32.4` (`sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d`) and `dolthub/dolt:2.1.7` (`sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`)
- result: 40 jobs started; 36 job PASS, 4 job FAIL
- verbose markers across all shard logs: 90,473 PASS, 4 FAIL, 322 SKIP
- `skip_justification`: the 322 markers (179 unique test names) are declared platform/provider-only, opt-in live-infrastructure, helper-process, or lane-handoff skips in the repository's full aggregate. None is the diff-owned regression test, and the process/integration lanes ran in this same command.
- no isolation tripwire fired
- aggregate log: `/var/tmp/gc-deploy-ga-rgujcq-full.log`
- shard logs: `/var/tmp/gc-local-tests.eXEmHz/*.log`

### Diff-owned test

- `TestAttachFallsBackToSelfWhenRunChainRootIsClosed`: PASS twice, 0 FAIL, 0 SKIP
- `diff_tests_executed`: 2 PASS / 0 FAIL / 0 SKIP
- `waiver_ref`: none

### Failure attribution

All four failures share the same root condition: a concurrent initializer observes a fresh `hq` database partway through migration and the beads shared-server guard refuses the random vNN-to-v66 cursor. Root cause and fix are tracked by pre-existing bead `ga-e2z1zb`, with standing root-cause evidence in `ga-lejnse`. Each fixture failed during city/beads initialization before molecule attach behavior began, and the candidate has no path overlap with the failing test files or initialization code.

| Failure | Tracker | Proof and path-overlap result |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `ga-e2z1zb` | `gc init` refused hq v59→v66 in `cmd-gc-process-4-of-6`. Mechanism proof: the fixture stopped in beads initialization before molecule code; the same test/signature predates this run in `ga-lejnse`. No changed path overlaps `cmd/gc/cmd_bd_test.go` or initialization. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `ga-e2z1zb` | `gc init` refused hq v38→v66 in `integration-review-formulas-retries-2-of-2`, before formula execution. No changed path overlaps the integration fixture or initialization. |
| `TestCleanInstallTutorialPath` | `ga-e2z1zb` | `gc init` refused hq v60→v66 in `integration-rest-full-2-of-8`. The tracker record already names this exact test and random-cursor condition. No changed path overlaps the integration fixture or initialization. |
| `TestHumaBinary_SessionMessageAsync` | `ga-e2z1zb` | Async city creation refused hq v39→v66 in `integration-rest-full-3-of-8`, before session-message or molecule behavior. No changed path overlaps the API fixture or initialization. |

- `failure_attribution`: mappings above; each sighting was appended to `ga-e2z1zb` and read back successfully
- clause (i), not diff-owned: satisfied for every failure
- clause (ii), tracked before this run: satisfied for every failure
- clause (iii), independent proof: mechanism/root-cause proof landed; every failure terminated inside pre-feature beads initialization
- clause (iv), no path overlap: satisfied for every failure
- `inconclusive-guard`: not used; the mechanism proof is conclusive

### Additional required gates

- `make test-ci-policy`: PASS
- `make fmt-check`: PASS
- `make check-hooks`: PASS (`core.hooksPath` is `.githooks`)
- `go build ./...`: PASS
- `go vet ./...`: PASS
- `make lint`: PASS, 0 issues, in a clean disposable worktree at the reviewed commit
- `policy_lane`: `make test-ci-policy` — PASS
- `ci_lane_run`: not applicable; no CI configuration changed

## Decision

PASS. The four raw failures are separate sightings of one independently tracked, non-diff-owned initialization race; the molecule change, its new regression test, and every required policy/static lane pass without a waiver.
