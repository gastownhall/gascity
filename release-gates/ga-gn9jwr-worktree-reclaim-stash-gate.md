# Worktree reclaim stash release gate

**Verdict:** **PASS**

Bead: `ga-gn9jwr`. Recorded 2026-10-08T13:56:45.402371+00:00.

Allows eligible daemon worktrees to be reclaimed while a stash exists in the repository. Reclaim leaves the shared stash refs and stashed contents available.

## Source and gate tree

- Reviewed source: `47769f2a9468cf8f5117d77434199932bddf8b65`; reviewer `ga-v7hklq` PASS; build `ga-qlcqru`.
- Pinned main: `7e91a156e3ccce0339d03494e8626298e51c42d2`.
- Canonical normal-hook merge: `bb8758b395727fe75b94a119522e4d31528b5986`; tree `c5ad0c953b1f0660d722d97c6be40de2a8f668d8`, equal to the predicted conflict-free merge. Parents are pinned base then exact reviewed source.
- Tests ran in `/var/tmp/gc-merge-ga-gn9jwr-rekey-fresh.z1V8N4`; independent fresh views ran build/vet, fast checks and extra CI accounting.
- Publication branch: `deploy/ga-gn9jwr-gate`, cut exactly at reviewed source. This record is its only addition.
- Local already-landed preflight: reviewed source is not an ancestor of pinned main; both RED/GREEN patches are absent by git cherry. Existing PR-state verification belongs to the mayor under `ga-dwlg08`; deployer did not read or touch PR #4732.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Single-pass reviewer ga-v7hklq recorded PASS at the exact resolved source. No rebase, branch-tip substitution or review carryover. |
| 2 | PASS | Real Git tests prove an unrelated stash does not block reaper/raw prune and a stash created in the pruned worktree survives removal with recoverable content. Both prune paths drop the stash probe. Uncommitted, unpushed, unreachable, path and liveness guards remain covered. Config prose and generated reference/schema match. |
| 3 | PASS | Whole-repository unit, acceptance including solo fixtures, integration packages and integration smoke completed as uncached first attempts. All four owned roots and every surviving consumer in the three changed test files PASS in unit and integration. All required CI leaves accounted below; Windows is explicitly deferred under ga-1zgega. |
| 4 | PASS | No unresolved HIGH or blocking review finding. Minor comment wording is non-blocking. Doctor follow-up ga-005o3m is separate; the reviewer corrected earlier shared-account attribution, so no operator ruling is inferred from that GitHub account. |
| 5 | PASS | Canonical and publication checkout clean before this record; generated/BUILD checks produced no tracked or untracked drift. Final committed checkout cleanliness is verified before publication. |
| 6 | PASS | Canonical merge retains normal hooks and matches git merge-tree. Independent fresh-view go build ./... and go vet ./... both exit0. No self-rebase. |
| 7 | PASS | One feature: daemon worktree reclaim with repository-global stashes. Two source commits cite ga-qlcqru. Production/tests and matching generated config prose are in scope; no internal agent config paths or unrelated theme. |

## Full-suite evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: none`; `policy_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`.

Required Bazel commands, with `--config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs` and the recorded pinned runtime environment:

```text
make check BAZEL="bazel --batch" BAZEL_FLAGS="<flags above + recorded runtime env and BEP>"
bazel --batch test <flags> --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel --batch test <flags> --config=integration --keep_going //test:integration_packages
bazel --batch test <flags> --config=integration-smoke --keep_going //test/integration:integration_test
```

| Lane | Targets | PASS | FAIL | SKIP |
|---|---|---|---|---|
| unit | 232 | 53190 | 0 | 214 |
| acceptance | 7 | 410 | 0 | 13 |
| integration-packages | 26 | 34972 | 0 | 53 |
| integration-smoke | 1 | 16 | 0 | 1 |

Counts include subtests and repeats across CI lanes; top-level counts are retained separately. Totals: `{"all": {"FAIL": 0, "PASS": 88588, "SKIP": 281}, "top_level": {"FAIL": 0, "PASS": 47745, "SKIP": 218}}`; Bazel executions: 383.

Owned tests (actual unit and integration PASS):

- `TestPruneAgentHomeWorktreeIfSafeInfo_UnpushedProbeError`: PASS.
- `TestPruneAgentHomeWorktreeIfSafe_StashInsideWorktreeSurvivesPrune`: PASS.
- `TestPruneAgentHomeWorktreeIfSafe_UnrelatedStashDoesNotBlock`: PASS.
- `TestReapClosedBeadWorktrees_UnrelatedStashDoesNotBlock`: PASS.

All 43 surviving roots in the three changed test files have named unit/integration PASS: `/var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.c3/changed-test-consumers-pass.json`. This covers consumers of the changed shared fake-probe fields/method. No skip in those files is excused. Tutorial and pool process scenarios actually PASS with GC_FAST_UNIT=0 in the integration lane. Generated TestSchemaFreshness actually PASSed.

## Skip integrity

Every skipped site, its printed reason, complete per-file ownership explanation, shared-hunk result, source guard and full test-binary reach are retained in `/var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.c3/full-skip-site-proof.json` (SHA-256 `85b9be881b750f2eb4083c52c4a89a88725938530ebd710038da654fa01e4f25`). Every skipped site is UNCHANGED and outside an owned test. No changed test file has a skipped result.

