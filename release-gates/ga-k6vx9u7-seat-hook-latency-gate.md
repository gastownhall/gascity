# Seat-hook bead-read concurrency release gate

**Verdict:** **PASS**

Evaluated 2026-10-25T01:02:32+00:00; deploy bead `ga-k6vx9u7`; build `ga-yu01v6x`; review `ga-zfsb75p`.

- deploy_mode: remote; base_ref: origin/main.
- reviewed_commit: `940a4d4fd10e1aa6797fda7fb4e5cbbc11a64dfd` (resolved as a commit; branch prose was not used as a source).
- base_commit: `f57926a5fa838c22eeabf2869e8f11b5a769ddb5` (frozen before the run).
- gated_merge_commit: `00b6aea9325339b2a39ee90a7e995de338d54660`; gated_tree: `50dcef5cd1f1a199fbd6545ac7c211724cf9faaf`.
- gate_base_kind: current-tip.
- merge_state: clean (origin/main@f57926a5fa838c22eeabf2869e8f11b5a769ddb5).
- merge_validation: the clean scratch merge was built and tested; MPR owns current-head merge validation and conflict resolution at merge time.
- evidence_root: `/var/tmp/deploy-ga-k6vx9u7-fresh.crlnxjmh` (retained host artifacts; all paths below are relative to this root).
- Manifest: `docs/PROJECT_MANIFEST.md` is absent; `TESTING.md`, `Makefile`, and `.github/workflows/bazel.yml` supply the canonical commands.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Live review ga-zfsb75p is closed with PASS on the exact resolved source. No review carryover, reviewer-added test commit, or waiver. |
| 2 | PASS | Both causal overlap tests fail with only the two production changes reverted to the frozen main, and pass on the unmodified candidate. Both tier paths use the new parallel reader; merge order and partial-error implementation are unchanged. SessionStart mail overlaps the pending step lookup while the reminder still renders first. The direct pinned-bd fixture is opt-in; other fixtures and shared shim are unchanged. Full-target, policy, build and latency evidence is detailed below. |
| 3 | PASS | Canonical complete five-phase gate: 497 fresh remote results, 994 digest-verified raw log/XML artifacts; native floor 16/16 successful steps. Official by-name reader: 126374 PASS, 0 FAIL, 370 SKIP over 513 logs; all 18 diff-owned roots and 26 extra required roots PASS. No failed-result retry, attribution, waiver, or cache-credited result. |
| 4 | PASS | Review reports zero unresolved blocker/major/minor findings. The four pre-existing off-test-goroutine Fatal calls are a non-blocking style nit; the narrow latency margin is an observation. |
| 5 | PASS | Source and canonical scratch were clean; final branch adds only this gate record to the reviewed source, with normal pre-commit hooks and post-commit clean-index verification required before push. |
| 6 | PASS | Criterion 6 ran first: clean materialized merge, full Bazel build/nogo, integration target compile, vet and active-hook check all pass (`build/units/gate.w1.stdout.log`, C6_PASS). No rebase or conflict repair. |
| 7 | PASS | One feature: shorten the seat-hook bead-read critical path. Other changed tests only protect recording fakes for concurrent calls and order-independent assertions. No independent feature, internal-agent configuration, or unrelated documentation is included. |

## Acceptance and negative controls

`red-proof-verified.json` verifies the original focused negative-control log (SHA-256 retained): `TestBdStoreTwoTierListReadsBothTiersAtOnce` and `TestPrimeHookReadsItsSessionStartMailWhileTheStepReminderIsOutstanding` both FAIL with the production rollback, with the expected serial-tier/late-mail diagnostics. The first parser missed Go test2json record prefixes; that original false parser result remains retained. The correction only re-read the original log; it ran no test again. This counterfactual is not a candidate failure or full-suite evidence.

`control-proof.json` records both roots PASS on the separate unmodified acceptance control; both also PASS within the canonical suite. `acceptance-static-proof.json` and the reviewed diff confirm unchanged `mergeListTierResults`, no budget/statistic/load/admission change, default directBD=false, and unchanged shared filebdshim/tool-bin setup.

The build's historical wording asked for two integration-full latency measurements. That lane now excludes the registry latency tests. Mayor measurement ruling `gm-wisp-qmtfty3` (independently read; retained in `latency-measurement-ruling.txt`) makes only the clear-probe integration-latency leg decisive, without a waiver or budget change. The reviewer recorded eight clear-probe PASS measurements at this source; this independent complete gate adds one fresh decisive PASS. The obsolete literal lane wording is not claimed as executed.

