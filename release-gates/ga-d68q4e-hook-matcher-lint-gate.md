# Hook matcher lint release gate

**Verdict:** **PASS**

Bead: `ga-d68q4e`. Recorded 2026-10-08T15:15:55.586679+00:00.

gc lint reports permission-rule or invalid-regex hook matchers as errors, and unknown tool matchers as advisory warnings. Empty/star and valid tool/source matchers remain accepted.

## Source and gate tree

- Reviewed source: `f0126cf5f5094af26f0b18ce4f720eac6e2cc829`; reviewer `ga-ofv4hq` PASS; build `ga-108vhy`.
- Pinned main: `e39d98ccfa23364258af6e26abe1f673d6c06bfd`.
- Canonical normal-hook merge: `9b55f6c4f5f2f6022c14947fcf854ced3520392a`; tree `9d83be635385d89a8f4a6ae01fc89a554d4fef1a`, equal to predicted conflict-free merge. Parents are pinned base then exact reviewed source.
- Tests ran in `/var/tmp/gc-merge-ga-d68q4e-rekey-fresh.zhxAke`; fresh independent views ran build/vet, fast drift checks, CI accounting and real CLI fixtures.
- Publication branch: `deploy/ga-d68q4e-gate`, cut exactly at reviewed source; this record is its only addition.
- Preflight: reviewed source is not an ancestor of pinned main; all three source patches are absent by git cherry; commit-associated target PR query returned none. No self-rebase.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer ga-ofv4hq recorded PASS at the resolved reviewed source; three TDD/grammar commits, no source substitution. |
| 2 | PASS | Nine owned roots test permission-syntax priority, invalid regex, warning severity, valid matchers, JSON shape handling and sorted findings. Ten built-CLI fixture commands verify human/JSON output and exit/error counts for clean, permission, invalid regex, warning-only and mixed packs. Existing bare-hook traversal consumers also PASS. |
| 3 | PASS | Whole-repository unit, acceptance including solo fixtures, integration packages and integration smoke complete as uncached first attempts. One unchanged session integration failure is attributed by measured coverage to ga-qzxpqo; raw FAIL and original failed pipeline remain recorded. Nine owned roots and all surviving roots in the changed test files have real named PASS in their configured lanes. All four required CI leaves accounted below; none deferred or uncovered. |
| 4 | PASS | No HIGH or blocking review finding. Non-blocking test/event and terminal-control-character hardening follow-ups ga-lwtazy/ga-d0hhh5 remain separate. Go RE2 proxy and known-tool snapshot limits are explicit. |
| 5 | PASS | Canonical and publication checkout clean before record. All generated/BUILD drift checks and untracked-inclusive status PASS; final committed checkout is checked before publication. |
| 6 | PASS | Normal-hook materialized merge matches predicted tree. Independent fresh-view go build ./... and go vet ./... exit0. |
| 7 | PASS | One feature: validating pack-overlay hook matchers through existing gc lint. Four changed Go files; all three source commits cite ga-108vhy; no internal agent config paths or unrelated theme. |

## Full-suite evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: TestSuspend_AutoRealTmuxDeletedSocketFailsAndDeadServerSucceeds -> ga-qzxpqo, clause3(c) COVERAGE`; `policy_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`.

Common Bazel flags: `--config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs`, with recorded runtime test env and per-phase BEP.

```text
make check BAZEL="bazel --batch" BAZEL_FLAGS="<common flags + runtime env + BEP>"
bazel --batch test <flags> --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel --batch test <flags> --config=integration --keep_going //test:integration_packages
bazel --batch test <flags> --config=integration-smoke --keep_going //test/integration:integration_test
```

| Lane | Targets | PASS | FAIL | SKIP |
|---|---|---|---|---|
| unit | 232 | 53249 | 0 | 214 |
| acceptance | 7 | 410 | 0 | 13 |
| integration-packages | 26 | 34998 | 1 | 53 |
| integration-smoke | 1 | 16 | 0 | 1 |

Counts include subtests and repeated coverage across CI lanes. Totals: `{"all": {"FAIL": 1, "PASS": 88673, "SKIP": 281}, "top_level": {"FAIL": 1, "PASS": 47770, "SKIP": 218}}`; 383 Bazel test executions. Top-level counts retained separately.

Diff-owned named results:

