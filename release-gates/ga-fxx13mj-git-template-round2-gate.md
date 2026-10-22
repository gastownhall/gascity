# Git-template remedy, round two — ga-fxx13mj

**Verdict:** **PASS**

Initial publication/CI head: 7855aa5be2e51ba2a7c5a3fdf36d0e8b7814e6f1.
PR: https://github.com/gastownhall/gascity/pull/8068.
Initial exact-head CI verified at 2026-10-17T18:13:35.096557+00:00: every required check passed.

This final gate-record-only commit follows mayor disposition gm-wisp-k5bu801.
The completed Linux evidence carries over with the sole-file delta proof;
no local suite is rerun. After this commit, every required GitHub job must also
pass on its new exact head, Windows included, before readiness and clearance.
The PR stays draft and clearance stays absent until that condition holds.

Final-record delta from 7855aa5be2e51ba2a7c5a3fdf36d0e8b7814e6f1: only
release-gates/ga-fxx13mj-git-template-round2-gate.md. The exact resulting
commit SHA and live git diff --stat proof are recorded on ga-fxx13mj;
any other path invalidates this carryover and requires a full gate.

## Source and authority

- build_bead: ga-9keoubm
- original_build_bead: ga-buns9h
- review_bead: ga-tq3w74p
- gc.fixes_tracker: ga-m7ua7a
- Repository: gastownhall/gascity; deploy mode: remote; base ref: origin/main.
- Reviewed source: 36fc885e2e2b29b137de430e5d4b2a03fe55efdd; tree: 9e9eeec962e75c0123681556f3a886e5f524ed3f.
- Source branch rework/ga-9keoubm-2tk2s3t is provenance only, never a push target.
- Original pinned source base: 2075ebb7298411b726d30d6f5416a61bc7cbfc09.
- Pinned deploy base: a2c9a5a112dbf3933b972ec786f15e28094d32d5.
- Tested canonical merge: cecb01a73730a4c141d85df36f718babef4001d7; tree: ef34b97d9bc17eebec38165c3980b1a1d70af399.
- merge_state: clean (origin/main@a2c9a5a112dbf3933b972ec786f15e28094d32d5).
- gate_base_kind: current-tip. No source rebase, source fix or branch repoint.
- All 17 code/build paths retain their reviewed bytes after integration;
  TESTING.md also includes an upstream integration-smoke command clarification.
- Original author Jim Wordelman's commit is retained. Credits: original
  gascity/builder implementation and helper rework by gm-wisp-l080vim and
  gm-wisp-sbt1ako; round-two author gm-wisp-2tk2s3t. Original source
  388d1bc0dfab68d4b70b2b4998df61514c6b3574 remains in the verified bundle history.
- Independent actual Codex reviewer: gm-wisp-le99qp6; LOCAL PASS on the exact
  reviewed source, no blocking findings. E1 corrects historical T1/W1 subtest
  arithmetic to 1201; M18 remains accepted and nonblocking.
- Permission/runtime dispositions gm-wisp-dlu72tk and gm-wisp-wr5mkso apply
  to the retained author-local results. Original author make check EXIT2,
  280 PASS/3 FAIL targets and 15 named failures, is retained unchanged.
  Every named case has authorized corrected-runtime PASS on unchanged source;
  this is neither a waiver nor a claim the original invocation returned zero.
- Load disposition gm-wisp-nlikwzi supersedes the original 120-second defer
  bound for this gate only: every local phase uses threshold 15, max-wait
  1800 seconds, on-timeout run; observed load summaries are retained.

## Criteria

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Independent review | PASS: ga-tq3w74p covers exact source; verified publication-head delta is the gate record alone. Mayor gm-wisp-9t9hoi2 confirms no additional review scope; MPR follows clearance. |
| 2 | Acceptance | PASS: preserved source contracts, one verified PR with original credit, all required current-head CI green; merge and landing guards remain explicitly reserved. |
| 3 | Tests | PASS: fresh full-scope Linux evidence and every required initial publication-head CI result. Final-record-only Linux carryover is explicitly authorized; new exact-head CI must also pass before clearance. |
| 4 | High-severity findings | PASS: zero unresolved blocking/HIGH findings in independent source review. |
| 5 | Clean branch | PASS: clean tracked/untracked status verified at the publication head after normal pre-commit and pre-push checks. |
| 6 | Build, vet and merge state | PASS: canonical clean merge built //cmd/gc:gc_test and whole-tree make vet/nogo; C6_COMPLETE and detached complete1/1 failed0 retained. |
| 7 | Single feature theme | PASS: central verified test Git-template installation, its boundary propagation and census-neutral helper reuse. |

