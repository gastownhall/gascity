# Mail injection inbox promise — ga-yw3kxx

**Verdict:** **PASS**

Reviewed source: `4f42910731bd2d4e2cbe5fc8de5d15b1e60ccaa7` (build ga-8gdkfy, review ga-70qwum).
Deploy branch: `deploy/ga-yw3kxx-gate`; deploy mode: remote; push remote: fork.
Gate base (`origin/main` resolved when evaluated): `b273ac7e6ac335e46bf3f3a7353b36f46a3efef2`.
Materialized merge: `2b090bb9ff9c447c7e2ed97e5a1bb6504ffcac50`; tested tree: `4d70a7d03e2edc8c7c6abf482cde96d0c0b7d961`.
Public issue: gastownhall/gascity#7208. The PR must say `Closes #7208`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-70qwum records PASS at the resolved reviewed source. Single-pass review; the second pass is disabled. No branch tip substituted. |
| 2 | Acceptance criteria met | PASS | Both explicit mail injection and managed SessionStart label archived auto-handoffs and count the unread remainder. Empty-body handoffs use one preview slot with all IDs newest first; ordinary messages fill remaining slots; body-bearing handoffs retain separate lines and priority. The selected/rendered messages are the archived set, after a successful write. The archive-on-delivery field is hidden from JSON. Seven owned tests and four children pass in both unit and integration; named unchanged priority/archive neighbors pass. |
| 3 | Tests pass | PASS | FAST lane passed before suite launch. Canonical full `make test` plus whole Bazel acceptance/integration/smoke targets and native Go acceptance passed, with retained raw logs/XML and all owned tests mapped by name. All 11 CI leaf jobs are accounted for below; release-config is deferred under ga-1zgega's host-precondition ruling and must pass on the exact PR head before merge. |
| 4 | No high-severity findings open | PASS | Zero unresolved HIGH findings. Review's low findings remain disclosed: unmanaged read-only SessionStart wording, count-only group formatting and retained IDs, surviving count mutant, historical comments, and unchanged write-before-archive ordering. |
| 5 | Final branch clean | PASS | Isolated branch is cut at the resolved source with only this gate record added. Clean source checkout and final post-commit status are independently checked before push; no implementation or generated output changed during the gate. |
| 6 | Diverges cleanly from main | PASS | `git merge-tree --write-tree` returned 0; its predicted tree equals the materialized tested merge tree. Fresh-view `go build ./...` and `go vet ./...` passed. No self-rebase used. |
| 7 | Single feature theme | PASS | Four source commits cite accepted build ga-8gdkfy; seven source files implement one mail-preview/archive promise. Scope guard accepts ga-yw3kxx + ga-8gdkfy, no undeclared stack or denied internal docs. |

The ga-vtxkj5 prerequisite landed via #6939 at
`93a1f646bb21f4d4927f26390b44278b814df363`, verified as an ancestor of the gate base.
The mayor already corrected its shipped work record. This PR's future merge SHA
is not that prerequisite's landing SHA.

## Full test evidence

- `test_cmd_scope: full-suite`
- `heavy_mode: none` (official classify output retained; no listed package selected).
- `diff_tests_executed: 7/7 PASS`, plus all four owned subtests PASS.
- `waiver_ref: none`; `failure_attribution: none`; `policy_attribution: none`.
- `ci_lane_run: n/a (no CI-config change in this diff)`.
- `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-yw3kxx.regate-integration-recovery/logs` (367 per-job normalized logs retained).
- Original raw unit/acceptance logs and XML: `/var/tmp/gc-heavy-gate/runs/ga-yw3kxx.regate-suite/bazel-artifacts`.
- Recovered raw integration/smoke logs and XML: `/var/tmp/gc-heavy-gate/runs/ga-yw3kxx.regate-integration-recovery/bazel-artifacts`.
- Raw hashes are rechecked against retained manifests; no log deleted.

Commands actually run, on fresh clean views of the same merge tree:

1. `make test BAZEL='bazel --batch' BAZEL_FLAGS='--config=ci --config=fork-cache --jobs=4 ...'` — full `//...`, 231/231 targets PASS.
2. `bazel --batch test --config=ci --config=fork-cache --jobs=4 --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` — all 5 targets PASS.
3. `bazel --batch test --config=ci --config=fork-cache --jobs=4 --remote_download_outputs=all --rewind_lost_inputs --config=integration --keep_going //test:integration_packages` — 19/19 targets, 41/41 shard results PASS, every target/shard attempt 1.
4. `bazel --batch test --config=ci --config=fork-cache --jobs=4 --remote_download_outputs=all --rewind_lost_inputs --config=integration-smoke --keep_going //test/integration:integration_test` — 1/1 target, 14/14 shard results PASS, attempt 1.
5. `make test-acceptance-go ACCEPTANCE_GO_TEST_FLAGS='-v -count=1'` — actual native preflight-acceptance job, rc=0; 169 PASS / 0 FAIL / 15 SKIP top-level, 473 / 0 / 15 including subtests.

All Bazel commands carry explicit `GO_TEST_WRAP_TESTV=1`, private ICU74
`LD_LIBRARY_PATH`, rootless `DOCKER_HOST`, `TESTCONTAINERS_RYUK_DISABLED=true`
and `BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1`. Exact argv are retained in
`/var/tmp/ga-yw3kxx-regate-suite-runtime.sh` and
`/var/tmp/ga-yw3kxx-regate-integration-recovery-runtime.sh`.

| Phase | PASS | FAIL | SKIP |
|---|---:|---:|---:|
| unit | 52964 | 0 | 216 |
| acceptance | 402 | 0 | 11 |
| integration-packages | 33378 | 0 | 52 |
| integration-smoke | 16 | 0 | 1 |
| Total canonical Bazel result lines | 86760 | 0 | 280 |

These are terminal result lines across jobs, not distinct test names. Native
Go counts above are separate. `gate-test-evidence.py` reports `ok:7` and uses
these runs' logs, not reviewer logs or an aggregate boolean.

| Diff-owned test | Unit | Full integration |
|---|---|---|
| `TestFormatInjectOutputCollapsedAutoHandoffGroup` | PASS | PASS |
| `TestMailCheckInjectAutoHandoffsDoNotDisplaceOrdinaryMail` | PASS | PASS |
| `TestMailCheckInjectKeepsInboxPromiseForArchivedAutoHandoffs` | PASS | PASS |
| `TestMailCheckInjectRendersAutoHandoffBacklogAsOneLine` | PASS | PASS |
| `TestMailInboxListsAutoHandoffsUntilInjectArchivesThem` | PASS | PASS |
| `TestSessionStartAutoHandoffInjectionAssertsRulesAAndB` | PASS | PASS |
| `TestSessionStartAutoHandoffInjectionRendersBacklogAsOneLine` | PASS | PASS |

`TestFormatInjectOutputCollapsedAutoHandoffGroup`'s four children also PASS:
`one_empty-body_auto-handoff`, `a_backlog_is_one_line,_newest_first`,
`the_group_costs_one_slot_and_ordinary_mail_fills_the_rest`, and
`a_body-bearing_auto-handoff_keeps_its_own_line`.

Required unchanged neighbors all PASS in unit and integration:
`TestMailCheckInjectFloatsPriorityAutoHandoffIntoWindow`,
`TestMailCheckInjectArchivesAutoHandoffMessages`,
`TestMailCheckInjectArchivesEphemeralAutoHandoffMessages`,
`TestInjectPreviewSurfacesNewestUnread`, and
`TestInjectPreviewPriorityStillOutranksRecency`.
Actual process coverage PASS in integration includes `TestTutorial01`,
`TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat`, and
`TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir`.

## Skip audit

Every skipped site's function and line are UNCHANGED under
`diff-owned-tests.py --site`; all audit return codes are 0, with no unresolved
site. Owned regression tests never skip. The changed shared test helpers
`injectedBulletLines`, `injectedLinesNaming`, and `createUnreadAutoHandoff`
are only called by the seven owned tests. The added regexp is a pure compiled
constant; no TestMain/init/build-tag or skip helper changed. No changed helper
can reach an unchanged skip guard.

- Unit: 216 skips. Pre-existing platform exclusions, optional live tools,
  process/measurement/registry opt-ins, and container/tag guards. Required
  process tests omitted by the unit lane PASS in the full integration lane.
