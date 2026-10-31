# ACP interrupt fake release gate

**Verdict:** **PASS**

Deploy bead: ga-buu3au7. Build/investigation: ga-88lxs10. Review: ga-r6y2c69.
Evaluated 2026-10-17; resumed the existing detached run and audited its retained results.

- deploy_mode: remote
- deploy_source: `6d72d99d1ab93bb63f83c3226f9511f1b5828ce4` (resolved commit; source branch is provenance only)
- deploy_branch: `deploy/ga-buu3au7-gate`
- gate_base: `7e4c305eb3eac790e6aeaaf07523c865edbc5775`
- gated_merge: `25428f1169f7c21b39251e6161596663b2353de9`
- gated_tree: `13a1eafcba2b8c06f9454ba90841fa5d305c80b0`
- merge_state: clean (origin/main@7e4c305eb3eac790e6aeaaf07523c865edbc5775)
- gate_base_kind: current-tip
- merge_validation: merge result built and vetted here; merge authority verifies the current merge result at merge time
- evidence_dir: `/var/tmp/deploy-ga-buu3au7._6ccisdj` (retained on the deploy host)
- merge_scratch: `/var/tmp/gc-merge-validate.cM8eW5zs`

The authenticated commit-to-PR lookup returned no PR at intake and again before publication.
A fresh origin fetch before publication still resolved main to the frozen gate base,
and merge-tree reproduced the gated tree. No rebase or source-branch push was performed.
`docs/PROJECT_MANIFEST.md` is absent; the canonical commands come from `TESTING.md`,
`Makefile` and `.github/workflows/bazel.yml`.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Live review ga-r6y2c69 records verdict pass and binds to the exact deploy source. No carryover or reviewer-added commit. |
| 2 | Acceptance criteria met | PASS | SIGINT is blocked before threads start; a daemon thread consumes it through sigwait. The existing log, ignore, cancel and exit paths remain exercised by all 12 helper consumers listed below. |
| 3 | Full-scope tests pass | PASS | Four fresh remote phases plus the required local floor completed; 116810 PASS, 0 FAIL, 370 SKIP top-level result occurrences. Every helper consumer passed by name. |
| 4 | No unresolved HIGH findings | PASS | Reviewer style/security findings are none; the inherited-SIG_IGN/macOS observation is explicitly non-blocking. |
| 5 | Branch clean | PASS | Source worktree and merge scratch were clean at evaluation; this gate record is the only added path. Final clean status is checked before push. |
| 6 | Merge state and build/vet | PASS | Clean merge against the frozen base; Bazel build of //... and make vet completed on its merge result. See criterion6-result.txt, build-nogo.log and vet.log. |
| 7 | Single feature theme | PASS | One test-fixture change in internal/runtime/acp/interrupt_test.go, +17/-5. No production, configuration, dependency or CI changes. |

## Criterion 3 execution

- test_cmd: `gate-criterion3.sh --repo <merge_scratch> --base <gate_base> --bead-json <evidence_dir>/bead.json --owned <evidence_dir>/owned.tsv --run-dir <evidence_dir>/c3-evidence`, launched by `gate-detached-run.sh` as a systemd user service
- test_cmd_scope: full-suite
- heavy_mode: none (mandatory classifier result retained in heavy-classify.txt and c3-evidence/classify.txt)
- test_exec_mode: remote-exec (derived from BEP)
- test_exec_bep: `c3-evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json` under evidence_dir; corresponding .evidence.json files have no predicate errors
- rbe_state: rc present, kill switch absent, certificate expires 2027-01-09; tree/main worker-env pins match (`sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0`)
- fallback_reason: none; no remote/local suite stitching
- admission: remote, floor 25 GiB, free 95.29 GiB, cap 2, waited 0.001 s
- farm_test_seconds: 8825.051
- test_log_dir: `/var/tmp/deploy-ga-buu3au7._6ccisdj/c3-evidence/remote/logs`
- test_counts: 116810 PASS, 0 FAIL, 370 SKIP (top-level result lines across 506 retained job logs; repeated executions count per job)
- diff_tests_executed: none (no test function body is added or changed; tool output and SHARED-HUNKS explanation are retained in owned-explain.txt)
- helper_consumers_executed: 12 PASS, 0 FAIL, 0 SKIP (supplementary by-name verification in the full-suite output)
- waiver_ref: none
- failure_attribution: none (no failed test or lane)
- owned_topup: empty; no missing or skipped diff-owned test
- floor_topup: complete=true, 16/16 steps exit 0; `c3-evidence/remote/floor-topup.json`
- skip_inventory: full reasons in execution.json and `c3-evidence/remote/skip-inventory.json`; native reasons in retained floor-logs
- ci_lane_run: n/a (no CI configuration change)