## Acceptance matrix

| Rework requirement | Evidence |
|---|---|
| Preserve trust, init, inheritance and testscript boundaries | Five core template files are byte-identical to original implementation; current contract tests and all owned tests PASS. |
| No census or policy increase | Original source-base census: all749/227, untagged500/152, Small471/137 calls/files; original independent physical BASE-ledger overlay whole resourcecensus113PASS/0FAIL/0SKIP. Current full unit lane also passes whole resourcecensus; census implementation and ledger files are outside this diff. |
| Fresh tests and helpers | All21 owned and all92 conservative helper consumers have actual same-package PASS, no FAIL or unrepaired subtest SKIP, in this deploy run. Real direct/proxied lifecycle runs in integration-packages; unit-tier skip retained. |
| Contract discrimination | Independent review audited genuine no-activation RED, 10 killed mutations and restored-source149PASS; historical author evidence is supporting evidence, not the current suite verdict. |
| Required build/policy/generated checks | Fresh build/vet/nogo, format, pinned lint, policy, BUILD sync and generated/OpenAPI checks PASS. |
| Resource and host discipline | Detached services, private evidence, pinned source/base and local load summaries; no host library/config/provider/schema/cache mutation or default tmux cleanup. |
| Provenance and one publication | Self-contained bundle verified; author history and immutable independent report retained; exactly one deploy/ga-fxx13mj-gate draft PR is authorized. |
| Landing guards | ga-9keoubm and ga-m7ua7a remain open; only mayor closes delivery after verified actual main landing. #7000/#6969/#7305 guards and ga-nova2a/ga-ow7y5vo/ga-kzu7mp4/ga-y7qavme conditions are not released here. |

## Linux test evidence

test_cmd_scope: full-suite

test_cmd: gate-criterion3.sh over the pinned canonical merge, running make check,
acceptance, integration-packages and **full integration without a smoke filter**,
then the mandatory native floor. All four Bazel phases use
--nocache_test_results and --noremote_local_fallback.

test_exec_mode: remote-exec; heavy_mode: none.
test_exec_bep: 490 first-attempt remote results; zero remote/local cached
results, other strategies or retries; every phase's start window and terminal
verdict predicate passes. farm_test_seconds: 7589.531.

| Phase | Bazel targets PASS | Forced remote results PASS |
|---|---:|---:|
| Unit / make check | 283 | 371 |
| Acceptance | 7 | 24 |
| Integration packages | 30 | 81 |
| Full integration | 1 | 14 shards |

test_counts: 116870 PASS / 0 FAIL / 370 SKIP
top-level result lines across retained logs, counting each executed job;
subtests are excluded. SKIPs remain SKIPs and are not promoted into this count.
diff_tests_executed: all21 PASS by name, including all descendants.
owned_topup: 0 additional repairs required.
floor_topup: all16 steps EXIT0, complete=true.
failure_attribution: none in the fresh Linux run; waiver_ref: none.
fallback_reason: none.
admission: remote, floor25GiB, cap2, admitted at
64.509GiB free; no remote load wait.
rbe_state: retained in c3-evidence/rbe-state.json, alongside exact worker-env
pins, certificate expiry and kill-switch state. Test counts and named outcomes
are retained in c3-evidence/execution.json and remote/by-name.txt.

policy_lane: PASS — pinned lint/format and shell guards; make vet/nogo;
11 uncached remote policy targets. drift_lane: PASS — make bazel-sync then
git diff --exit-code and empty tracked/untracked status in a fresh view;
6 uncached generated-artifact targets. OpenAPI fast lane: 1 uncached PASS.
Each lane used a fresh tree and the pinned gate base; suite source stayed clean.
ci_lane_run: n/a for criterion3c (no CI-config diff); actual PR jobs remain mandatory.
test_log_dir: /var/tmp/deploy-ga-fxx13mj.fresh.0fg4gsp_/c3-evidence/remote/logs.

### Native floor

| Package or check | Result |
|---|---|
| cmd/gc | EXIT 0 |
| examples/gastown | EXIT 0 |
| internal/beads | EXIT 0 |
| internal/beads/beadstest | EXIT 0 |
| internal/beads/contract | EXIT 0 |
| internal/beads/exec | EXIT 0 |
| internal/doctor | EXIT 0 |
| internal/mail/exec | EXIT 0 |
| internal/runtime/herdr | EXIT 0 |
| internal/runtime/runtimetest | EXIT 0 |
| internal/runtime/tmux | EXIT 0 |
| test/acceptance/helpers | EXIT 0 |
| test/integration | EXIT 0 |
| fsys-darwin-compile | EXIT 0 |
| push-gate-lock-selftest | EXIT 0 |
| local-concurrency-selftest | EXIT 0 |

