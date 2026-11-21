# Release gate: delivered-but-unobserved nudge handling (`ga-8p7sl2`)

- Deploy bead: `ga-8p7sl2`
- Source build beads: `ga-civwyz`, `ga-ob71a1`
- Review bead: `ga-h3nj8q`
- Reviewed source: `5d476b675cf2fa2ccde006348c3770fb0fa5acbb`
- Source branch (provenance only): `builder/ga-civwyz`
- Base: `origin/main@99543af567060da509f1e6400f485481d90c90e6`
- Merge base: `0d73c75e3091de7be9b42bc1554f9f1c94177f1a`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-14
- Overall: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-h3nj8q` records `verdict: pass` for the exact reviewed SHA, with no review carryover. |
| 2 | Acceptance criteria met | **PASS** | Independent source inspection confirmed the drained-composer classifier; the delivered-but-unobserved sentinel; success/ack handling for direct, mail, and queued nudges; startup retry suppression; the unchanged fence-mismatch path; and the private-`TMPDIR` repair for the prior full-suite contention failure. All 11 diff-owned test groups produced executed PASS evidence after applying the recorded real-tmux lane waiver. |
| 3 | Tests pass | **PASS** | The documented full-suite command completed all 40 jobs: 38 PASS and 2 attributed raw FAIL, with 46,970 PASS / 2 FAIL / 225 SKIP top-level executions. Nine diff-owned groups passed there. The two diff-added real-tmux groups skipped in that lane, then both ran and passed in the exact targeted `GC_TMUX_INTEGRATION=1` lane authorized by `waiver_ref` `mayor-2026-09-13-1948Z-ga-82s3eo`, explicitly extended to this bead. The targeted run had 2 top-level PASS / 0 FAIL / 0 SKIP and 8 passing subtests. |
| 3a | Pre-existing failures may be attributed | **PASS** | `TestProviderLiveClaudeKindPath` is attributed to open predating tracker `ga-iepsvr`: the identical `agent_pane_busy` condition has a same-command, same-diff before/after control, and this diff has no `internal/runtime/herdr` path overlap. `TestAdoptPRFormulaRetriesTransientReviewerStep` is attributed to open predating condition tracker `ga-lejnse`: it failed during `gc init` on the shared-server schema-migration refusal before the reviewed nudge path could execute, and its test file/package does not overlap the diff. Both sightings were appended to their trackers. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed all Python and Go policy checks. `make vet` passed. `git diff --check`, changed-Go formatting, and `make check-hooks` also passed. |
| 3c | CI-config diff lane | **PASS** | `ci_lane_run: n/a` — the diff does not modify CI jobs, matrices, timeouts, or required-check configuration. |
| 4 | No high-severity review findings open | **PASS** | `ga-h3nj8q` records no style or security findings and no unresolved high-severity finding. |
| 5 | Final branch is clean | **PASS** | The exact-SHA evaluation worktree was clean before this gate record was written. Two unrelated stale gate drafts were set aside outside the repository rather than included in this candidate. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main 5d476b675cf2fa2ccde006348c3770fb0fa5acbb` exited 0 and produced merge tree `ffe8f41b601650ed8b22ee1ee4b39213101b8527`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The commit set has one coupled theme: classify delivered-but-unobserved tmux nudge submission as delivery, prevent duplicate retry/queue behavior, preserve that classification through startup, and repair the test isolation and manifests needed to gate it. |

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`, invoked through `packs/actual/all/scripts/isolated-test-run.sh`
- `test_cmd_scope`: `full-suite` plus the authorized targeted real-tmux lane for the two opt-in tests
- `test_counts`: full suite — 40 jobs, 38 PASS, 2 attributed FAIL, 0 unexecuted; 46,970 PASS, 2 attributed FAIL, 225 SKIP top-level tests. Authorized targeted lane — 2 PASS, 0 FAIL, 0 SKIP top-level tests; 8 PASS subtests.
- `diff_tests_executed`: 11 PASS, zero FAIL, zero unresolved SKIP after the authorized lane:
  - `TestDeliverSessionNudgeWithWorkerAcksDeliveredUnobservedInsteadOfFailing` PASS
  - `TestSendMailNotifyWithWorkerAcksDeliveredUnobservedInsteadOfDuplicating` PASS
  - `TestTryDeliverQueuedNudgesByPollerAcksDeliveredUnobservedInsteadOfRetrying` PASS
  - `TestSecretEnvIsAbsentFromProcCmdline` PASS
  - `TestDoStartSessionReturnsNudgeDeliveryError/delivered-but-unobserved_submit_is_not_fatal` PASS
  - `TestSendStartupNudgeWithRetry_DeliveredButUnobservedNeverRetried` PASS
  - `TestPaneShowsDrainedComposer` PASS, including the still-drafted composer cases
  - `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory` PASS
  - `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce` PASS
  - `TestNudgeConfirmBusyRenderLag` PASS in the authorized lane, 2/2 subtests
  - `TestNudgeConfirmBudgetThreshold` PASS in the authorized lane, 6/6 subtests
- `skip_justification`: the full suite's two diff-owned SKIPs are covered only by the recorded merge-authority waiver and the executed targeted PASS lane above. The other 223 are existing suite-controlled helper, platform, privilege, and live-provider opt-in skips; none is a test added or modified by this diff.
- `waiver_ref`: `mayor-2026-09-13-1948Z-ga-82s3eo` — the mayor extended the existing exact-test `GC_TMUX_INTEGRATION=1` lane decision to `ga-8p7sl2` at 2026-09-14 09:58Z.
- `ci_lane_run`: n/a — no CI-config change
- Rootless Podman 5.8.4 was healthy. The refreshed test pins resolved as `dolthub/dolt-sql-server:1.32.4@sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d` and `dolthub/dolt:2.1.7@sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`.
- Full log: `/var/tmp/ga-8p7sl2-full.log`
- Shard logs: `/var/tmp/gc-local-tests.j7Bqrt`
- Authorized real-tmux log: `/var/tmp/ga-8p7sl2-waived-tmux.log`
- Policy log: `/var/tmp/ga-8p7sl2-policy.log`
- Vet log: `/var/tmp/ga-8p7sl2-vet.log`

## Raw failure attribution

- `failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | clause 3: cross-run control — the same full-suite command on an unrelated diff previously produced both this exact agent_pane_busy failure and a PASS; clause 4: no path overlap with internal/runtime/herdr`
- `failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-lejnse | clause 3: mechanism — gc init stopped at the external bd shared-server migration gate before formula adoption or nudge delivery; clause 4: no path overlap with test/integration/review_formula_test.go`

## Disposition

All release criteria pass. Cut the isolated `deploy/ga-8p7sl2-gate` branch from the exact reviewed SHA, commit this checklist there, push only that isolated branch, open the pull request, publish deploy clearance on its exact head, and route the merge-request to the merge authority.