| Phase | Fresh remote results | Cached | Retries | Predicate |
|-------|----------------------|--------|---------|-----------|
| unit | 371 | 0 | 0 | PASS |
| acceptance | 24 | 0 | 0 | PASS |
| integration-packages | 81 | 0 | 0 | PASS |
| integration-full | 14 | 0 | 0 | PASS |

Each phase forced `--nocache_test_results --noremote_local_fallback`, with attempt 1,
strategy remote, a start inside the phase window and a terminal verdict for every result.
Commands reproduce the unit `bazel test //...`, acceptance
`bazel test --config=acceptance //test/acceptance:acceptance_test`, integration-packages
`bazel test --config=integration //test:integration_packages`, and full integration
`bazel test --config=integration //test/integration:integration_test` lanes with CI flags.
Full integration used no smoke filter. `TestTutorial01` passed in integration-packages.

The required native floor reran every environment-absence selection by package,
selecting parents for subtests, plus the pinned beadstest check, Darwin compile,
push-gate-lock self-test and local-concurrency self-test. It recorded 21 PASS,
0 FAIL and 14 SKIP top-level results (included in the aggregate above).
Native top-ups scrubbed live GC/BD/BEADS/DOLT store selectors and used non-interactive
Git credentials. The outer isolation wrapper reported pass-through for the clean
scratch path; remote actions used Bazel's declared inputs, and the floor runner
explicitly scrubbed those selectors rather than relying on pass-through isolation.

skip_justification: the remaining skips are pre-existing and outside this helper:
opt-in/live-provider, unavailable external fixture/tool/backend, intentional
capability/characterization and platform-specific rows. Environment-absence rows
were retried through the computed native floor; 14 top-level skips remain there:
replay/simulation fixtures (2), opt-in live herdr journeys (7), unavailable br (1),
and tests requiring an enclosing tmux session (4). No skipped row is in the changed
ACP helper or any of its 12 consumers; its shared hunks change only the inline
Python signal implementation and cannot reach the unrelated skip sites. Detailed
per-test reasons remain available in the retained logs and inventory.

## Fast lane and load evidence

The format, policy/lint and generated-file drift lanes passed before the suite.
Each ran in a separate fresh-tree view; none modified the suite's merge scratch.

- policy_lane: PASS — pinned formatter; `make lint-changed` against the frozen base; `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary`; Bazel format, goreleaser, CI policy/analytics/coverage and dependency/boundary guards (policy.log)
- drift_lane: PASS — `make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec`, `git diff --exit-code`, empty `git status --porcelain` (drift.log)
- check-hooks: PASS — .githooks active (hooks.log); committing this record runs pre-commit again
- remote load admission: no host-load wait or heavy slot, as prescribed for remote execution
- local floor load_threshold: 15
- local floor load_waited_seconds: 1801
- local floor load_wait_timed_out: 1 (ordinary soft timeout; command proceeded)
- local floor run_start_load: 47.09
- local floor run_max_load: 56.11
- local floor run_mean_load: 49.18
- local floor run_readings: 12
- local floor wait_first_load: 34.56
- local floor wait_max_load: 64.47
- local floor wait_mean_load: 41.13
- local floor wait_readings: 61
- local floor read_errors: 0

## Changed-helper consumers

Every entry below passed in both the unit and integration-packages retained logs:

- TestInterruptIdleSendsNoCancelOrSignal: PASS
- TestInterruptSettlesBySessionCancel: PASS
- TestInterruptSessionCancelCarriesSessionID: PASS
- TestInterruptAnswersPermissionsBeforeSessionCancel: PASS
- TestInterruptCancelNeverFollowsTheNextPrompt: PASS
- TestInterruptSendsNoSIGINTAfterTheTurnEnds: PASS
- TestInterruptEscalatesToSIGINTWhenCancelIgnored: PASS
- TestInterruptReturnsNotSettledAndLeavesAgentRunning: PASS
- TestInterruptAgentExitCountsAsSettled: PASS
- TestInterruptKeepsAgentThatExitsOnSIGINT: PASS
- TestInterruptCrossProcessRunsCancelInOwner: PASS
- TestInterruptTurnAgainstTheAgent: PASS

In particular, repeated SIGINT logging, exit status 130, ignored signals and
cross-process cancellation remain covered. The reviewer independently exercised
the deterministic delayed-read reproduction; those reviewer measurements are
context, not a substitute for this gate's fresh complete suite.

macOS execution is outside this Linux gate; the reviewer recorded the inherited
ignored-SIGINT portability observation as non-blocking. The Darwin floor check
is compilation evidence for fsys, not execution of the ACP fake on macOS.

The fix's landing guard ga-ye3mxi3 and tracker ga-qxw4vmw remain open until actual
main landing; a gate PASS or deploy-bead close is not landing. Merge belongs to
mayor/MPR. No merge is performed by the deployer.