The floor reruns every environment-absence SKIP by package, selecting parents
for skipped subtests; it always includes beadstest, Darwin compile and runner
self-tests. Remaining nonowned skips are inventoried with exact reasons:
fixture/opt-in omissions, platform constraints, intentionally unavailable test
providers, absent optional tools/services and fast-tier exclusions whose real
process-tier outcomes remain available. None of the changed helpers reaches
an unexecuted owned test; all92 reached consumers and all21 owned tests have
real PASS including subtests. The only farm skips in changed packages are three
unchanged acceptance-helper tooling tests; the native floor reruns them.

skip_inventory: /var/tmp/deploy-ga-fxx13mj.fresh.0fg4gsp_/c3-evidence/remote/skip-inventory.json (SHA256 737bfd365c0a8c0aa72c7eb415310d4dac09ff71cf22c1172cf9c5b2c289301a).
Qualified owned/helper proof: /var/tmp/deploy-ga-fxx13mj.fresh.0fg4gsp_/qualified-current-results.json (SHA256 ac172ff0662c0b487021b81e9a713a7bfd3c2de97ef1e77303813bcc8240b082).
Raw LOAD_GATE_SUMMARY records: fast-evidence lane output and
c3-evidence/remote/floor-topup.stdout under the evidence root below.

```text
policy/format: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=59.43 run_max_load=62.39 run_mean_load=58.59 run_readings=8 wait_first_load=81.21 wait_max_load=95.72 wait_mean_load=75.52 wait_readings=61 read_errors=0
native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=32.95 run_max_load=34.38 run_mean_load=32.74 run_readings=8 wait_first_load=77.08 wait_max_load=85.09 wait_mean_load=57.30 wait_readings=61 read_errors=0
```

### Owned tests

| Package | Test | Result |
|---|---|---|
| cmd/openapi-breaking | TestLoadBaseReadsSpecAtGitRef | PASS |
| internal/git | TestSanitizedAndHermeticEnvPreserveGitTemplateDir | PASS |
| internal/testenv | TestCheckTemplateEntry | PASS |
| internal/testenv | TestCommitInAFreshTestRepoSpawnsNoAutoMaintenance | PASS |
| internal/testenv | TestEnsureGitTemplateIsSafeUnderConcurrentCreation | PASS |
| internal/testenv | TestEnsureGitTemplateRefusesATemplateItDidNotWrite | PASS |
| internal/testenv | TestEnsureGitTemplateRefusesEntriesOthersCouldWrite | PASS |
| internal/testenv | TestEnsureGitTemplateRejectsAnUnusableRoot | PASS |
| internal/testenv | TestEnsureGitTemplateWritesExactlyTheTemplateOnceAndReusesIt | PASS |
| internal/testenv | TestGitReposCreatedByTestsCarryAutoMaintenanceOff | PASS |
| internal/testenv | TestInheritsGitTemplateOnlyForTheTemplateByAbsolutePath | PASS |
| internal/testenv | TestInitInstallsItsGitTemplateOverTheAmbientOne | PASS |
| internal/testenv | TestInitInstallsNoGitTemplateInTestscriptSubcommandMode | PASS |
| internal/testenv | TestInitKeepsTheGitTemplateItInherits | PASS |
| internal/testenv | TestInitRefusesAPlantedGitTemplate | PASS |
| internal/testenv | TestInstallGitTemplateRefusesAWinnerThatIsNotTheTemplate | PASS |
| internal/testenv | TestInstallGitTemplateYieldsToAValidWinner | PASS |
| scripts | TestCheckCoreBoundary | PASS |
| scripts | TestPushOwnershipGuard | PASS |
| scripts | TestRebaseResolveLib | PASS |
| test/acceptance/helpers | TestNewEnvForwardsGitTemplateDir | PASS |

## Required publication-head CI

Every row below has terminal SUCCESS and GitHub check-run head_sha equal to
7855aa5be2e51ba2a7c5a3fdf36d0e8b7814e6f1. Current workflow path filters and main rules were read live.
The Windows job ran on blacksmith-4vcpu-windows-2025 while the PR was draft;
both the credential-provider unit contract and Windows Job Object descendant
cleanup steps actually succeeded. Bazel includes unit, acceptance,
integration-packages and integration-smoke. All four required CodeQL analyses
succeeded. No failing, cancelled, timed-out or action-required check was present.

