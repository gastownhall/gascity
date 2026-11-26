# Fresh worker anchor handoff release gate

**Verdict:** **PASS**

Date: 2026-11-25; deploy bead: ga-djt1xiqf; build: ga-70takxcc; review: ga-7h8d0mc8.
Reviewed commit: `0aa9b0e6413aa2468452222f81cd02df944d426e` (resolved in this repository).
DEPLOY_MODE: remote; PUSH_REMOTE: fork; BASE_REF: origin/main.
gate_base: `887a253144d46bb177f977749eb6869b12e4aec4`; gate_head: `0aa9b0e6413aa2468452222f81cd02df944d426e`.
Tested merge commit: `6f846cc9f93b06f6a535e2bab0ff7ea646a4d550`; tested tree: `541f9736317bcad6b47bfb6a9b5246867afea7e0`.
Evidence root: `/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo`. These tests cover the merged source tree; the deploy commit adds this record only.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-7h8d0mc8 round 2 records PASS for the resolved reviewed commit; no carryover or reviewer-added-test substitution. |
| 2 | Acceptance criteria met | PASS | A1–A7 verified below; public explanation references shared issue #9320 and identifies legacy-only scope with v2 pending. |
| 3 | Tests pass | PASS | Canonical five-phase full-suite run and all 16 native floor jobs completed; all six diff-owned tests PASS by name; SKIPs verified below; required CI closure accounted. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer round-2 verdict has no unresolved HIGH finding; earlier trimming and complexity findings have tests and zero-growth evidence. |
| 5 | Final branch clean | PASS | Functional checkout and merge scratch clean before this record; this record is committed separately. A clean final status is required again before push. |
| 6 | Merge state and build/vet | PASS | Clean scratch merge into the pinned base; Bazel build+nogo/vet completed with BEP overallSuccess=true and SUCCESS. |
| 7 | Single feature theme | PASS | Five source commits implement and test the legacy fresh-cycle live-anchor ownership guard, including its owning lifecycle invariant. Eight source files; no independent feature. |

merge_state: clean (origin/main@887a253144d46bb177f977749eb6869b12e4aec4)
gate_base_kind: current-tip
merge_validation: merged result was built and tested; merge authority must validate the exact PR head and current merge result at merge time.
preflight: no PR associated with the reviewed commit; no existing deploy branch. Source ancestry scope check passed for this deploy/build/review lineage; no stack or denied internal configuration paths.

## Acceptance evidence

- A1: deterministic reconciler regression stages an assigned/open snapshot, closes the previous bead during this incarnation, removes live claims, then releases the backing anchor. The released row preserves runtime, session key, current bead, claim stamp and reset metadata.
- A2: reassigned and blank anchors retain the worker; genuine owned anchors cycle. Named, pooled, stable-alias and whitespace controls PASS; transient pool slots, alias history and pool templates retain.
- A3: primed-cache regression follows exactly one authoritative live Get for status and assignee; changing the backing owner after that Get detects split reads.
- A4: legacy read-error fall-through is pinned. The preserved guard family has 58 named PASS results and no FAIL/SKIP/TIMEOUT; Q1–Q3, busy and restart protections remain intact.
- A5: existing fake runtime, clock and store seams; no new real process, sleep or fleet kill in the new regression.
- A6: full unit/nogo/generated/policy, acceptance, integration-packages and full integration/latency legs passed. BUILD registration and fresh `make bazel-sync` drift check passed. Owning `internal/worker/AGENTS.md` documents the invariant. Active `.githooks` verified with `make check-hooks`; the gate commit runs pre-commit.
- A7: complexity form(ii), base and merge check rc2 with 270 measured rows each, no merge row worse. Pinned-base complexity-diff prints only improvement `reconcileSessionBeadsTracedWithNamedDemand` 640→639. No new/regressed/deleted living function; complexity-baseline.json unchanged. Raw base/merge checks and proof retained under the evidence root.

## Tests and execution

test_cmd: canonical `gate-criterion3.sh` on the exact merged tree; remote `make -k check`/Bazel unit, acceptance, integration-packages, full integration excluding budget tests, then dedicated integration-latency; exact per-phase commands retained in execution.json and phase stdout.
test_cmd_scope: full-suite
test_log_dir: /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/criterion3/evidence/remote/logs (retained; includes floor logs)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
ci_lane_run: n/a (no CI job/matrix/timeout/required-check change)
policy_lane: PASS — fresh pinned-base view, make lint (Bazel nogo), go_format_test, diff whitespace check and all make-check shell guards.
drift_lane: PASS — independent fresh view; make bazel-sync, git diff --exit-code and empty untracked status.

```json
{
  "test_exec_mode": "remote-exec",
  "test_cmd_scope": "full-suite",
  "test_counts": {
    "PASS": 159330,
    "FAIL": 0,
    "SKIP": 618
  },
  "test_exec_bep": {
    "unit": {
      "results": 427,
      "remote_executed": 427,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 25,
      "remote_executed": 25,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 87,
      "remote_executed": 87,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 15,
      "remote_executed": 15,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-latency": {
      "results": 1,
      "remote_executed": 1,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    }
  },
  "farm_test_seconds": 14612.061,
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 53.312957763671875,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72",
    "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"
  },
  "heavy_mode": "none",
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "log_dir": "/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/criterion3/evidence/remote/floor-logs"
  },
  "latency_leg": {
    "verdict": "measured",
    "verdicts": {
      "TestGcBdLatencyBudget": {
        "PASS": 1
      },
      "TestGcSeatHookLatencyBudget": {
        "PASS": 1
      }
    },
    "probe": [
      "GATE_FARM_PROBE waiting waited=0s load1=93.81 load5=98.85 load15=53.24 cpus=32 threshold=50 max_wait=600s",
      "GATE_FARM_PROBE verdict=clear waited=40s load1=48.11 load5=86.46 load15=50.99 cpus=32 threshold=50 max_wait=600s",
      "GATE_FARM_PROBE window readings=10 max_load1=48.11 over_threshold=0 threshold=50"
    ]
  },
  "skip_inventory": {
    "farm_rows": 604,
    "native_rows": 72
  },
  "delta_gate": {
    "mode": "shadow",
    "decision": "full",
    "reason": "not-gate-pr",
    "applied": false
  },
  "native_baseline_admission": {
    "class": "local-heavy",
    "floor_gib": 40.0,
    "free_gib": 52.47754669189453,
    "waited_seconds": 0.001,
    "cap": 3,
    "prewait_s": 420.133
  }
}
```

