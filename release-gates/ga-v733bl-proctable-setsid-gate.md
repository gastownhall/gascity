**Verdict:** **PASS**

# Release gate: deterministic setsid child selection (ga-v733bl)

- Reviewed deploy source: `32a2ca3fe7cedb8ef516294cc00d1c8deab36ae3` (build ga-cfr67u, review ga-92qs9f).
- Full-suite merge base: `origin/main@fa413406025fecb8f6c8461782e18743b1e2e24f`; materialized merge `c3b23a41fd652f7d30592f780e5c6905229e4c6d` (tree `f3784c0799907070edd91ab34cac303ed299daff`).
- Latest base checked before deploy: `origin/main@1337499b71e1a77eaedb295945822349a49b73e7`; clean merge tree `16161880ddfc0e07d1c452eef106e0140651f59f`.
- Deploy mode: remote; push target: fork; isolated branch: `deploy/ga-v733bl-gate`.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Clean divergence from main | PASS | `git merge-tree --write-tree origin/main 32a2ca3fe7cedb8ef516294cc00d1c8deab36ae3` returned 0, producing `16161880ddfc0e07d1c452eef106e0140651f59f`. The reviewed source is not already on main. Ancestry scope accepted only ga-v733bl and its build bead ga-cfr67u; no internal documentation path enters the diff. |
| 1 | Review PASS present | PASS | Review bead ga-92qs9f records a final PASS pinned to `32a2ca3fe7cedb8ef516294cc00d1c8deab36ae3`; no review carryover or waiver is used. |
| 2 | Acceptance criteria met | PASS | The test captures its own setsid child PID from the spawning shell's `$!`, rather than a system-wide process-name lookup; an ambient child and a second decoy are logged with distinct PIDs. The existing `sid == pid` assertion still rejects a broken setsid. The reviewer recorded the RED reproduction; the current test passed twice in the full sweep and five more times on the latest merge tree. Build bead ga-cfr67u is stamped `gc.fixes_tracker=ga-961qe1`. Only the proctable test file changes. |
| 3 | Tests pass | PASS with one attributed failure | The documented full-scope 40-job `make test-local-full-parallel` completed: 39 PASS, 1 FAIL, 0 skipped jobs; 53,318 PASS, 1 FAIL, 232 SKIP Go test results. The changed test passed in both unit and integration package jobs. The single failure is the independently reproduced systemd unit condition tracked by ga-ltjdum; details below. `test_cmd_scope: full-suite`. |
| 3b | Required policy and lint lanes | PASS | `make test-ci-policy`, `make lint-affected`, `make fmt-check-changed`, `go build ./...`, `go vet ./...`, and `git diff --check` passed on the latest merge tree. Native dependency guard, native DoltLite beads, Tier A acceptance, bd v1.0.4 contract, beads topology and proxied-native acceptance also passed. All four worker profiles passed both phase-1 and phase-2 lanes on the latest merge tree. |
| 4 | No high-severity review findings open | PASS | The review bead records no unresolved HIGH finding; style and security reviews report none. |
| 5 | Final branch clean | PASS | The merge scratch and role worktree were clean before the gate file was added. After committing this record on the isolated deploy branch, `git status --short` was empty. |
| 7 | Single feature theme | PASS | The source adds one test-only fix in `internal/runtime/proctable/supervisor_fork_orphan_repro_test.go` (+52/-4), with RED and GREEN commits both citing ga-cfr67u. |

## Criterion 3 evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel
wrapper: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- bash -c 'make test-local-full-parallel'
test_cmd_scope: full-suite
test_counts: 39 PASS / 1 FAIL / 0 SKIP jobs; 53,318 PASS / 1 FAIL / 232 SKIP test results
diff_tests_executed: TestSetsidDoesNotPreventOrphanSelection PASS in unit-core and integration-packages-core-2-of-4; 0 FAIL, 0 SKIP
waiver_ref: none
logs: /var/tmp/ga-v733bl-r2c-full.log and /var/tmp/ga-v733bl-r2c-full-shards/
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
load_start: 11.65
load_max: 28.33
load_mean: 18.59
```

The 232 skipped results are pre-existing platform, opt-in, and fast-tier exclusions, each with a reason in the test output. None belongs to the changed test. Rootless Podman was reachable through `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, Ryuk was disabled per the host recipe, and the pinned Dolt image was cached before the full run.

`failure_attribution: TestStartDrift_SystemdManaged_RestartsToNewBuildID -> ga-ltjdum | clause 3d BASE_REF reproduction: /var/tmp/ga-ltjdum-repro-base.log contains the same systemd unit-not-found condition on an untouched base.` The full-suite failure is in `integration-rest-full-5-of-8.log`: `systemctl --user start` exits 5 because the unit is not found under the private test HOME. Clause 1 passes: the candidate did not change `test/integration/start_drift_test.go`. Clause 2 passes: ga-ltjdum predates this run, names this exact test and condition, and was read; this run's sighting was appended. Clause 3 is proven by the independent base reproduction and the test-only proctable diff. Clause 4 passes: no path overlap. The inconclusive path was not used.

The repeat-condition fix-carrying exception applies: this deploy's build bead ga-cfr67u is stamped as the fix for ga-961qe1, and ga-ltjdum's designated fix bead ga-2qe684 is stamped `gc.fixes_tracker=ga-ltjdum`, has a blocked work record, and its commit `c9447df6d6f6651e6708575f041da04cba79721e` is not on `origin/main`. The raw FAIL remains visible; it is attributed, not rewritten as green. `TestHumaBinary_SessionMessageAsync` and `TestGraphWorkflowFailureRunsCleanup` both PASS in the completed full run.

Required supporting lanes:

- `make test-acceptance` with pinned bd `f45b249ce6b40ba62aecc03949e6371e8f7c79d8`: PASS; package time 653.111s.
- `make test-bd-cli-contract` with minimum-supported bd v1.0.4: PASS.
- Beads topology: proxied default and M1 local topology PASS. Legacy migration tests and M5 skip because the optional pre-journal GC binary is unavailable; neither path is changed by this diff. Proxied-native safety and lifecycle PASS.
- `make test-native-doltlite-beads`, `make check-native-dependency-surface`, `make check-gomod-replace`, `make check-eventexport-isolation`, `make check-core-boundary`, `make check-docs`: PASS.
- Worker phase-1 and phase-2 (`claude`, `codex`, `cursor`, `gemini` tmux-cli profiles): 8/8 PASS on latest merge tree.
- `make test-ci-policy`, `make lint-affected`, `make fmt-check-changed`, `go build ./...`, `go vet ./...`, `git diff --check`: PASS on latest merge tree. `.githooks` is active (`make check-hooks` PASS).

## Base movement

The full suite started when main was `fa413406025fecb8f6c8461782e18743b1e2e24f`. Main later advanced to `1337499b71e1a77eaedb295945822349a49b73e7` with identity/claim code; it has no path overlap with the proctable test change and merges cleanly. On that latest merge tree, the changed test passed five consecutive runs with distinct decoy/target PIDs, all four worker profile pairs passed, and build, vet, policy, lint, format, and whitespace checks passed. The earlier private-HOME run that exposed a zsh first-run prompt was interrupted and excluded from this verdict; the completed run above used a fresh HOME with `.zshrc` and its tmux integration check passed.