## Canonical criterion 3

- test_cmd_scope: full-suite.
- test_cmd: `gate-criterion3.sh` detached through `gate-detached-run.sh`, normal remote admission, `load-gate-run.sh --class remote`, and `isolated-test-run.sh --rbe`; exact argv retained in `c3/cmd.argv` and `c3/evidence/execution.json`.
- Unit command: `make check` (Bazel `//...`, nogo, format, generated artifacts, policy and shell guards).
- Acceptance: Bazel acceptance config, `//test/acceptance:acceptance_test` plus `//test/acceptance:acceptance_solo_tests`.
- Integration packages: Bazel integration config, `//test:integration_packages`; includes the real `TestTutorial01` process scenarios, PASS by name.
- Full integration: Bazel integration config, `//test/integration:integration_test`, excluding exactly the registry's two latency-budget tests.
- Latency: the same integration target, only the registry's two tests, unsharded, with its on-worker clear-load probe.
- Every farm phase uses `--config=ci --config=remote-exec --nocache_test_results --noremote_local_fallback`, with verbose named results and retained BEP.
- test_exec_mode: remote-exec, derived from BEP. Native floor evidence is supplemental and does not stitch two partial full suites.
- test_exec_bep: every result uses remote strategy, neither cached flag, attempt 1, a start within its own phase window, and a terminal verdict. Zero local, cached, retried, or out-of-window results.
- rbe_state: marker present, kill switch absent; certificate expires 2027-01-09T20:13:22Z; pinned worker image `sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45` matches the base.
- fallback_reason: none; admission: remote, 25 GiB floor, cap 2, 74.31660461425781 GiB free, 0.001 s admission wait; no remote host-heavy slot or host-load wait.
- farm_test_seconds: 10667.132.
- heavy_mode: none (classifier verified); heavy_units/coverage_check: not applicable.
- test_log_dir: `/var/tmp/deploy-ga-k6vx9u7-fresh.crlnxjmh/c3/evidence/remote/logs`; raw BEP/log/XML and native logs are retained.
- owned_topup: empty; no owned root missing or needing another run.
- floor_topup: complete, 16 successful steps, including every environment-absence skip's package/parent, beadstest cache resolution, Darwin compile, push-gate lock self-tests (55 PASS), and concurrency self-tests (25 PASS).
- failure_attribution: none; policy_attribution: none; waiver_ref: none (Gas City has no waiver path).
- ci_lane_run: n/a; no CI job, matrix, timeout, or required-check change.

| Phase | Fresh remote results | Raw artifacts verified | Result |
|---|---:|---:|---|
| unit | 373 | 746 | PASS |
| acceptance | 24 | 48 | PASS |
| integration-packages | 85 | 170 | PASS |
| integration-full | 14 | 28 | PASS |
| integration-latency | 1 | 2 | PASS |

`final-audit.json` independently verifies all reports, 994 raw artifact digests and the normalized-reader copies (only Go test2json U+0016 prefixes are stripped), source SHA, clean scratch tree, RED/GREEN log digests, and complete native records. `final-owned-by-name.txt` is the official reader's 18-test result; `c3/evidence/remote/by-name.txt` also verifies the additional 26 required roots.

## Latency and local floor load

- latency_leg: complete/measured on attempt 1, farm probe clear, load1=0.24/load5=0.11/load15=0.05 on 32 CPUs; no busy deferral or test retry.
- Seat-hook fastest-of-seven: prime 913 ms, drain 688 ms, mail check 980 ms, all below the unchanged 1000 ms budget. The mail-check margin is 20 ms; this is an observation, not an added budget or waiver.
- Both `TestGcBdLatencyBudget` and `TestGcSeatHookLatencyBudget` PASS by name in the decisive lane.
- Native floor: load_threshold=15; load_waited_seconds=1814; load_wait_timed_out=1; run_start_load=36.45; run_max_load=37.34; run_mean_load=36.36; run_readings=9; wait_first_load=123.72; wait_max_load=129.53; wait_mean_load=94.35; wait_readings=61; read_errors=0.
- This is the documented soft wait for an ordinary non-heavy floor. Its timeout is recorded, not waived. The systemd restart check PASSes (5.6 s restart against its unchanged 15 s budget).
- ICU74: recorded loader provenance OK for all three required sonames (`icu74.log`). Rootless podman socket and pinned image were verified before suite launch; fleet store selectors were scrubbed before the PASS-THROUGH isolation wrapper. No library overlay or action-key library override was used.

