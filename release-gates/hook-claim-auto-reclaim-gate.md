# Release gate: opt-in stale hook-claim recovery

- Deploy bead: `ga-f4t4v0`
- Review bead: `ga-4xr9eu`
- Reviewed commit: `1b6d0fa758b7bae035cb785978b0d1c87be04931`
- Base evaluated: `origin/main@7b09fbb37775dc107683f14ddedf08f943653a56`
- Deploy mode: `remote`

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-4xr9eu` is closed with `verdict: pass` and `deploy_commit: 1b6d0fa758b7bae035cb785978b0d1c87be04931`. The reviewer independently verified the rebased handwritten diff against the original reviewed change and reviewed the generated-artifact regeneration. No review carryover was used by this gate. |
| 2 | Acceptance criteria met | PASS | `gc hook --claim` now optionally asks the beads store to reclaim exactly the route-matched candidate whose only blocker is a stale assignee, then claims that same bead in the same hook cycle. No-reclaim responses leave the candidate untouched, budget-deferred candidates remain ineligible, and successful recovery emits the registered typed `hook.claim.reclaimed_stale` event. `auto_reclaim_stale_claims` is wired through agent config, patches, overrides, cloning, pool expansion, migration, schemas, and generated clients; its zero value keeps the path off by default. No dependency or storage-schema change is present. |
| 3 | Tests pass | PASS | The documented full local CI aggregate ran through the isolation wrapper with real rootless Podman. It produced three tracked, non-diff-owned failures; all satisfy criterion 3a attribution below. Every added or modified test ran twice and passed: 30 PASS / 0 FAIL / 0 SKIP. Generated API/dashboard gates and static checks also passed. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make fmt-check`, `make check-hooks`, `go build ./...`, and `go vet ./...` passed. `make lint` passed at the exact reviewed commit in a clean disposable worktree. |
| 3c | CI-config lane | PASS | Not applicable: the diff changes no workflow, matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | PASS | Review notes record no blockers, majors, minors, or security findings; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `make spec-ci` and `make dashboard-ci` regenerated no diff, and `git status --short` was empty after gate evaluation. |
| 6 | Branch diverges cleanly from main | PASS | After refreshing `origin/main`, `git merge-tree --write-tree origin/main 1b6d0fa758b7bae035cb785978b0d1c87be04931` exited 0 and produced tree `0ce51654883c3811831fd842b525c1cb865ea501`. The bounded self-rebase exception was not invoked. |
| 7 | Single feature theme | PASS | The five-commit range implements and verifies one behavior: default-off stale-assignee recovery for route-matched `gc hook --claim` candidates, plus the generated config/API/dashboard projections of that same setting and event. |

## Criterion 2 evidence

- Scoped store operation: `internal/beads.BdStore.ReclaimStale` invokes the reclaim operation for one explicit bead ID and returns the previous owner when recovery succeeds.
- Same-cycle behavior: `cmd/gc` retries claim only for the candidate it just reclaimed; an empty reclaim result does not mutate or claim the candidate.
- Eligibility: the route match and budget-deferred window are checked before any reclaim call.
- Default-off behavior: `AutoReclaimStaleClaims` is a zero-value boolean and the disabled branch never invokes the reclaim operation, preserving common-case latency.
- Observability: `events.HookClaimReclaimedStale` is present in `KnownEventTypes` and registered with `HookClaimReclaimedStalePayload`.
- Configuration integrity: field-sync, patch/override, clone/pool, persistence, schema, OpenAPI, Go-client, and TypeScript-client projections are covered by tests and successful regeneration.
- Scope integrity: `go.mod` and `go.sum` are unchanged; no database migration or new storage model is introduced.

## Criterion 3 evidence

### Full CI-equivalent aggregate

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- isolation: `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- container environment: rootless Podman at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`
- container images refreshed before the run: `dolthub/dolt-sql-server:1.32.4` (`sha256:b0400696...`) and `dolthub/dolt:2.1.7` (`sha256:eba699ca...`)
- result: 40 jobs started; 37 job PASS, 3 job FAIL
- verbose markers across all 40 shard logs: 90,492 PASS, 3 FAIL, 322 SKIP
- `skip_justification`: the 322 markers (179 unique test names) are declared platform/provider-only, opt-in live-infrastructure, helper-process, or lane-handoff skips in the repository's full aggregate. None is an added or modified test, and the process/integration lanes ran in this same command.
- no isolation tripwire fired
- aggregate log: `/var/tmp/gc-deploy-ga-f4t4v0-full.log`
- shard logs: `/var/tmp/gc-local-tests.avFdgC/*.log`