- Bazel acceptance: 11 skips. Source-declared selfhost placeholders, opt-in
  live registry and historical legacy-gc migration fixtures. No owned test.
- Integration packages: 52 skips, all exact reasons/sites retained. Optional
  SSH/herdr/br and real tmux opt-ins, live registry measurements, harness
  process sentinels, upstream bd capability, separate PostgreSQL DSN fixtures,
  missing historical crew template, and pre-declared readiness/cwd guards.
  `TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors` skips at
  `internal/beads/native_dolt_store_test.go:685` before its assertion because
  its direct upstream Open call has no configured SQL port (127.0.0.1:0).
  The changed mail rendering cannot configure/reach that constructor guard;
  actual Podman-backed Dolt tests PASS with the runtime below.
- Integration smoke: sole skip `TestBdStoreConformance` at
  `test/integration/bdstore_test.go:55`, an unchanged unconditional t.Skip
  from ga-e7z613 before runtime detection. Environment changes cannot remove
  this source guard. All 14 names in the official smoke filter have terminal
  results: 13 PASS, this one unchanged SKIP. Empty shard partitions are
  accounted for, not mistaken for omitted names.
- Native acceptance: 15 skips, all sites unchanged with zero shared hunks.
  Five topology-matrix roots are deliberately disabled by this native CI
  target and PASS in the complete Bazel acceptance lane. Two require the
  removed historical legacy gc; seven are source-declared selfhost
  placeholders; one requires an optional live registry source. The changed
  mail rendering does not reach their setup/placeholder guards.

Exact per-name outputs, reasons, source-site evidence and raw artifact links
are retained as `unit-skip-audit.json`, `acceptance-skip-audit.json` in the
original run; `integration-packages-skip-audit.json`,
`integration-smoke-skip-audit.json`, `preflight-acceptance-skip-audit.json`
in the recovery run. Detailed required-name maps are retained beside them.

## Policy, generated files and CI closure

`policy_lane: PASS` — pinned golangci-lint 2.12.0, affected/importer lint,
changed formatting, full nogo, `test-ci-policy`, `check-gomod-replace`,
`check-native-dependency-surface`, `check-eventexport-isolation`,
`check-core-boundary`, `check-docs`. `gate-base.sh run` pins
`LINT_CHANGED_REF` to the gate base.

`drift_lane: PASS` — `make bazel-sync` followed by `git diff --exit-code`;
`make dashboard-ci` followed by dashboard generated-tree diff;
all three spec/client/schema in-sync targets; `make openapi-breaking-check`
with pinned `OPENAPI_BREAKING_BASE`. Each lane uses its own fresh view.
`make check-hooks` confirms the normal local hook. The first dashboard
attempt missed the private ICU loader namespace and failed two loader jobs;
that full failed evidence is retained, and the corrected environment passed
10/10 dashboard targets and 3/3 codegen targets before the suite launched.
No source fix or policy attribution was used.

The merged workflow blob is `9c6ede65527bafa8b0f55693494f072c23f05970`.
`ci_required_triggers.py` accounts for all 11 leaf jobs (13 including
aggregators): mail, cmd_gc_process and integration filters match; shared,
beads, packs and credential_provider do not. Full local suites ran anyway.
No job in this closure is silently omitted.

| CI leaf job | Status | Actual evidence |
|---|---|---|
| `preflight-static` | LOCAL-PASS | `bash .github/scripts/go-mod-download-retry.sh; GOPROXY=off go mod verify` |
| `runner-policy` | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/ga-yw3kxx-regate-runner-output.txt PYTHONDONTWRITEBYTECODE=1 python3 .github/workflows/scripts/runner_policy.py` |
| `changes` | LOCAL-PASS | `python3 /var/tmp/ga-1zgega-gate-tools/ci_required_triggers.py MERGE_SCRATCH GATE_BASE_SHA DEPLOY_SHA fork` |
| `release-config` | DEFERRED-TO-PR-CI | `command -v goreleaser: rc=1, empty stdout/stderr; host-precondition-failed under ga-1zgega` |
| `preflight-generated` | LOCAL-PASS | `make openapi-breaking-check` |
| `integration-rest-full` | PUSH-ONLY | `  integration-rest-full                  LEAF       linux                      PUSH-ONLY (never runs on a pull request)` |
| `credential-provider-windows` | NOT-TRIGGERED | `credential-provider-windows              LEAF       NON-LINUX windows-latest   NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'` |
| `preflight-unit-cover-noncmdgc` | PUSH-ONLY | `preflight-unit-cover-noncmdgc            LEAF       linux                      PUSH-ONLY (never runs on a pull request)` |
| `preflight-unit-cover-cmdgc` | PUSH-ONLY | `preflight-unit-cover-cmdgc               LEAF       linux                      PUSH-ONLY (never runs on a pull request)` |
| `pack-gate` | NOT-TRIGGERED | `pack-gate                                LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'` |
| `preflight-acceptance` | LOCAL-PASS | `make test-acceptance-go ACCEPTANCE_GO_TEST_FLAGS='-v -count=1'` |