diff_tests_executed:
  TestFreshAnchorBeadTerminal_BypassesPrimedCacheOnExternalClose PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-912ab643330b35bd0b6b.log)
  TestFreshAnchorCycleSkippable PASS (integration-packages-bc482d2ae768251ad47f.log, unit-4d20b3d28497f844aa39.log)
  TestFreshCycleRepro_AnchorOwnershipUsesOneLiveRowPastPrimedCache PASS (integration-packages-75f7330b9b673df65e4f.log, unit-23e7d9fc763496dd8095.log)
  TestFreshCycleRepro_LiveAnchorOwnershipControlsCycle PASS (integration-packages-568812e4d525bdd1f974.log, unit-f679cd2551fcc64525aa.log)
  TestFreshCycleRepro_LiveAnchorReadErrorStillCycles PASS (integration-packages-cb110414c557c0928e82.log, unit-2bde55fab377f50b4739.log)
  TestOwnerReadsReadPastAStaleCacheThroughAHandle PASS (integration-packages-830685c60d0c2dd88567.log, unit-fd986cf5bab8709b8d45.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name


skip_justification: no diff-owned SKIP. Farm inventory has 319 unchanged source sites plus three unchanged conformance helper sites resolving 30 ambiguous rows. Native inventory has 45 unchanged source sites and no unresolved sites. Shared helpers are in cmd/gc; module import closures were computed under unit/integration tags for all 227 roots, with no unknown root. Other packages cannot import this changed main package. Every skipped test whose binary can reach those shared helpers was compared freshly at the pinned base using the corresponding command. The native floor executed the environment-absence cases; remaining skips describe unchanged platform/capability, opt-in live-service, fixture/projection and harness conditions. All skip reasons, source proofs and import reach are retained.

```text
/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/skip-baselines/group-0-compare/output.txt
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a00, head rbe-a06)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/skip-baselines/group-1-compare/output.txt
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/skip-baselines/group-2-compare/output.txt
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
/var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/native-baseline-compare.stdout
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
```

## Required CI job accounting

ci_required_closure: python3 packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.K5wbwN 887a253144d46bb177f977749eb6869b12e4aec4 0aa9b0e6413aa2468452222f81cd02df944d426e fork; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs.
Runner expressions resolve through runner-policy outputs: Linux cross-compile lane and Windows native lane. Cross-compile commands ran sequentially in a fresh view through load gate and isolation wrapper; their real terminal status is rc0.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/ci-required/closure.stdout |
| runner-policy | LOCAL-PASS | /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/ci-required/runner-policy.stdout |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/ci-required/native-windows-reach.json |
| windows-lane | LOCAL-PASS | /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/ci-required/windows-lane-output.stdout |

windows_reach: none for the native Windows test allowlist, computed from its own unit/integration import closures; changed_reachable=[].
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
The gate authorizes PR opening. Merge requires CI / required and the deferred Windows job to be green on the exact PR head.

## Host load and retained evidence

```text
criterion3/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=37.97 run_max_load=44.43 run_mean_load=23.97 run_readings=106 wait_first_load=37.97 wait_max_load=37.97 wait_mean_load=37.97 wait_readings=1 read_errors=0
criterion3/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=15.32 run_max_load=20.76 run_mean_load=18.47 run_readings=9 wait_first_load=41.96 wait_max_load=41.96 wait_mean_load=21.28 wait_readings=61 read_errors=0
ci-required/windows-lane-output.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.40 run_max_load=36.17 run_mean_load=25.56 run_readings=5 wait_first_load=19.77 wait_max_load=19.84 wait_mean_load=17.43 wait_readings=14 read_errors=0
native-baseline-test.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=420 wait_timed_out=0 run_start_load=14.64 run_max_load=18.30 run_mean_load=16.76 run_readings=3 wait_first_load=29.88 wait_max_load=29.88 wait_mean_load=20.31 wait_readings=16 read_errors=0 prewait_s=420
```

Initial interrupted local C6 setup is excluded from credited evidence; the corrected exact-tree remote build completed. No suite was restarted during recovery.
Full execution record: /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/criterion3/evidence/execution.json (SHA256 eb3e51f1da05ffce12b3ba5cf793c062cc65469db2e1bf809e9b7cedca0f24ac)
Full native comparison: /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/native-baseline-compare.stdout (SHA256 20718ca174b4326e6dd20863f0f9837d3b040471accf063729bc34d9f9935a52)
Full CI accounting: /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/ci-required/accounting.json (SHA256 b7d5211adfad62a473437af33a01adb56fc64d4d951d87bfb587d06bd3b3d20f)
Full acceptance preservation evidence: /var/tmp/gc-ga-djt1xiqf-gate.5am9fjjo/preserved-guards.json (SHA256 3cf4dd6b5b6ee5ed88b95b2c30b9f775218f992c122268821c88fdb227e10ac6)