Config AST proof: /var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.config-ast/proof.json records an independently compared identical non-comment AST; changed hunks contain no Go directive. Generated descriptions are checked separately. Runtime changes only remove stash checks from the reaper/prune paths after their normal eligibility checks. Config edits are comments; generated schema/reference edits carry those comments. They do not alter platform checks, opt-in selectors, helper-entry flags, fixed provider errors/capabilities, tool availability or early skip guards. Complete default/integration/acceptance import closures acknowledge internal/config and cmd/gc reach; absence of Go imports is not used to rule out CLI subprocess reach. Each actual guard was read individually. Same-name/package process omissions have fresh integration PASS; required live pack guards have the separately executed CI leaf evidence. Parents passing do not certify skipped children.

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

`ci_required_closure`: actual ruled tool at the canonical merged tree, source/base pins above, head location `origin` (mayor updates existing PR); merged ci.yml blob `7c1953ee32ea7c17a01fe1c19ecf133642a7ff4c`; 4 LEAF jobs. AGGREGATORs only roll up. Accounting follows architect ruling `ga-1zgega`; current workflow, not its historical job list, is authoritative.

| Leaf | Accounting | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Actual script with EVENT_NAME=pull_request, PR_AUTHOR=quad341, empty FORCE_BLACKSMITH; exit0; runner-policy stdout/stderr retained. |
| changes | LOCAL-PASS | ci_required_triggers.py actual source/base three-dot diff, current matcher and merged workflow; exit0, full/shared filter observed. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Both real GOOS=windows build closures computed; windows_reach none. No Windows test execution or LOCAL-PASS claimed. |
| pack-gate | LOCAL-PASS; exact job pack pin check and live registry make target, exit0 | Job commands/probe/env/stdout/stderr retained; live catalog and import roots actually PASS when local. |

`windows_reach: none`; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner)`; `not_covered: none`.

Accounting proof: `/var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.ci-accounting/ACCOUNTING_VERIFIED.json`, SHA-256 `204c783c91db9bfeb12f650cf2567678e1078126cbd42ae37eea6ac50ee523fb`. Exact argv/exit/status and stdout/stderr paths live in that proof. The live target uses verbosity-only GOFLAGS=-v; its CI target, name filters and tags are unchanged.

The existing PR may merge only when CI / required is green on its exact gated head and the deferred Windows job succeeds. This gate permits publication/handoff; it does not certify Windows runtime behavior or merge readiness.

## Fast lane and runtime

Complete whole-repository nogo, shell/policy/hooks, CI policy and 18 exact required policy/generated/OpenAPI targets PASSed before C3, each mutation lane in a fresh view under gate-base.sh. make bazel-sync plus git diff --exit-code and empty untracked-inclusive status PASSed. Fast proof `/var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.fast-rest/COMPLETE_FAST_VERIFIED.json`, SHA-256 `96067adb6d9a0e20f2420c0de2cdf4f69eb9cd15f79d178c5c9200ddccb096f3`.

OpenAPI form: workflow-equivalent per ga-3rtlng; standard form not run (ga-twhw87 open). E1-E6: tracker open and merged rule still rctx.symlink(spec, "openapi.json"); real pinned-base spec under the run directory outside /tmp, equal to git-object bytes; raw log shows pinned base, TestCommittedSpecAgainstBase PASS, TestGateAgainstOasdiff PASS and six breaking fixtures; teeth proof ga-2egszb and /var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro. The exception sunsets when the tracker closes or that rule changes. Base SHA-256 `04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912`.

Podman socket, pinned cached Dolt2.2.0 image and enabled testcontainer sweep verified before C3. Ryuk-disabled and BEADS_ALLOW_UNREAPED_TESTCONTAINERS stay paired. Test bd v1.3.1 and private ICU runtime overlay supplied. isolated-test-run.sh explicitly reported PASS-THROUGH / NOT ISOLATED; fresh worktrees, detached/Bazel sanitized environments and test-owned fixtures supply isolation.

`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.47 run_max_load=16.14 run_mean_load=12.56 run_readings=61 wait_first_load=12.47 wait_max_load=12.47 wait_mean_load=12.47 wait_readings=1 read_errors=0`

Official detached systemd runner preserves the suite across session recycles. Raw per-job log/XML/BEP and normalized named results are retained, never deleted. `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.c3/logs`. Complete proof `/var/tmp/gc-heavy-gate/runs/ga-gn9jwr-rekey-fresh.c3/FULL_SUITE_VERIFIED.json`, SHA-256 `cd206633323b39a31b5837d71d5c08ed773130c0f7e6b61c1ea0df1b076357b4`.

## Publication disposition

ga-dwlg08 authorizes only deploy/ga-gn9jwr-gate isolated publication. No new PR, no read/action on #4732, no contributor contact, no deployment to the live city, no merge. Mail mayor gated ga-gn9jwr plus the exact committed gate head. Mayor alone performs the lease-pinned existing PR update and verifies that head. Only after the mayor mails #4732 head = <exact gated head>, verified by the mayor, may deployer POST and GET-verify success clearance in gastownhall/gascity on that exact commit, then close blocked pending MPR. No clearance is posted before that mail. Doctor follow-up ga-005o3m remains separate; PR #4619 remains outside this deploy.

Supplemental publication freshness: `{"base": "e39d98ccfa23364258af6e26abe1f673d6c06bfd", "limit": "supplemental conflict-free text merge only; full-suite evidence remains pinned to 7e91a156e3ccce0339d03494e8626298e51c42d2", "stderr": "/var/tmp/ga-gn9jwr-rekey-fresh-publication-merge-tree.stderr", "stdout": "/var/tmp/ga-gn9jwr-rekey-fresh-publication-merge-tree.stdout", "text_merge_rc": 0, "tree": "3226690df0d5220b245311a2b4d7ada2bf03781d"}`. Full-suite evidence stays pinned to the base above.