Preflight-static's shared setup downloaded successfully on attempt 1/4;
`GOPROXY=off go mod verify` verified every module. No shared-cache purging
fallback ran. Native preflight-acceptance is the actual Make command, not a
proxy. The Windows reach probe found no credential-provider inputs; the
three PUSH-ONLY jobs do not run on a PR. Full CI definitions, trigger rows,
commands, return codes, probes and logs are retained in
`/var/tmp/ga-yw3kxx-regate-ci-accounting-complete.json`.

**Merge condition:** `release-config` and `CI / required` must succeed on
the exact opened PR head before MPR merges. A successful local gate and
clearance status do not waive that deferred job.

## Runtime, load and recovered transport failure

The rootless Podman socket was verified active and the pinned
`dolt-sql-server:2.2.0` image verified before the suite
(image SHA `7aedf01cfa63b26616624f7a8a07c1b9ef7158c1a7787e21e8fc1b270fa27d38`).
Ryuk is disabled together with the explicit beads opt-out under the existing
host testcontainer sweep. Pinned bd v1.3.1 is first on test PATH.
The isolation wrapper reported **PASS-THROUGH / NOT ISOLATED**, and no
inherited BD_/BEADS_/GC_/DOLT_ environment names. Fresh clean disposable
views, explicit test runtime variables, Bazel hermetic sandboxes, and the
native target's `env -i` supply the actual isolation. The private ICU74
mount namespace leaves host libraries/loader configuration unchanged.
The native Go shim's existing process/cache/cgroup policy remains active.

Original detached suite load:

```text
load_threshold=15 load_waited_seconds=1350 load_wait_timed_out=0
run_start_load=14.99 run_max_load=67.03 run_mean_load=27.60 run_readings=85
wait_first_load=60.93 wait_max_load=60.93 wait_mean_load=24.82 wait_readings=46 read_errors=0
```

Bounded recovery detached service load:

```text
load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=1
run_start_load=31.58 run_max_load=41.04 run_mean_load=28.18 run_readings=59
wait_first_load=21.62 wait_max_load=55.66 wait_mean_load=37.74 wait_readings=61 read_errors=0
```

These are the runner's five-minute load readings. The documented ordinary
wait proceeds on timeout; heavy_mode is none. Evidence and bead notes
record that timeout rather than pretending the host reached the threshold.

The original full run completed all unit and acceptance targets, then Bazel
integration failed to build 10/19 targets with lost cached inputs and a
remote-cache circuit-breaker message. The failed phase is excluded from PASS
counts and remains retained in full. It is tracked by ga-i0ccz5. One bounded
transport recovery used supported `--rewind_lost_inputs` and eager output
downloads with unchanged whole targets, configuration and source. All missing
phases then completed. This is new passing evidence, not a waiver or failure
attribution. It does not claim a durable landed fix for the host condition.
Both completed prior phase manifests and every raw hash were verified before
reusing their same-tree results. The original driver returned logical rc=1;
the recovery service output says `CANONICAL_SUITE_RECOVERY_PASS` and
`GATE_RUN_EXIT rc=0 state=complete`, with systemd inactive/dead.

Artifacts and detached runs survive a session recycle. No shared cache was
cleaned, runtime policy changed, or test source patched. The deployer opens
the PR and routes it to mayor for MPR; it does not merge.
