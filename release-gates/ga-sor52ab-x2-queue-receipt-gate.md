**Verdict:** **PASS**

# X2 lane-host queued-receipt release gate

Evaluated 2026-10-22 for deploy **ga-sor52ab**, build **ga-nww0kvt**, review **ga-bpcbddd**.

- Reviewed source: `e49f041ccd6cf8fb814d684900ba211bf42e31d1`.
- Isolated deploy branch: `deploy/ga-sor52ab-gate`.
- `deploy_mode: remote`; `base_ref: origin/main`.
- `gate_base: 13ba4e0592fb6cd98d3adeeb51c05ac268871fb7`.
- `merge_state: clean (origin/main@13ba4e0592fb)`.
- `gate_base_kind: current-tip`.
- `merge_validation: merge result built and vetted here`.
- Scratch merge: `d10948af105f5a352d31815409c77fe28e21a472`.
- Tested merge tree: `3378c3a7b1b2112e8e12de97f524b3b0f24b5263`.
- Evidence root: `/var/tmp/gc-gate.ga-sor52ab.ii4ze1` (retained).
- No source PR existed at intake or the final pre-publication lookup.
- `docs/PROJECT_MANIFEST.md` is absent; commands were resolved from `TESTING.md`, the Makefile, CI and the deployed release-gate protocol.

Criterion 6 was evaluated first, then review/acceptance, the fast lane, and the complete runtime chain.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-bpcbddd records PASS on the exact resolved source above. No carryover or reviewer-added tests. |
| 2 | Acceptance criteria met | PASS | The diff matches the requested busy hold/release around the at-least-once queue nudge when a controller is up. The assertion and other guarantees remain intact. Independent RED/GREEN and the canonical whole target are detailed below; `make check` passed all 287 targets. |
| 3 | Tests and required lanes | PASS | Full five-phase remote coverage plus all 16 native-floor steps completed. One failure is attributed to ga-od4rcxy under the fix-carrying rule; raw failure records remain unchanged. No waiver or failed-test retry. |
| 4 | No open HIGH review findings | PASS | Review reports no blocking style, security or spec findings. The unrelated integration-tagged harness lint observation is informational and not a required-lane failure. |
| 5 | Final branch clean | PASS | Source checkout was clean after preserving the nine pre-existing untracked gate records outside the checkout. This checklist is the only deployer addition; publication checks clean status after its commit. |
| 6 | Merge recorded; build and vet | PASS | The scratch tree equals the mechanical merge of the frozen base and reviewed source. `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...` passed 1,385 targets, including nogo lint/vet, in 92.090s. |
| 7 | Single feature theme | PASS | Source changes only `test/conversationlane/x2_test.go`, +12/-0, in helper `runX2Host`. Full ancestry guard accepts this deploy and its build bead, with no stack or forbidden paths. |

## Fast lane

`fast/units/gate.w1.{stdout,stderr}.log` records `CRITERION6 PASS`, `FORMAT PASS`, `POLICY PASS`, `DRIFT PASS` and `FAST_LANE PASS` before the runtime suite started.

- Format: `fresh-tree-run.sh -- gate-base.sh run -- run-pinned-lint.sh -- make fmt-check` passed.
- Formatter: `GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}`.
- `policy_lane: make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback' -> PASS` through `gate-base.sh run` in its own fresh view.
- `drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code -> PASS`, reproducing CI's BUILD drift check in another fresh view.
- Format view `/var/tmp/gc-fresh-tree.IppBXO`; policy view `/var/tmp/gc-fresh-tree.YFF2I7`; drift view `/var/tmp/gc-fresh-tree.0QmtfA`.
- Each `FRESH_TREE_RECORD` reports the same merge/tree, `modified=0 untracked=0 ignored=5`, excluding only generated Bazel links and `.bazelrc.local` from the view.
- Both `GATE_BASE_RECORD` lines pin `13ba4e0592fb6cd98d3adeeb51c05ac268871fb7`, with knobs `CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE`.
- ICU check: `GATE_ICU74 verdict=OK provenance=recorded`, all three ICU74 library digests match the host record. No library-path or namespace overlay.
- `make check-hooks` confirms `.githooks` owns the active hooks. The temporary scratch commit omitted hooks after canceling only a queued build; explicit checks above replaced that setup attempt.

## Runtime coverage and evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `ci_lane_run: n/a (no CI-config change)`; `waiver_ref: none`.

`test_cmd`: `gate-criterion3.sh` launched through `gate-detached-run.sh`, remote admission and `isolated-test-run.sh --rbe`. Its five standard phases are:

| Phase | Command/coverage | Result | Fresh remote result records |
|---|---|---|---:|
| Unit | `make check` (`bazel test //...` plus the four Makefile guards) | 287/287 targets PASS | 373 |
| Acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 7/7 targets PASS | 24 |
| Integration packages | `bazel test --config=integration //test:integration_packages` | 31/31 targets PASS | 80 |
| Full integration | `bazel test --config=integration --test_arg=-test.v --test_arg=-test.skip='^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$' //test/integration:integration_test` | One attributed FAIL in 14 shards | 14 |
| Integration latency | Same whole integration target, standard two-name latency filter, disabled sharding and worker `gate-farm-probe.sh` | Both latency tests PASS | 1 |

Every test invocation also carries `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<phase>.bep.json`. The exact phase commands and timestamps remain in `c3/evidence/remote/*.stdout`.

`test_exec_mode: remote-exec` is derived from the five BEPs: **492/492** results have remote strategy, first attempt, a start in the phase window and a terminal verdict; cached remotely=0, cached locally=0, other strategy=0, retried=0. Build/cache hits in the fast lane are not runtime test credit.

