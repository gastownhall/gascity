# Deterministic unhealable-cache parity assertions

**Verdict:** **PASS**

Date: 2026-10-20. Deploy bead: ga-jy6rqcr. Build: ga-rasc3zz. Review: ga-w3k5gea.

## Source and merge record

- Reviewed source: `53eb662fd938ad2fd7ac5e0a8d449663c41ef326`, resolved against the repository before use.
- Isolated deploy branch: `deploy/ga-jy6rqcr-gate`, cut at exactly that source; the builder branch is provenance only.
- Gated merge: `e4afdf804fee176267662abe5326f49e30a1da59`.
- Pinned base: `ab37e19f07e83d375fb53bdf7db5acdacc798839`.
- merge_state: clean (origin/main@ab37e19f07e83d375fb53bdf7db5acdacc798839).
- gate_base_kind: current-tip.
- merge_validation: the clean merge result was tested; MPR owns validation against the base at merge time. No deployer rebase, reset, force-push, or merge.
- Ancestry scope: two reviewed commits, both citing ga-rasc3zz, touching only `cmd/gc/bd_passthrough_parity_extra_test.go`; no unrelated theme or denied path.

## Numbered criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-w3k5gea records verdict `pass` on the exact reviewed source; no carryover or reviewer-added commit is required. |
| 2 | Acceptance criteria met | PASS | Pre-merge acceptance is verified: the comparison removes only the reported cache-entry leaf, preserves cache identity, error class, exit status, stdout and legacy/trimmed routing parity, and retains the other cases. All three owned tests pass in both suite builds and 20 consecutive local sandbox repetitions. The explicitly deferred tracker-close condition is recorded below, not claimed complete. |
| 3 | Tests pass | PASS | All four full-scope Bazel phases completed. Raw counts are 122342 PASS / 3 FAIL / 347 SKIP. Each failure meets criterion 3a, with exact existing trackers and independence proofs below; zero unaccounted failures. Supplementary local floor: 2836 PASS / 0 FAIL / 52 SKIP, plus the native pinned-module test PASS. No owned FAIL, SKIP or MISSING. |
| 4 | No high-severity review findings open | PASS | Reviewer security and style findings are none; unresolved HIGH count is zero. The three unchanged-test failures are separately tracked, not suppressed. |
| 5 | Final branch is clean | PASS | The reviewed tree is clean; the sole deploy addition is this gate record, committed before push. Generated-file checks left no drift. |
| 6 | Merge state recorded; build and vet pass | PASS | Fresh clean merge view built with `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...`: exit 0, 1376 targets, 227.330 seconds; includes nogo lint/vet. The original pinned base is retained even though main subsequently advanced. |
| 7 | Single feature theme | PASS | Test-only stabilization of unhealable-cache bd-passthrough parity; no production behavior, API, configuration or dependency change. |

## Full-suite execution

Canonical coverage comes from `TESTING.md` and `.github/workflows/bazel.yml`; `docs/PROJECT_MANIFEST.md` is absent. The full local run covers:

| Phase | Targets | Raw Bazel target result |
|---|---|---|
| Unit, nogo, format, generated and policy | `bazel test //...` | 285 PASS / 1 FAIL; 372 shard results, all attempt 1 |
| Acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 7 PASS; 24 shard results, all attempt 1 |
| Integration packages | `bazel test --config=integration //test:integration_packages` | 29 PASS / 1 FAIL; 79 shard results, all attempt 1 |
| Full integration, no smoke filter | `bazel test --config=integration //test/integration:integration_test` | 13 PASS / 1 FAIL among 14 shards, all attempt 1 |

The local commands force fresh test execution with `--nocache_test_results`, use `--config=fork-cache --remote_cache= --remote_executor= --config=ci --jobs=4`, retain verbose named test logs and per-phase BEP, and run through detached services, local admission, the load gate, the isolation wrapper and `gate-base.sh run`. The wrapper reports PASS-THROUGH for the merge scratch: this is a clean, dedicated merge view, and both local commands explicitly scrub live city/store selectors and pin Git configuration and the pane shell. No ambient-city isolation is claimed. Rootless Podman and the pinned bd 1.3.1 are configured before testing; Ryuk is disabled under the host reaper policy.

- test_cmd_scope: full-suite. The local floor and 20-repeat check are supplemental focused evidence, not substitutes for these phases.
- test_exec_mode: local, derived from all 489 full-suite BEP test results using `linux-sandbox`; zero cached results and zero retry attempts.
- fallback_reason: the original remote run ended REMOTE_ERROR 34 (UNAVAILABLE/NOT_FOUND), without a complete suite. A complete local restart was selected explicitly; its runner records `rollout-disabled`. The abandoned remote run and first stopped local setup contribute no credited result.
- farm_test_seconds: 0 credited to this local gate; abandoned farm evidence is retained separately.
- rbe_state: local marker present, kill switch absent, certificate valid through January 9, 2027, and pinned worker-environment digests match. Transport failure, not a test assertion, caused the remote abandonment.
- heavy_mode: none. Classification is retained with the original runner evidence.
- waiver_ref: none; no gascity waiver is used.
- ci_lane_run: n/a, no CI-configuration change.

## Diff-owned tests and local floor

`diff-owned-tests.py --explain` identifies exactly these three tests. Each reports PASS in the full unit and integration-package logs, and PASS 20/20 under the local Bazel sandbox with `-test.count=20`:

- `TestBdPassthroughConfigReportsAReadinessPassThatCannotHeal`.
- `TestGcBdPassthroughTrimsKeepRoutingMore`, including `fallback_cannot_heal`.
- `TestUnlinkedCacheEntryIsNotPartOfTheErrorIdentity`, including unequal cache-directory and unequal error-class checks.

