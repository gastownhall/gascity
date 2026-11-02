# Release gate: hour-format Claude busy timers

**Verdict:** **PASS**

- Deploy bead: ga-0n79f26; build bead: ga-b42fdg; review bead: ga-1r20er6.
- Evaluated: 2026-10-13T01:43:04.939672+00:00.
- DEPLOY_MODE: remote; BASE_REF: origin/main.
- Reviewed source: `4b6afbac49d219891e23b9388d66c07079ead559` (resolved as a commit; builder branch is provenance only).
- gate_base: `1228f064520fd9f40164941b0cbfdbcd6bca636e`.
- Merge validation: `fe5efcde8b76f38c5a71fee0df08938828c1a4e5`; tree `c846f2e7409b5cd358a817a6ac197638df1d27f8`.
- Preflight: the reviewed commit has no associated PR; no already-merged reconciliation needed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live review ga-1r20er6 records verdict pass at the resolved reviewed source. |
| 2 | Acceptance criteria met | PASS | Separator-anchored busy timer accepts h/m/s; hour and unfamiliar-glyph fixtures read busy; done markers and other idle chrome read idle. Both added tmux tests and both modified manifest tests pass by name. The existing integration table also runs in the full integration package tier. Part 1 only: the existing bare-gerund scan handles glyph-and-verb bare frames; no day unit added. |
| 3 | CI-equivalent tests pass | PASS | Fresh full CI-equivalent chain on the materialized merge: unit, acceptance including solo tests, all integration packages with GC_FAST_UNIT=0, and integration-smoke. All 288 targets pass; Go terminal result counts 101586 PASS / 0 FAIL / 306 SKIP. All four diff-owned tests pass, with no failing or unexecuted subtest. |
| 4 | No unresolved HIGH review finding | PASS | Review security findings: none; style finding LOW only (timer-row comment accuracy), carried as advisory. |
| 5 | Clean final branch | PASS | Clean reviewed-source checkout verified before adding this gate artifact; only this gate record is committed by deployer. Pre-commit runs with active .githooks; clean status is verified again after the commit. |
| 6 | Merge validated against main | PASS | Clean materialized merge against the pinned gate base; full Bazel build and make vet/nogo passed. The normal synthetic-merge pre-commit hook ran. Base is the scratch merge's first parent and stays pinned for all later lanes. |
| 7 | Single feature theme | PASS | Three source commits concern hour-format tmux busy detection, its fixtures, and the test census; no independent feature or internal-doc contamination. |

## Test command and scope

`test_cmd_scope: full-suite`

The canonical commands reproduce the required lanes in .github/workflows/bazel.yml and TESTING.md:

```text
make check BAZEL_FLAGS="--keep_going --config=ci --config=remote-exec --nocache_test_results --jobs=16 --remote_download_outputs=toplevel --rewind_lost_inputs --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<unit BEP>"
bazel test <same flags> --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel test <same flags> --config=integration //test:integration_packages
bazel test <same flags> --config=integration-smoke //test/integration:integration_test
```

The integration-packages tier supplies GC_FAST_UNIT=0, covering TestTutorial01 and the real process scenarios the unit tier skips. The non-gating full test/integration nightly lane is separate from the required integration-smoke lane. No ad hoc package or test-name subset substitutes for a required lane. The repository has no docs/PROJECT_MANIFEST.md; commands were resolved from TESTING.md, Makefile and current CI.

| Lane | PASS targets | Retained shard/run results |
|---|---:|---:|
| unit | 251 | 335 |
| acceptance | 7 | 24 |
| integration-packages | 29 | 76 |
| integration-smoke | 1 | 14 |

- test_log_dir: `/var/tmp/gc-heavy-gate/runs/ga-0n79f26.c3/logs` (449 retained logs; original log/XML, BEP and digest-verified CAS mappings kept beside them).
- test_counts: 101586 PASS, 0 FAIL, 306 SKIP (column-0 Go terminal result lines; repeated tests count once per run, not distinct test names).
- failure_attribution: none (0 FAIL).
- waiver_ref: none (gascity has no waiver path).
- ci_lane_run: n/a (no CI-config change).
- heavy_mode: none (classify completed before the suite).

