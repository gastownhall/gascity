# Supervisor SSE cursor reattachment release gate

**Verdict:** **PASS**

Deploy bead: ga-12r0j8v. Review: ga-diyx1wt. Build: ga-im71dfd.
Condition fixed: ga-spromaw. Evaluated 2026-10-19.

Returning cities now enter the composite SSE cursor before their watcher can
deliver events. A failed or timed-out watcher attachment retains the city's
resolved start sequence for the next sync. A LatestSeq failure still returns
before recording the city.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-diyx1wt records PASS on the resolved reviewed source below; no review carryover or reviewer-added commits. |
| 2 | Acceptance criteria met | PASS | StreamCursor.Attach runs inside MuxWatcher.Sync's start callback, before Watch, and no longer after Sync returns. The membership test helper follows that ordering. The new regression holds beta's watcher attachment while alpha delivers, checks that beta remains in the frame ID, then verifies beta's backlog resumes. It passed by name in both fresh unit and integration-package phases. |
| 3 | Full CI-equivalent coverage | PASS | All four remote phases completed, with 489 fresh remote results. Unit: 286/286 targets; acceptance: 7/7; integration packages: 30/30. Full integration: 13/14 shards passed; one unrelated failure is attributed with positive pristine-base reproduction under criterion 3a below. All 16 required native-floor steps completed with exit 0. Raw failure records are retained. |
| 4 | No unresolved HIGH review findings | PASS | Review records no style/security findings or uncovered acceptance criteria. The failed-attachment case lacks a dedicated test and is an explicitly non-blocking review observation; its intended behavior is included in the PR description. |
| 5 | Final branch clean | PASS | Isolated deploy branch starts at the exact reviewed source. The release-gate record is its only additional change. Empty git status is required after the real pre-commit hook completes and before push. |
| 6 | Merge state and build | PASS | Clean merge against the pinned base below. Materialized tree equals git merge-tree output; bazel build //cmd/gc:gc and full-tree make lint/nogo passed on a fresh view of that merge. |
| 7 | Single feature theme | PASS | Four source/test files implement one supervisor cursor/reattachment fix. Ancestry guard accepted only ga-im71dfd/ga-spromaw citations; no forbidden paths or stack. |

## Source and tested tree

```text
deploy_mode: remote
deploy_branch: deploy/ga-12r0j8v-gate
reviewed_source: 7557cecdf72ec490268363d7a5247023743e23bb
gate_base_sha: b1a5e28d43f2122ff395282e6d0d86e6578a57b6
merge_state: clean (origin/main@b1a5e28d43f2122ff395282e6d0d86e6578a57b6)
gate_base_kind: current-tip
merge_validation: clean merge result built, vetted and tested; merge authority validates the merge-time head/base
tested_merge_commit: fd11c7b9475b0ea23ad0fff532f539cc40574b46
tested_merge_tree: ae438bcbf1b979a5c827207f68e4f20d076bae76
heavy_mode: none
fast_lane: PASS before suite launch, in independent fresh views
policy_lane: pinned fmt-check-changed; make lint with nogo; check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary, check-hooks PASS
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code PASS
ci_lane_run: n/a (no CI configuration changes)
waiver_ref: none (gascity has no waiver path)
```

The source SHA, base, tested merge and tree were resolved against git. The
reviewed branch is provenance only and was neither changed nor pushed. The
source has two commits (test then fix); the gate record adds no runtime code.
The repository has no docs/PROJECT_MANIFEST.md at this revision; the actual
Bazel/Makefile lanes and the deployer's release criteria define this gate.

## Test execution and retained evidence

Evidence root: `/var/tmp/ga-12r0j8v-deploy.P7dxpp` (retained).
The detached original run is `criterion3`; mandatory native completion is
`floor-continuation`. Both finished. No failed candidate test or suite was
retried to obtain a passing result.

```text
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
farm_test_seconds: 7455.218
test_counts: PASS=120428 FAIL=1 SKIP=368
test_counts_basis: top-level result lines per job, summed over 505 retained logs, including the native floor; not unique test names
test_log_dir: /var/tmp/ga-12r0j8v-deploy.P7dxpp/criterion3/evidence/remote/logs
diff_tests_executed: TestSupervisorGlobalEventStreamReturningCityRejoinsIDBeforeItsWatcherDelivers PASS (unit and integration-packages)
owned_topup: [] (no missing owned results)
floor_topup: complete=true, 16/16 steps exit 0; floor-topup.json and floor-logs retained
skip_inventory: remote/skip-inventory.json, 354 events including subtests (71 environment-absence, 283 other)
floor_skip_inventory: floor-skip-inventory.json, 39 events including subtests (29 environment-absence, 10 other)
raw_runner_exit: 1
raw_runner_criterion3_valid: false (unaltered; policy PASS applies criterion 3a after required floor completion)
admission: remote floor=25 GiB, cap=2, free=65.87139129638672 GiB, waited=0.001 seconds
floor_admission: remote floor=25 GiB, cap=2, free=60.17927169799805 GiB, waited=0.001 seconds
rbe_state: rc present; kill switch absent; certificate expires Jan 9 2027; candidate/base worker-env sha256 pins match
```