owned_topup: no owned test was missing; the 20-repeat check additionally verifies acceptance under the local sandbox. floor_topup: complete, 13 steps exit 0, including package/parent replays of every environment-absence SKIP, the repeated owned tests, and `TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache`. The last is the mandatory native floor test explicitly excluded from `internal/beads/beadstest/BUILD.bazel`; native Go does not replace any Bazel suite phase. Darwin compile and both runner self-tests also pass.

skip_inventory: 347 full-suite top-level SKIPs, classified as 64 environment-absence and 283 other; exact names, sites, reasons and logs are retained in `criterion3-local-floor/evidence/skip-inventory.json`. Other skips are unchanged capability/conformance exclusions, opt-in fixture/measurement checks and documented replacement proofs. Environment-absence parents were replayed across 11 packages; non-owned remaining SKIPs are inventoried (52 supplemental top-level SKIPs), not called PASS. There are zero skipped tests from the changed parity file and no changed production input that can affect any skip site.

## Criterion 3a: retained failures, not passing retries

All three failures are outside the changed test file and its package. Each tracker was opened, predates this run, names the exact test and observed signature, and received a read-back-verified sighting comment. These are the first occurrences in this bead's gate, not repeat-to-green attempts.

| Failing test | Existing tracker | Signature and independence proof |
|---|---|---|
| `TestWriteTokenFileKeepsGroup` | ga-ow7y5vo, created October 15 | `write_test.go:383`, chown returns EINVAL. Clause 1: tokenrender test untouched. Clause 3(a): a cmd/gc test-only diff cannot enter the tokenrender test binary. Clause 4: different package, no overlap. |
| `TestProvider_JoinedForeignAgentPaneIsNotTheAgent` | ga-rkis7u4, created October 13 | `adapter_test.go:752`, first pane `%1`, expected joined pane `%2`; host tmux 3.7c. Clause 1: tmux tests untouched. Clause 3(a): no changed production input can enter that binary. Clause 4: different package, no overlap. |
| `TestGCLiveContract_BeadsAndEvents` | ga-zt0ulg3, created October 13 | `gc_live_contract_test.go:497`, suspend HTTP 200 followed by state `asleep`, expected `suspended`. Clause 1: integration test untouched. Clause 3(a): neither the integration binary nor its launched gc executable changes; this diff only changes cmd/gc test assertions. Clause 4: no path overlap. |

The original unit and tail services therefore remain raw FAILED. Their three failing logs are preserved in the combined local evidence; criterion 3 PASS uses the documented attribution rule, not a rewritten runner verdict or a retry.

## Fast lane and load evidence

- policy_lane: PASS. Fresh views ran `make lint`, 20 Python workflow-policy checks, `//:go_format_test`, `//scripts/cipolicy:cipolicy_test`, `//scripts/prwatchdog/...`, `//scripts:scripts_test`, and the release-dist/routed-row/split-topology/residency shell guards. One initial remote NOT_FOUND/NO STATUS interruption was completed by an unfinished-check retry; no failed test was retried.
- drift_lane: PASS. Separate fresh view ran `make bazel-sync` followed by `git diff --exit-code`; no generated drift.
- The fast lane completed before full-suite execution. `make check` coverage is represented by the full Bazel unit phase plus its shell guards, with the raw failures explicitly attributed above.
- Hook ownership: `make check-hooks` confirms `.githooks` is active; the gate-record commit runs its pre-commit hook.
- admission: local-heavy cap 1, free-disk floor 40 GiB. Unit admission 84.663 GiB free; floor admission 83.312 GiB free.

| Local run | load_threshold | load_waited_seconds | load_wait_timed_out | run_start_load | run_max_load | run_mean_load | run_readings |
|---|---|---|---|---|---|---|---|
| Full unit | 15 | 0 | 0 | 9.15 | 25.65 | 20.72 | 54 |
| Remaining three phases and shell/Darwin/runner checks | 15 | 0 | 0 | 12.78 | 67.01 | 26.68 | 63 |
| Supplemental floor and 20-repeat check | 15 | 1801 | 1 | 34.39 | 34.06 | 30.55 | 34 |

The last bounded, ordinary soft wait timed out and then proceeded as prescribed; it is not a waiver or a heavy-path exception. Values are recorded exactly as emitted by the load-gate tool.

## Retained evidence and deferred landing condition

Evidence root: `/var/tmp/deploy-ga-jy6rqcr.Cnli5bWl`. Build, policy, policy-remainder and drift logs are retained at its top level. Full local unit logs/BEP are under `criterion3-local-2/evidence/local`; the remaining three phases, combined named evidence and original-log manifest are under `criterion3-local-tail/evidence`. Supplemental floor logs, BEP, inventory and its completed JSON record are under `criterion3-local-floor/evidence`. The three detached services have finished; no test log is deleted.

test_log_dir: `criterion3-local-tail/evidence/combined-logs` plus `criterion3-local-floor/evidence/logs`, relative to the evidence root. test_exec_bep: unit in `criterion3-local-2/evidence/local/unit.bep.json`; acceptance, integration-packages and integration-full in `criterion3-local-tail/evidence`.

**Deferred exit-contract item 4 is NOT complete before merge:** ga-y7qavme remains open until the fix actually lands on main. The merge-request must name this requirement. After confirmed landing, the mayor/deployer must verify or perform tracker closure. PR creation and merge-request routing do not assert shipment; the deploy work record is `blocked` until the merge authority lands the change. Only MPR/operator/mayor has merge authority; the deployer does not merge.