## Skip inventory and scope

`skip-audit.json` retains 395 farm/native SKIP events including subtests, original logs and printed reasons. The official reader's 370 count is its top-level per-job count, including deliberate tier duplicates. Farm inventory classifies 285 other and 71 environment-absence events before the floor; all environment-absence roots were submitted to the native floor. No diff-owned root is skipped in the complete evidence.

Remaining non-owned skips are existing platform/privilege restrictions, missing optional credentials/tools/live fixtures, deliberately unsupported provider capabilities, test harness/negative controls, opt-in persistence/replay/registry scenarios, and explicit tracked placeholders. Their skip guards are unchanged. The ordinary unit lane's process skips have full integration-package PASS coverage where enabled. Native-only remaining skips (including absent replay data, live-herdr gates, tmux keybindings outside tmux, optional Postgres DSN and br, and intentional runtime negative controls) retain their actual reasons and are not reported as PASS.

The one remaining skipped test in a changed file is `TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses`: the official site tool reports UNCHANGED for `integration_test.go:2828`. It skips immediately on Linux `/proc` availability, before any changed fixture constructor. Its file's shared hunks only add the direct-BD opt-in wrapper and preserve existing calls with directBD=false; neither reaches this guard. Other skipped test/guard files are untouched. The interleaved crash-injection skip reason was recovered from its original raw line: `ga-vnycm2.27: interrupted abort-scope converges with different skip metadata`; no parser silence is accepted as an explanation.

## Fast lane

- policy_lane: PASS before runtime/suite launch, completed 2026-10-25T00:01:36Z. Pinned golangci-lint 2.12.0 (mismatch=false) with integration tags on changed packages; full nogo/vet, fmt-check, active hooks, four shell guards and ten CI policy targets PASS.
- drift_lane: PASS; independent fresh view ran `make bazel-sync`, then empty tracked/untracked status and `git diff --exit-code`.
- generated_lane: PASS; seven generated spec/schema/client/dashboard/lockfile checks in a third independent fresh view.
- Each view received the frozen gate base through `gate-base.sh run`; none wrote the canonical suite scratch. Raw commands and results are retained in `fast/units/gate.w1.stdout.log` and its stderr log.

## Diff-owned tests

- `cmd/gc`: `TestCmdSlingInlineBeadBareTargetFromRigCwdBdProvider` — PASS.
- `cmd/gc`: `TestCmdSlingInlineBeadRigScopedBdProvider` — PASS.
- `cmd/gc`: `TestPrimeHookReadsItsSessionStartMailWhileTheStepReminderIsOutstanding` — PASS.
- `internal/beads`: `TestBdStoreListBothTiersAppliesCreatedBeforeBeforeMergedLimit` — PASS.
- `internal/beads`: `TestBdStoreListBothTiersMessageUnionsEphemeralQueryWithoutTierFiltering` — PASS.
- `internal/beads`: `TestBdStoreListBothTiersReturnsWholeReadError` — PASS.
- `internal/beads`: `TestBdStoreListBothTiersUnionsBdListAndEphemeralQuery` — PASS.
- `internal/beads`: `TestBdStoreListBothTiersUnionsEphemeralQueryConformance` — PASS.
- `internal/beads`: `TestBdStoreListStorageTierConformance` — PASS.
- `internal/beads`: `TestBdStoreListWispAwareTiersTolerateAdaptersWithoutBdQuery` — PASS.
- `internal/beads`: `TestBdStoreListWispsAppliesMetadataBeforeClientLimit` — PASS.
- `internal/beads`: `TestBdStoreListWispsAssigneesMultipleFallsBackToClientFilter` — PASS.
- `internal/beads`: `TestBdStoreListWispsAssigneesSingleUsesAssigneeClause` — PASS.
- `internal/beads`: `TestBdStoreListWispsQuotesNonBareQueryValues` — PASS.
- `internal/beads`: `TestBdStoreListWispsRequestsUnlimitedResultsByDefault` — PASS.
- `internal/beads`: `TestBdStoreListWispsUsesBdListWithClientTierFilter` — PASS.
- `internal/beads`: `TestBdStoreTwoTierListReadsBothTiersAtOnce` — PASS.
- `test/integration`: `TestGcSeatHookLatencyBudget` — PASS.

The publication branch is cut mechanically from the reviewed source and adds this record only. The tested scratch merge is never published. The final PR head must equal the local committed head before deploy clearance is posted; MPR remains the merge authority.