Each remote phase used `--config=remote-exec --keep_going --config=ci
--nocache_test_results --noremote_local_fallback --rewind_lost_inputs
--remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1` and a
separate `--build_event_json_file`. Commands were:

- Unit/policy: `make check` (includes `bazel test //...` and shell guards).
- Acceptance: `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`.
- Integration packages: `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`.
- Full integration: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v //test/integration:integration_test`, with no smoke filter.

| Phase | Fresh remote results | Cached local/remote | Retries | Phase window | Terminal verdicts |
|---|---:|---|---:|---|---:|
| Unit | 372 | 0/0 | 0 | Valid | 372 |
| Acceptance | 24 | 0/0 | 0 | Valid | 24 |
| Integration packages | 79 | 0/0 | 0 | Valid | 79 |
| Full integration | 14 | 0/0 | 0 | Valid | 14 |

The BEP predicate was independently recomputed from each raw BEP using its
recorded phase timestamps. All 978 raw log/XML artifacts match their retained
SHA-256 digests; readable log normalization matches the runner's removal of
0x16 framing bytes. `final-evidence-audit.json` retains that audit.
`gate-test-evidence.py` independently verified the owned test by name.

The native floor selected every environment-absence skip's parent test in
13 packages (including the mandatory beadstest package), plus Darwin fsys
compile, push-gate-lock self-tests (55 passed) and concurrency self-tests
(25 passed). Its separate logs and commands remain in `floor-topup.json`.

The wrapper reported PASS-THROUGH on the disposable merge worktree; no claim
of an isolated clone is made. The floor scrubbed ambient GC/BD/BEADS/DOLT
selectors for each command and used pinned bd v1.3.1 (c1c4b642a).

```text
load_scope: native-floor continuation (remote admission has no host-load wait)
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 102.21
run_max_load: 127.32
run_mean_load: 116.07
run_readings: 12
wait_first_load: 43.09
wait_max_load: 102.21
wait_mean_load: 68.62
wait_readings: 61
```

This is the documented ordinary soft wait: timeout proceeds and records the
load. Every native step exited 0; the high load is disclosed without using it
to explain a failure.

## Criterion 3a attribution

`TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step`
fails at `test/integration/gc_live_contract_test.go:1509`: HTTP 422, expected
200, because `query.assignee` is missing. Tracker ga-od4rcxy was opened and
its designated fix ga-ze0aym0 is open and routed to the builder.

- Clause 1: not diff-owned. The integration fixture is unchanged; official
  `--site` reports UNCHANGED, untouched helper, zero shared hunks in that file.
- Clause 2: first sighting in this run. The gm-sf3238 discovery-round rule
  applies because clauses 1/4 are clear and the positive clause-3 proof landed.
- Clause 3, proof (d): the identical missing-assignee 422 was reproduced on
  pristine pinned base b1a5e28d43f2122ff395282e6d0d86e6578a57b6. This is a
  deterministic fixture condition, reproduced by the focused real-binary
  Bazel integration run, rather than a claim about host contention. Fixture,
  input, handler and route blobs are identical between base and candidate.
- Clause 4: no path overlap. The failing package is test/integration; the
  production changes are in internal/api and internal/events.

```text
failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy | clause 3: d, positive pristine-base reproduction
base_repro_log: /var/tmp/ga-12r0j8v-deploy.P7dxpp/base-repro/test.log
base_repro_log_sha256: 017eab47c0c160f3f5c66bcdf3ab758a175a332bab057adf3dfb4e8ce2799006
attribution_record: /var/tmp/ga-12r0j8v-deploy.P7dxpp/failure-attribution.json
completion_record: /var/tmp/ga-12r0j8v-deploy.P7dxpp/attributed-completion.json
```

The raw runner remains failed with criterion3_valid=false. Criterion 3's
policy PASS attributes that one failure and includes the completed mandatory
floor; it does not replace raw evidence or use a waiver. The focused base
run is attribution evidence only.

## Skips and supplementary checks

All owned results passed. Other skips cover platform-only behavior,
deliberate fixture/capability branches, optional live backends, legacy
binaries, unavailable services and separately activated process lanes.
Every environment-absence skip was selected for the required native floor;
remaining native skips include replay/simulation inputs, live herdr,
Postgres DSN, br, tmux binding context and intentional conformance branches.
Their exact names/reasons are retained in both inventories. These do not
exercise the changed supervisor cursor ordering.

The three API process-signal skips are in the untouched
handler_status_scoped_store_test.go, selected out of the unit lane. The
shared test-helper changes are gatedAttachProvider (used only by the new
regression) and membershipConsumer.sync (events tests); neither reaches
those signal-test skip conditions. No init, TestMain or build-tag change.

`make dashboard-check` passed all ten targets on a fresh merge view; this
supplementary check included cached results and is separate from the forced
fresh suite above. `make dashboard-build-npm` and local `npm run preview`
served HTML and JavaScript with HTTP 200. The preview process group stopped
and its port was confirmed closed. Evidence: `dashboard.log` and
`dashboard-preview.log`. Hooks are active (`core.hooksPath=.githooks`);
the real pre-commit docsync gate runs for this gate-record commit.