### Diff-owned tests

Each test below ran twice in the documented aggregate (fast and process/full coverage where selected) and recorded two PASS markers, with no FAIL or SKIP:

- `TestDoHookClaimReclaimsStaleAssigneeAndClaimsSameCycle`
- `TestDoHookClaimLeavesCandidateUntouchedWhenNothingReclaimed`
- `TestDoHookClaimFlagOffNeverAttemptsReclaim`
- `TestDoHookClaimEmitsReclaimedStaleEventOnSuccess`
- `TestDoHookClaimSkipsBudgetDeferredStaleAssigneeCandidate`
- `TestBdStoreReclaimStaleReturnsPreviousOwner`
- `TestBdStoreReclaimStaleReportsNothingReclaimed`
- `TestDeepCopyAgentCoversAllFields`
- `TestAgentFieldSync`
- `TestApplyAgentPatchCoversAllFields`
- `TestApplyAgentOverrideCoversAllFields`
- `TestApplyPatches_AgentAutoReclaimStaleClaims`
- `TestApplyAgentOverride_AutoReclaimStaleClaims`
- `TestAgentConfigFromAgentCoversPersistedFields`
- `TestAgentConfigFromAgentCarriesAutoReclaimStaleClaims`

`diff_tests_executed`: 30 PASS / 0 FAIL / 0 SKIP. `waiver_ref`: none.

### Failure attribution

Every raw failure is outside the changed tests and production paths. Both trackers predate this run, and the gate sightings were appended to each tracker.

| Failure | Tracker | Proof and path-overlap result |
|---|---|---|
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-e2z1zb` | The fixture stopped during `gc init`: a concurrent initializer observed the shared `hq` database mid-migration and beads refused migration from v54 to v66. The tracker names this exact random-vNN-to-v66 mechanism. The candidate does not touch initialization, migrations, this test, or its package. |
| `TestGraphWorkflowSuccessPath` | `ga-e2z1zb` | The same independently tracked initialization mechanism reproduced at v63 to v66 before feature behavior began. The candidate does not touch initialization, migrations, this test, or its package. |
| `TestSendReloadControlRequestNoChange` | `ga-vkhfnj` | The reload-control reply timed out after 4.30 seconds during the 40-job contention run. The same test and signature independently recurred on unrelated candidate `ga-jsjn2t` and is consolidated under the existing contention tracker. The candidate does not touch `cmd_reload.go` or `cmd_reload_test.go`, adds no test target or file, and does not change the resource census. |

- `failure_attribution`: mappings above
- clause (i), not diff-owned: satisfied for every failure
- clause (ii), tracked before this run: satisfied for every failure
- clause (iii), independent proof: named initialization mechanism for the first two failures; cross-PR recurrence for the reload timeout
- clause (iv), no path overlap: satisfied for every failure
- `inconclusive-guard`: not used; each attribution has landed independent proof

### Additional required gates

- `make test-ci-policy`: PASS
- `go build ./...`: PASS
- `make fmt-check`: PASS
- `make check-hooks`: PASS (`core.hooksPath` is `.githooks`)
- `go vet ./...`: PASS
- `make lint`: PASS, 0 issues, in a clean disposable worktree at the reviewed commit
- `make spec-ci`: PASS; regenerated OpenAPI and Go client are byte-clean
- `make dashboard-ci`: PASS; build, TypeScript checks, Go dashboard tests, and TypeScript client generation completed with no repository diff
- dashboard smoke: Vite preview served the generated SPA successfully on loopback
- `policy_lane`: `make test-ci-policy` — PASS
- `ci_lane_run`: not applicable; no CI configuration changed

## Decision

PASS. The three raw full-suite failures are independently attributable to existing, non-diff-owned conditions; all feature-owned tests, required policy/static lanes, generated-artifact checks, and acceptance criteria pass without a waiver.