- `test_exec_bep`: `c3/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`, with sibling `.evidence.json` reports.
- `rbe_state`: generated definitions present; kill switch absent; certificate expires 2027-01-09T20:13:22Z; tree and base worker pin both `sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45`.
- `fallback_reason: none`; no farm/local suite stitching.
- `admission`: remote cap=2, floor=25 GiB; initial free=55.917 GiB and continuation free=56.638 GiB, each waited 0.001s.
- `farm_test_seconds: 10170.995` across the five phases.
- `test_log_dir: /var/tmp/gc-gate.ga-sor52ab.ii4ze1/c3/evidence/remote/logs`.
- `test_counts: PASS=124664 FAIL=1 SKIP=370` across 508 logs. These are top-level result lines per job, not distinct tests or subtests.
- `diff_tests_executed: none (no diff-owned tests)`. Official `diff-owned-tests.py --explain` identifies two shared hunks in helper `runX2Host`, with no changed test body.
- `owned_topup: []`; no diff-owned tests needed a repair run.
- `floor_topup: complete=true`, 16/16 exit 0: all environment-absence selections by package, pinned-beads module verification, Darwin compile, push-gate-lock self-test and local-concurrency self-test.
- `skip_inventory`: 356 remote records: 71 environment-absence, 285 other. Every environment-absence selection was included in the fixed native floor. Residual native skips include optional replay/simulation inputs, live herdr opt-ins, absent `br`, binding tests needing an existing tmux server and deliberate classified-provider/subtest skips. All are outside the modified helper. Full reasons remain in `skip-inventory.json` and `floor-logs`; no SKIP is silently credited as PASS.
- The only conversation-lane skip is `TestX6FanIn/full`, its documented opt-in 5,000-notice manual measurement. Official `--site x6_test.go:192` reports UNCHANGED, no shared hunks in that file; the changed helper has only the X2 caller and cannot reach this skip.
- `latency_leg: complete=true verdict=measured`: both `TestGcBdLatencyBudget` and `TestGcSeatHookLatencyBudget` PASS. Probe clear immediately, load1=7.12, cpus=32, threshold=50; no deferral or retry.
- Native load: `load_threshold=15 load_waited_seconds=90 load_wait_timed_out=0 run_start_load=14.34 run_max_load=18.84 run_mean_load=17.28 run_readings=8`. Wait first=15.07, max=15.57, mean=15.03, readings=4, read_errors=0.
- `final-evidence-audit.json`: all 984 retained raw artifacts match SHA-256; all 492 readable logs match the documented framing normalization. Evidence and logs are retained.

## Attributed failure: ga-od4rcxy

`failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy | clause 3(a): MECHANISM`.

The full integration read sweep receives HTTP422, expected200, at `test/integration/gc_live_contract_test.go:1509`: required query parameter `assignee` is missing. Exact log: `c3/evidence/remote/logs/integration-full-65412ec6e874f54aa912.log`. This is the existing fixture condition, not the X2 receipt assertion.

1. **Not owned:** the official failure-site check is UNCHANGED, `shared_hunks_in_file=0`. No hunk touches the failing test or its fixture.
2. **Tracked:** ga-od4rcxy was opened and read; it predates this run (2026-10-19) and names this exact route, line, status and validation error. This sighting was appended to that tracker, without creating or routing another tracker.
3. **Mechanism:** the diff changes only a helper in a separate `_test.go` binary. The failing binary's computed 65-directory import closure contains no changed non-test input. It cannot compile or execute `conversationlane_test.runX2Host`; the real gc binary also excludes that test-only code. The failing fixture is blob-identical to the frozen base. No census, new target or new test-file load was added.
4. **No overlap:** failing package `test/integration` is distinct from changed package `test/conversationlane`.

The repeated-signature **fix-carrying exception** is verified: this deploy's `build_bead=ga-nww0kvt` carries `gc.fixes_tracker=ga-w912t1y`. The blocking condition's designated fix ga-ze0aym0 carries `gc.fixes_tracker=ga-od4rcxy`, is reviewed but closed with outcome `blocked`, and has not landed. Its resolved commit `f39fa5525a3a947a8e5f28548e3087cf73fcf2e1` is not an ancestor of the frozen main, whose fixture still lacks the required-query case. No waiver is used.

The original `c3/evidence/execution.json` remains **exit_code=1, criterion3_valid=false**. It was not rewritten or retried into green. After attribution, detached `continuation` invoked the existing runner's `latency_leg` and `floor_topup` functions on the same unchanged merge tree, completing the remaining coverage. `attributed-completion.json` separately records `complete=true`, all required continuations exit0 and the original failure. The failed candidate test was never rerun.

## Independent acceptance checks

- Canonical whole conversation-lane target PASS: all six affected `TestX2LaneHostKillMatrix/{sqlite,dolt}/{take,effect,done}/at_least_once` cells were verified by name in `integration-packages-8ecc4094e08f9bfd6d51.log`. Other host/CLI crash cells and expected outcomes remain intact.
- Scratch-only read-back-delay amplifier, 20s, five runs of each affected cell: **RED** without the fix gives 5/5 `done` FAIL on each store, all at the original queued-output assertion (`Nudged worker`), while all 20 `take/effect` cells PASS. **GREEN** with the reviewed fix gives 30 PASS, 0 FAIL, 0 SKIP.
- `red.log` and `green.log` retain the focused supplemental evidence. A parser initially missed Bazel's framing character; the saved RED output was parsed correctly without rerunning RED. The GREEN service completed successfully. Neither amplifier nor baseline replacement was committed or applied to the canonical suite tree.
- Nine pre-existing untracked gate records were copied with digest verification to `preexisting-gates/` under the evidence root before removing their checkout copies. They were not discarded or included in this deploy.

The deployer does not merge. Publication and clearance bind to the exact isolated branch head containing this record; MPR owns the merge.
