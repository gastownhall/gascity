**Verdict:** **PASS**

# Test runner pane shell release gate

- Bead: ga-tnljpp; build ga-yghhjf; review ga-x4cprv.
- Evaluated: 2026-10-07T08:25:18.887867+00:00.
- Reviewed source: `cbc219712dc464f9ca953c89f706eb00380bfcc3`; isolated branch `deploy/ga-tnljpp-gate`; fork `quad341/gascity`.
- Local gate base: `3130911080d8470ce68720a47475e5f6562ef165`; materialized merge `d7d9a4148bc9c96d6be505a210a4eefbbd6e461d`; tree `190fd74b233b4b92b50567f8ca7839701371d194`.
- Draft CI base: `9373e5a8587694891d31255976ec08299924f1ac`; PR merge `5f7c545ac328e5aeea1fd86801bc7c41926cbce7`; CI workflow blob `9c1c80b5372951709c6d7af25a138144d0908df7`.
- Final main merge check: `973ced041aeaeabc3f8fb578cf4a590a84d0949a`; clean merge tree `978ba653ab67bf045bbc7ca9e58949ffadfec1d8`.
- PR: https://github.com/gastownhall/gascity/pull/7283. This record adds only gate evidence to the reviewed source. Publication-head CI must be green before the merge handoff.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-x4cprv passed the exact resolved reviewed source; no carryover or rebase. |
| 2 | Acceptance criteria | PASS | All four runner allowlists pin SHELL=/bin/sh once; no forwarding. New guard and all four subtests pass. Independent fresh-HOME RED reproduces the wizard; GREEN passes. Fifth acceptance helper reaches only explicit commands. |
| 3 | Required tests and policy | PASS | Full local Bazel suite 213/213 targets; owned guard PASS. Fast policy and every generated drift check PASS. Draft CI required and all four Bazel tiers success at the exact reviewed head; 15 required leaf jobs accounted below. |
| 4 | High-severity findings | PASS | Zero unresolved HIGH/blocker findings in the passed review. |
| 5 | Clean source branch | PASS | git status --porcelain was empty before this gate record was written. A clean status after committing this record is required before push. |
| 6 | Main merge | PASS | Original pinned materialized merge built and vetted. Current origin/main merges without conflicts; merge-tree returned 0. No self-rebase or force push. |
| 7 | One feature theme | PASS | Seven source files implement one runner pane-shell invariant; both source commits belong to ga-yghhjf. No stack or denied internal path. |

## Full-suite evidence

```text
test_cmd: make test "BAZEL=bazel --batch" "BAZEL_FLAGS=--config=ci --config=fork-cache --jobs=4 --test_env=LD_LIBRARY_PATH=/var/tmp/gc-heavy-gate/runs/ga-aqrxki.generated/icu74 --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-tnljpp.bazel-unit-runtime/unit.bep.json"
test_cmd_scope: full-suite
bazel_test_targets: 213 PASS, 0 FAIL
test_counts: 52624 PASS, 0 FAIL, 215 SKIP (column-zero result lines)
parent_name_result_counts: 28966 PASS, 0 FAIL, 166 SKIP (names without a slash)
diff_tests_executed: TestRunnerTestEnvsPinPaneShell PASS; Makefile, test-local-parallel, test-go-test-shard, test-integration-shard subtests PASS
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-tnljpp.bazel-unit-runtime/logs
heavy_mode: none
ci_lane_run: n/a (this diff changes no CI job, matrix, timeout or required-check list)
waiver_ref: none
failure_attribution: none (the completed gate has no failures)
```

The documented command ran the complete default Bazel tree without a test filter. The driver reports test_rc=0, artifact_rc=0 and GATE_RUN_EXIT rc=0 state=complete. All 271 testResult logs and XML were retained with Bazel labels, shard identities and SHA-256 checksums in unit-artifacts.json. The owned guard is in //scripts:scripts_test shard 5 of 6. rules_go emits some subtest result lines at column zero; the first tally is therefore a result-line count, not a distinct top-level census. Raw framing bytes are retained alongside normalized copies.

## Skip evidence

Every one of the 215 named skip events has its printed reason and source site in /var/tmp/gc-heavy-gate/runs/ga-tnljpp.bazel-unit-runtime/unit-skip-audit.json. All 215 sites are UNCHANGED; zero unresolved sites; no owned test skipped. /var/tmp/ga-tnljpp-owned-explain.txt records both candidate files. The shared hunk in cmd/gc/session_event_pump_live_test.go is a comment only and cannot reach its unchanged missing-herdr skip. The regexp import and regex globals in scripts/git_test_env_test.go are used only by the new guard; no existing body or skip path changed, and no skip in that file was observed. All other skip files have no shared hunk.