```text
diff_tests_executed:
  TestPaneContainsBusyIndicatorHourTimer PASS (integration-packages-5a078b24388ef3c79748.log, unit-ef2749222a71266756ae.log)
  TestSnapshotPaneIdleWithPrefixHourSpinner PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-4bcc997199c6a7426526.log)
  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-ee115c844954da53d34d.log)
  TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce PASS (unit-db0feb4ad70da6248282.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name

```

## Fast lane, before runtime setup and suite

- policy_lane: PASS — pinned formatter 2.12.0, shell guards, check-hooks, test-ci-policy, full nogo lint and 11 explicit policy Bazel targets.
- drift_lane: PASS — make bazel-sync then git diff --exit-code and empty full status; 6 generated-artifact targets; pinned-base OpenAPI breaking check (1 target).
- All 18 fast-lane target logs/XML retained with no artifact errors. Each lane ran in a distinct fresh-tree view of the same merge commit through gate-base.sh; none modified the suite's scratch.
- Canonical suite used isolated-test-run.sh. This repo takes its PASS-THROUGH mode; no live BD/BEADS/GC/DOLT selectors were inherited. Bazel constructs the worker test environment. Pinned host test bd v1.3.1 was verified by the wrapper; fleet bd was untouched.
- Rootless podman socket, cached 2.2.0 image and enabled testcontainer-sweep were checked as preparation; first-party tests have no testcontainers/image reference, so this is not a claim of container-backed coverage.

## Skip evidence

All 306 SKIP lines have source locations in unchanged files. The official diff-owned tool checked all 232 unique sites: UNCHANGED, shared_hunks_in_file=0, with no skipped file in the four-file change. Per-test names, source sites and reasons are retained in skip-evidence.json and skip-site-proofs.json under the run directory. The change only widens the busy-timer pattern and its fixtures/census; it cannot change the platform, opt-in live-tool, process-tier, capability or deliberately skipped harness predicates below. Unit process skips are exercised by the integration tier. Optional live providers, MCP/Postgres and platform-only probes do not bear on the tested hour-timer behavior.

Within tmux, TestPokeActivityRealTmux skips at activity_poke_integration_test.go:21 because GC_TMUX_INTEGRATION is opt-in, before creating the runtime. TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin skips at state_cache_test.go:926 because this is Linux; its process-argument guard is untouched. Both sites independently returned UNCHANGED with zero shared hunks.