| Required job | Result | Actual evidence |
|---|---|---|
| Runner policy | SUCCESS | [job 117466292821](https://github.com/gastownhall/gascity/actions/runs/39056794406/job/117466292821) |
| Runner policy | SUCCESS | [job 117466292725](https://github.com/gastownhall/gascity/actions/runs/39056794317/job/117466292725) |
| Runner policy | SUCCESS | [job 117466292450](https://github.com/gastownhall/gascity/actions/runs/39056794330/job/117466292450) |
| Runner policy | SUCCESS | [job 117466292430](https://github.com/gastownhall/gascity/actions/runs/39056794329/job/117466292430) |
| Runner policy | SUCCESS | [job 117466292352](https://github.com/gastownhall/gascity/actions/runs/39056794287/job/117466292352) |
| Detect changes | SUCCESS | [job 117466346892](https://github.com/gastownhall/gascity/actions/runs/39056794406/job/117466346892) |
| Credential provider / Windows process tree | SUCCESS | [job 117466394404](https://github.com/gastownhall/gascity/actions/runs/39056794406/job/117466394404) |
| Check | SUCCESS | [job 117466678797](https://github.com/gastownhall/gascity/actions/runs/39056794406/job/117466678797) |
| CI / required | SUCCESS | [job 117466716867](https://github.com/gastownhall/gascity/actions/runs/39056794406/job/117466716867) |
| bazel / unit | SUCCESS | [job 117466334941](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466334941) |
| bazel / acceptance | SUCCESS | [job 117466335042](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466335042) |
| bazel / integration-packages | SUCCESS | [job 117466334876](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466334876) |
| bazel / integration-smoke | SUCCESS | [job 117466334936](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466334936) |
| bazel test (side-by-side) | SUCCESS | [job 117466943256](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466943256) |
| BUILD files in sync | SUCCESS | [job 117466334899](https://github.com/gastownhall/gascity/actions/runs/39056794297/job/117466334899) |
| Analyze (actions) | SUCCESS | [job 117466346665](https://github.com/gastownhall/gascity/actions/runs/39056794330/job/117466346665) |
| Analyze (go) | SUCCESS | [job 117466346608](https://github.com/gastownhall/gascity/actions/runs/39056794330/job/117466346608) |
| Analyze (javascript-typescript) | SUCCESS | [job 117466346621](https://github.com/gastownhall/gascity/actions/runs/39056794330/job/117466346621) |
| Analyze (python) | SUCCESS | [job 117466346634](https://github.com/gastownhall/gascity/actions/runs/39056794330/job/117466346634) |

Full exact-head proof: /var/tmp/deploy-ga-fxx13mj.fresh.0fg4gsp_/required-ci-pass.json
(SHA256 6e7e4c2c6d3b75ffe07c839414c77c2cd64e8b5e0ee22fb356df21cf2492709e).

The publication head retains the exact independently reviewed source in its
ancestry and its net delta adds only the gate record. Mayor clarification gm-wisp-9t9hoi2
confirms this satisfies source review; the standard GitHub review is MPR's after
clearance. The normal pre-commit Bazel docsync target actually passed; the
normal pre-push passed all 283 targets (246 executed, remaining cached), a
supplemental hook result separate from the 490 forced Linux gate results.

Next authorized release sequence: ready, exact-head deploy clearance, verified
merge-request to mayor, then MPR review/merge. Mayor gm-wisp-k5bu801 authorizes this one final gate-record-only seam with
Linux carryover and fresh exact-head CI. Every other head move requires a new
gate; no source update is silently covered by this verdict. Actual main
landing and delivery/tracker/landing-guard closure remain the mayor's work.

## Immutable provenance and retained evidence

- Evidence root: /var/tmp/deploy-ga-fxx13mj.fresh.0fg4gsp_.
- Bundle: /var/tmp/ga-9keoubm-intake.NJwO5L/evidence/remedy.bundle;
  SHA256 797baa93e9a38d8472b8e70a5a904fee4194d57098f0ed3748ed8651d51814d8.
- Independent review: /var/tmp/ga-tq3w74p-review.fre5Fc/review-report.txt;
  SHA256 b548cab438ae60cb1320b5ab21d2d43cd847e7ab1d116dba216fb32c508eab1b.
- Review manifest: /var/tmp/ga-tq3w74p-review.fre5Fc/SHA256SUMS;
  SHA256 db066a9ed329b99af91c89c6247d4362aa396d36456a1e1fee5776e683b70cd9.
- Current author report/manifest use the verified hash amendment:
  4aecac6bb702656bbe862e7651a9e5f473cdb53c0bd04f82cd7bd31858461524 and
  29c14963643cce83ae039a24b40ab8beac0da4f36232b11019423943bab643d4.
  Old report hashes remain historical coordinates, not silently rewritten.