Unit-lane process exclusions are exercised by the successful required Bazel integration-packages and integration-smoke runs. Remaining platform, optional-tool/provider, fixture and characterization skips are unchanged and do not test the shell-pin guard. Their individual reasons and unchanged-site proofs are retained; no aggregate green result is used to excuse the owned guard.

## Fast lane and runtime

- Build and vet: fresh-view go build ./... and go vet ./... PASS (ga-tnljpp.fast-envfixed).
- Policy/lint: pinned-base lint-affected, fmt-check-changed, test-ci-policy, check-gomod-replace, check-native-dependency-surface, check-eventexport-isolation, check-core-boundary and check-docs PASS; full nogo build PASS.
- Drift: make bazel-sync plus git diff --exit-code; make dashboard-ci plus dashboard diff; all three Bazel generator checks; OpenAPI breaking-change check PASS. Each used its own fresh-tree view.
- Fast run: /var/tmp/gc-heavy-gate/runs/ga-tnljpp.fast-cachefixed; GATE_RUN_EXIT rc=0 state=complete.

Fresh-view creation records (build/vet, interrupted lint, then corrected policy, Bazel drift, dashboard drift, codegen drift and breaking-change lanes in execution order):

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.2cCsOu modified=0 untracked=0 ignored=0 ignored_names=-
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.vJdQbU modified=0 untracked=0 ignored=0 ignored_names=-
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.tzqE0u modified=0 untracked=0 ignored=1 ignored_names=.github/workflows/scripts/__pycache__/
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.kurFbL modified=0 untracked=0 ignored=1 ignored_names=.github/workflows/scripts/__pycache__/
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.h6TkJt modified=0 untracked=0 ignored=1 ignored_names=.github/workflows/scripts/__pycache__/
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.Q5MKUo modified=0 untracked=0 ignored=1 ignored_names=.github/workflows/scripts/__pycache__/
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-tnljpp.AXCPcV head=d7d9a4148bc9 tree=190fd74b233b dir=/var/tmp/gc-fresh-tree.BEFzYQ modified=0 untracked=0 ignored=0 ignored_names=-
```

The first corrected fast views record only closure-reader Python bytecode as an ignored source path. That exact path was preserved with a scoped Git stash before the suite; subsequent reads disable bytecode generation. No generated or tracked input was carried between lanes.

The prescribed isolation wrapper was used; its pass-through notice identifies the already-private merge worktree and absent ambient BD_/BEADS_/GC_/DOLT_ names. Rootless podman.socket was verified active and the documented runtime environment supplied. The host ICU 77 does not satisfy the Bazel-built native binary’s ICU 74 ABI. A UID-preserving bubblewrap mount namespace supplies only the compatibility sonames in a private /usr/lib64 view; host library directories and loader cache are unchanged. An empty-environment TestInitScrubsLeakVectors probe passed. Bazel batch mode ensures child processes inherit the private runtime. The documented /usr/local/go symlink points to the existing Go 1.26.6 toolchain.

The earlier source-view hook run failed (198/203 target success), and all its 254 logs remain at ga-tnljpp.draft-push-identity. No waiver or attributed PASS was asserted for that run. The completed gate and normal active hook used the pinned materialized merge with the repaired private runtime; all 47 previously failing names report PASS there. The normal hook passed 213/213 and published only the reviewed deploy ref, with push_rc=0/artifact_rc=0. No hook bypass was used.

## Load measurements

```text
canonical suite: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=56.38 run_max_load=54.10 run_mean_load=33.75 run_readings=32 wait_first_load=27.71 wait_max_load=68.68 wait_mean_load=44.82 wait_readings=61 read_errors=0
RED pane probe: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1143 wait_timed_out=0 run_start_load=14.67 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=26.77 wait_max_load=49.10 wait_mean_load=28.30 wait_readings=39 read_errors=0
GREEN pane probe: LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.73 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=16.00 wait_max_load=16.13 wait_mean_load=15.47 wait_readings=6 read_errors=0
normal push: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1351 wait_timed_out=0 run_start_load=14.98 run_max_load=42.37 run_mean_load=31.29 run_readings=15 wait_first_load=27.72 wait_max_load=48.62 wait_mean_load=26.50 wait_readings=46 read_errors=0
```

NA run extrema on the short pane probes mean they finished before the sampling interval. Ordinary-path suite load wait expired as recorded, then the full suite passed.

## Independent acceptance probe

scripts/test-go-test-shard ./internal/runtime/tmux 1 1 ran a one-name integration manifest with initially empty HOME and SHELL=/usr/bin/zsh; /bin/zsh resolves to the same binary. At the recorded pre-fix RED commit, TestHiddenAttachedClientCanSendText failed with zsh-newuser-install and ELLO_HIDDEN_ATTACH. At the pinned materialized merge it passed (0.64s), no wizard. RED/ GREEN logs are retained under ga-tnljpp.pane-regression-runtime; overall driver rc=0. test_cmd_scope: focused-acceptance-probe, supplemental to the full suite. The five direct acceptance tmux starts pass sleep/printf commands; the provider fallback also supplies an explicit command, so its SHELL passthrough reaches no interactive default shell.

## Required CI accounting

Authorized exact-source draft: mayor mail gm-wisp-5gdtzgi. The actual CI run is https://github.com/gastownhall/gascity/actions/runs/37590583645, head cbc219712dc464f9ca953c89f706eb00380bfcc3, conclusion success. The closure tool ran on the fetched PR merge and its workflow blob above, comparing the exact base to reviewed source with fork semantics: 15 LEAF jobs, 2 rollups. The earlier local-base closure of 21 leaves is retained as historical evidence and superseded by this observed draft workflow. Main’s later topology-to-Bazel migration changes the publication workflow; CI on the final publication head is checked before readiness and handoff.

| Job | Accounting | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Actual pull_request/quad341 runner_policy.py rc=0; outputs and log /var/tmp/ga-tnljpp-runner-policy-pr.* |
| changes | LOCAL-PASS | ci_required_triggers.py on the recorded fetched PR merge/base/source, fork; rc=0; /var/tmp/ga-tnljpp-ci-required-pr-closure.txt |
| preflight-static | CI-PASS | [Preflight / static checks](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112690932262) — success at reviewed head |
| preflight-acceptance | CI-PASS | [Preflight / acceptance A](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112690932251) — success at reviewed head |
| preflight-generated | CI-PASS | [Preflight / generated artifacts](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112690932250) — success at reviewed head |
| release-config | CI-PASS | [Release config](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112690932454) — success at reviewed head |
| integration-rest-full | PUSH-ONLY | integration-rest-full                  LEAF       linux                      PUSH-ONLY (never runs on a pull request) |
| beads-topology-acceptance | CI-PASS | [Beads / topology acceptance](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112691024133) — success at reviewed head |
| beads-proxied-native-acceptance | CI-PASS | [Beads / proxied-native acceptance](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112691024132) — success at reviewed head |
| credential-provider-windows | DEFERRED-TO-PR-CI | reason=non-linux-runner; recomputed Windows closure has no changed inputs; reach stdout empty, rc=0. Draft Windows job also observed success; required again on publication head. |
| preflight-unit-cover-noncmdgc | PUSH-ONLY | preflight-unit-cover-noncmdgc            LEAF       linux                      PUSH-ONLY (never runs on a pull request) |
| preflight-unit-cover-cmdgc | PUSH-ONLY | preflight-unit-cover-cmdgc               LEAF       linux                      PUSH-ONLY (never runs on a pull request) |
| pack-gate | CI-PASS | [Pack compatibility gate](https://github.com/gastownhall/gascity/actions/runs/37590583645/job/112691024167) — success at reviewed head |
| k8s-session | NOT-COVERED | K8s session tests step was skipped in the actual CI run; source has no Kubernetes change (ga-1zgega §3.4). |
| openclaw-bridge | NOT-TRIGGERED | openclaw-bridge                          LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.openclaw_bridge == 'true' |

deferred_to_pr_ci: credential-provider-windows (non-linux-runner). windows_reach: none. not_covered: k8s-session. No Linux host-gap deferral is used.

## Primary Bazel CI tiers

| Job | Result | Actual run |
|---|---|---|
| BUILD files in sync | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112690925971) |
| bazel / acceptance | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112690926041) |
| bazel / unit | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112690926071) |
| bazel / integration-packages | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112690926082) |
| bazel / integration-smoke | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112690926246) |
| bazel test (side-by-side) | PASS | [job](https://github.com/gastownhall/gascity/actions/runs/37590583555/job/112693157228) |

The four actual bazel test steps, including full //..., executed successfully. PUSH-only coverage was skipped by its own event condition.

ga-1wilql remains OPEN until this pin lands on main; opening or readying the PR does not clear that tracker. Merge belongs to MPR, routed through mayor.