| Skip reason (verbatim) | Result lines |
|---|---:|
| spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage | 29 |
| provider family "" has no invocation-usage extractor (out of scope) | 16 |
| launchd path only applies on darwin | 14 |
| herdr not installed | 14 |
| skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 | 14 |
| starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage | 13 |
| starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage | 9 |
| GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance | 8 |
| Start: provider environment unavailable | 7 |
| macOS-only /tmp <-> /private/tmp Claude project path alias | 6 |
| MCP mail script not found: file does not exist | 5 |
| not inside tmux — need server for bind-key | 4 |
| helper process only | 4 |
| skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived | 4 |
| bwrap not installed; cannot build a private-/tmp mount namespace | 4 |
| exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage | 4 |
| requires a real Dolt server; run make test-cmd-gc-process for full coverage | 4 |
| there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal | 3 |
| set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server) | 3 |
| harness process | 3 |
| spawns real processes; run via make test-cmd-gc-process | 3 |
| bd binary not on PATH | 3 |
| there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology | 2 |
| ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix | 2 |
| darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug | 2 |
| gate G1 times this host: set GC_V2_G1=1 | 2 |
| GC_V2_REPLAY_DIR is not set | 2 |
| Darwin-specific syntax guard | 2 |
| Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions) | 2 |
| backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs | 2 |
| gate G1b reads a store copy: set GC_V2_G1B_STORE | 2 |
| CloseIfMatch: conditional writes unsupported | 2 |
| UpdateIfMatch(status=closed): conditional writes unsupported | 2 |
| upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused | 2 |
| ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix | 2 |
| ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix | 2 |
| backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs | 2 |
| backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs | 2 |
| embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release | 2 |
| zsh not installed | 2 |
| set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary | 2 |
| crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory | 2 |
| unsupported-os test only applies outside darwin/linux | 2 |
| backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs | 2 |
| ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix | 2 |
| ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix | 2 |
| ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix | 2 |
| set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server) | 2 |
| awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): "1.50", <nil> | 2 |
| requires a managed dolt server; run make test-cmd-gc-process for full coverage | 2 |
| requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage | 2 |
| uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage | 2 |
| darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms | 2 |
| requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage | 2 |
| waits through stale session-key detection; run make test-cmd-gc-process for full coverage | 2 |
| bd not available | 2 |
| placeholder for ga-q0bf.1 | 1 |
| placeholder for ga-xxqx | 1 |
| placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions | 1 |
| set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test | 1 |
| placeholder for ga-6wrr.1 | 1 |
| placeholder for ga-r8iz.1 | 1 |
| pending ga-cwdaqv: supervised gc start does not validate config pre-registration | 1 |
| pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main | 1 |
| set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement | 1 |
| passwordless ssh to localhost unavailable | 1 |
| br not available | 1 |
| installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands | 1 |
| bd 1.0.4 trips the silent-fallback guard (pre-existing, non-regression; pinned to avoid 1.0.5 corruption) — ga-e7z613 | 1 |
| builds a real bd binary from source; run make test-cmd-gc-process for full coverage | 1 |
| spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage | 1 |
| runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage | 1 |
| runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage | 1 |
| uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage | 1 |
| no bd available (set GC_ACCEPTANCE_BD_BIN) | 1 |
| probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage | 1 |
| bd not probeable in this environment: locate bd: exec: "bd": executable file not found in $PATH | 1 |
| runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage | 1 |
| uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage | 1 |
| runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage | 1 |
| Start: session already exists | 1 |
| concurrent Start(preexisting-concurrent-session): session already exists | 1 |
| first Start: provider environment unavailable | 1 |
| concurrent Start(skip-start-3): provider environment unavailable | 1 |
| Start sentinel: provider environment unavailable | 1 |
| Start(skip-start-11): provider environment unavailable | 1 |
| Start(skip-start-14): provider environment unavailable | 1 |
| Start(skip-start-18): provider environment unavailable | 1 |
| Start skip-start-21: provider environment unavailable | 1 |
| Start skip-start-22: provider environment unavailable | 1 |
| Start(skip-start-25): provider environment unavailable | 1 |
| Start(skip-start-30): provider environment unavailable | 1 |
| Start shared session: provider environment unavailable | 1 |
| tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied | 1 |
| runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage | 1 |
| exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage | 1 |
| bug ga-91uk5m fixed; delete this characterization test | 1 |
| helper process | 1 |
| needs root to chown the fixture to the Secret-mount shape | 1 |
| exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage | 1 |
| set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail) | 1 |
| not invoked as a subprocess helper | 1 |
| pinned beads v1.3.1 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.1/cmd/bd/migrate_dolt_mode.go: no such file or directory | 1 |
| tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied | 1 |
| profile fixture has no edit result to evaluate | 1 |
| filesystem did not preserve setuid bit in test mode: -rwxr-xr-x | 1 |
| uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage | 1 |
| set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens | 1 |
| spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage | 1 |
| PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way | 1 |
| on linux /proc answers directly, so a ps stub cannot make the identity unreadable | 1 |
| spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage | 1 |
| spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage | 1 |
| starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage | 1 |
| spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage | 1 |
| ga-vnycm2.27: interrupted abort-scope converges with different skip metadata | 1 |
| macOS /private/tmp path aliases only apply on darwin | 1 |
| macOS-only /tmp <-> /private/tmp Gemini project path alias | 1 |
| skipping LongLapsed (bead ga-example, expires 2000-01-01): example | 1 |
| runs the real bd provider init; run make test-cmd-gc-process for full coverage | 1 |

## Host-load evidence

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=52.97 run_max_load=52.50 run_mean_load=31.89 run_readings=29 wait_first_load=70.34 wait_max_load=85.17 wait_mean_load=50.80 wait_readings=61 read_errors=0
```

The ordinary wait reached its bounded timeout, then proceeded as prescribed. Wait metrics and run metrics are distinct; the suite ran remotely. No waiver or attributed failure was used.