- `cmd/gc/TestLintAcceptsShippedHookMatchers`: PASS in unit and integration.
- `cmd/gc/TestLintRejectsPermissionSyntaxHookMatcher`: PASS in unit and integration.
- `cmd/gc/TestLintWarnsOnUnknownToolHookMatcherWithoutFailing`: PASS in unit and integration.
- `internal/overlay/TestFindInvalidHookMatchers_Classification`: PASS in unit (overlay is not a member of //test:integration_packages).
- `internal/overlay/TestFindInvalidHookMatchers_IgnoresEntriesWithoutMatcher`: PASS in unit (overlay is not a member of //test:integration_packages).
- `internal/overlay/TestFindInvalidHookMatchers_InvalidJSON`: PASS in unit (overlay is not a member of //test:integration_packages).
- `internal/overlay/TestFindInvalidHookMatchers_NoHooksObject`: PASS in unit (overlay is not a member of //test:integration_packages).
- `internal/overlay/TestFindInvalidHookMatchers_OrdersFindingsByCategoryThenIndex`: PASS in unit (overlay is not a member of //test:integration_packages).
- `internal/overlay/TestFindInvalidHookMatchers_PermissionSyntaxOutranksCompileError`: PASS in unit (overlay is not a member of //test:integration_packages).

All 41 surviving roots in both changed test files have named PASS: `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c3/changed-test-consumers-pass.json`. This also covers the extracted bare-hook traversal and consumers of shared helper/import hunks. Tutorial01 including 08-agent-pools actually PASSed in integration with GC_FAST_UNIT=0. TestSchemaFreshness and the OpenAPI base/comparator/fixtures actually PASSed.

Real CLI fixtures: `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c2/FIXTURES_VERIFIED.json`, SHA-256 `a4065eb37e4c81b8dceacab26a50998db3d4f7537015ab7d2def39c2d0340102`. No installation of the built binary into the city.

## Skip integrity

Each SKIP has its printed reason, actual source guard, complete per-file --explain, --site UNCHANGED result, shared-hunk accounting and test-binary reach in `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c3/full-skip-site-proof.json` (SHA-256 `04c5b3d48267e3046117da3e2a860b95fc7da44640bfa7560e9d4580f4a8c812`). Every skipped site is outside an owned test, no changed test file has a SKIP. Every actual site/reason was read individually.

Production changes add pure matcher classification and a warning constructor only in the gc lint overlay walk; the extracted JSON traversal preserves old bare-hook behavior. The new fixed valid regex literals and private maps/lists perform no I/O, change no environment or fixture. Actual default/integration/acceptance closures acknowledge cmd/gc and overlay reach; import absence does not rule out CLI subprocesses. Source guards are reviewed independently: none reads a value changed by lint matcher classification. New test helpers are called only by owned roots; added standard-library imports do not modify setup or skip selectors. All unit process omissions have fresh same-name/package integration PASS. Parent PASS never stands for skipped child execution.

- unchanged dependency feature guard: 2 result rows.
- unchanged explicit external fixture or opt-in guard: 27 result rows.
- unchanged explicit historical placeholder or disabled characterization: 20 result rows.
- unchanged external executable, runfile or capability precondition: 40 result rows.
- unchanged host permission or procfs capability guard: 4 result rows.
- unchanged narrowly tracked conformance row: 4 result rows.
- unchanged platform guard: 30 result rows.
- unchanged provider conformance fixture: 20 result rows.
- unchanged retired characterization condition: 1 result rows.
- unchanged skip-ledger example fixture: 1 result rows.
- unchanged subprocess helper entry guard: 9 result rows.
- unchanged unsupported fixture capability: 17 result rows.
- unchanged upstream fixture connection guard: 2 result rows.
- unit process coverage executed in integration: 104 result rows.

## Required CI job accounting

`ci_required_closure`: actual ruled tool on the merged tree, source/base above, head location fork; merged ci.yml blob `7c1953ee32ea7c17a01fe1c19ecf133642a7ff4c`; 4 LEAF jobs. Accounting follows ga-1zgega; aggregators take no status.

| Leaf | Accounting | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact command/trigger row, env, exit and stdout/stderr retained in proof. |
| changes | LOCAL-PASS via required-triggers tool | Exact command/trigger row, env, exit and stdout/stderr retained in proof. |
| credential-provider-windows | NOT-TRIGGERED ;   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' | Exact command/trigger row, env, exit and stdout/stderr retained in proof. |
| pack-gate | NOT-TRIGGERED ;   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' | Exact command/trigger row, env, exit and stdout/stderr retained in proof. |

`windows_reach: none`; `deferred_to_pr_ci: none`; `not_covered: none`.

Required CI proof `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.ci-accounting/ACCOUNTING_VERIFIED.json`, SHA-256 `f1b3abcc4345747196140113179930c31acbb3d23caf97fed16cdcd2bf99bf89`. Actual filter is filtered, shared=false; Windows and pack jobs NOT-TRIGGERED by the tool rows. Runner-policy ran with EVENT_NAME=pull_request PR_AUTHOR=quad341 empty FORCE_BLACKSMITH. No Windows runtime PASS is claimed.

Merge only after CI / required is green at the exact gated PR head. Gate PASS permits publication/handoff.

## Fast lane and runtime

Whole-repository nogo, shell/policy/hooks, CI policy and all18 required policy/generated/OpenAPI targets PASSed before the suite. Each mutation lane ran in a fresh view under gate-base.sh; make bazel-sync plus git diff --exit-code and empty untracked-inclusive status PASS. Proof `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.fast-rest/COMPLETE_FAST_VERIFIED.json`, SHA-256 `9bfd491f1e2a7b8feb3ce2723d715a370d449eeaca3bd58d36528cca356fd74c`.

OpenAPI form: workflow-equivalent per ga-3rtlng; standard form not run while ga-twhw87 is open. E1-E6: tracker open and merged rule still rctx.symlink(spec, "openapi.json"); actual real pinned-base file under run directory outside /tmp exactly matches git bytes; TestCommittedSpecAgainstBase, TestGateAgainstOasdiff and all six breaking fixtures PASS. Teeth proof ga-2egszb and /var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro. Exception sunsets on tracker close or rule change. Base SHA-256 `04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912`.

Podman socket, pinned Dolt2.2.0 cached image and enabled container-sweep verified before C3. Ryuk-disabled and BEADS_ALLOW_UNREAPED_TESTCONTAINERS paired. Test bd v1.3.1 and private ICU runtime supplied. Isolation wrapper honestly reports PASS-THROUGH / NOT ISOLATED; fresh worktrees, detached/Bazel sanitized environments and test-owned fixtures provide isolation.

`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.62 run_max_load=17.42 run_mean_load=11.85 run_readings=56 wait_first_load=13.62 wait_max_load=13.62 wait_mean_load=13.62 wait_readings=1 read_errors=0`

Official detached systemd runs survive session recycle. Per-job raw log/XML/BEP and exact commands/env/manifests retained, never deleted. `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c3/logs`; complete proof `/var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c3/FULL_SUITE_VERIFIED.json`, SHA-256 `02bc7f5ca3493a11deb5a6499e7d3930f0e668e6eeac8030bb490f5293c1a409`.

## Failure attribution (one raw FAIL retained)

The original integration-package lane exited nonzero; //internal/session:session_test FAILED while the other 25 configured targets passed. It reports Suspend against a dead server at manager_tmux_integration_test.go:39 (helper assertion117). The original C3 RESULT remains failed, and integration smoke ran for the first time as a separate official detached continuation on the same canonical tree. No earlier lane was relaunched and no failing test is relabeled PASS.

Clause1: failing function/site39 UNCHANGED, full per-file explain has no shared hunks, and file blobs match pinned main. Clause4: no same-package overlap; production edits are in cmd/gc and internal/overlay. Clause3(c): all five changed overlay functions measured 0.0% in the focused coverage probe; cmd/gc is absent from the actual integration test-binary import closure. Valid fixed regex initialization and pure private lists/maps perform no I/O and change no suspend inputs. The focused probe PASS is non-reproduction, not the proof used.

Tracker ga-qzxpqo was created and read back in this run after exact-name and condition searches found no existing tracker; it is not called pre-existing. The gm-sf3238 discovering-run escape applies because measured proof(c) landed and clauses1/4 are clear. Tracker stays OPEN/unrouted until a resolution lands. A future repeat must apply the repeat-hit rule. No waiver.

Coverage proof: /var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.attribution-coverage/COVERAGE_ATTRIBUTION_VERIFIED.json; ownership/source proof: /var/tmp/gc-heavy-gate/runs/ga-d68q4e-rekey-fresh.c3/failure-site-proof.json. Both are hashed in the full-suite proof. Raw first failure and sealed per-job log/XML/BEP are retained.

Original pipeline: status=failed key=ga-d68q4e-rekey-fresh-full-ci-sweep-9d83be635385d89a8f4a6ae01fc89a554d4fef1a-0403aaa6e4 units=0/1 failed=1. Smoke continuation: status=complete key=ga-d68q4e-first-smoke-9b55f6c4f5f2-4b4b3c54ef units=1/1 failed=0. ['LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.56 run_max_load=8.01 run_mean_load=7.85 run_readings=3 wait_first_load=7.56 wait_max_load=7.56 wait_mean_load=7.56 wait_readings=1 read_errors=0']

## Publication

Normal new isolated PR from fork. Public issue #7113. No merge and no deployment to the live city. City reaper purge-age override remains 876000h, and source contains a41b91a032 plus fail-closed 56d8e6aa47, independently verified before building. Separate city witness dead-hook correction ga-4fdh1f remains OPEN; coordinate that live-city update independently before installing the new lint rule.
