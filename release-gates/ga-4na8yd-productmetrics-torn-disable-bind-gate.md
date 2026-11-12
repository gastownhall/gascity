# Release gate: metrics disable binding across atomic config replacement

**Verdict:** **PASS**

Evaluated 2026-10-09T09:54:27.747141+00:00. Deploy bead ga-4na8yd; build/fix ga-mghdmc; independent review ga-1ja3nm; issue #7215.

- Reviewed source: `de799546ea36a8cf6eb1bb3f2e03cf9c5b0ed2cb` (resolved commit, not the provenance branch tip).
- Tested origin/main pin: `5bff4ac776ad0130128a3284c680de39b2018985`. Publication-time origin/main: `7ad7d59c02c3def5038e34bedb57b484090f2a70`; merge-tree succeeds without conflicts (tree `08c9063a76fbf7f53ce2329c17e473daa8b5f8e5`). Main advanced by one controller effect-transaction commit in cmd/gc during the suite; it touches no candidate path, shared build/config input or productmetrics code. The full-suite evidence remains explicitly pinned to the tested merge below, as gate-base.sh requires for a moving shared ref.
- Canonical merge commit: `17c87c87e47ac4414c961ad140ee5bdd903140ec`; predicted and materialized tree: `9f8d1b44e89fb6e99eaac06624310e786db7ee8d`.
- Deploy mode: remote; push remote: fork (quad341/gascity); isolated publication branch: deploy/ga-4na8yd-gate.
- Already-merged preflight: reviewed source is not reachable from main; no published target PR existed. No rebase or review carryover used.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-1ja3nm records an independent PASS at the exact reviewed source. Reviewer gm-wisp-oaqsl55 differs from author gm-wisp-xnx4io6. No actionable style, security or spec findings. |
| 2 | Acceptance criteria met | PASS — actual fresh full-suite output names all three new regression roots and all three torn-read subcases PASS; the stable hard-link security control PASSes. The patch preserves original storage errors and fail-closed validation, translates only proved config replacement at disable binding, and touches exactly the specified four files. Builder RED/GREEN and A/B plus independent package/race/stress evidence verified; PR body includes Closes #7215 and the measured results. |
| 3 | Tests pass | PASS — full-suite scope on the canonical merge; all four configured Bazel lanes completed with first-attempt, uncached results, one unrelated named FAIL attributed under criterion 3a (ga-of7dfv), zero diff-owned FAIL/SKIP. Every remaining skip has exact-site, whole-file/shared-hunk, printed-reason and causal reach evidence below. |
| 4 | No HIGH review findings open | PASS — zero unresolved HIGH findings; independent review reports none actionable. |
| 5 | Final branch clean | PASS — reviewed source and canonical merge are clean. The only additional publication artifact is this gate record; publication guards require its committed tree to be clean and source ancestry intact. |
| 6 | Diverges cleanly from main | PASS — clean merge materialized through normal hooks, identical to the predicted tree; whole-repository Bazel build including nogo/vet succeeded (20,783 actions), check-hooks PASS. Fresh private output base; repaired Vitess cache condition did not recur. |
| 7 | Single feature theme | PASS — one productmetrics CAS/read-boundary correction and its deterministic/security regressions; four files, +197/-1, one reviewed authored commit. No unrelated theme or internal agent configuration. |

## Full-suite execution

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: TestProviderRestartReportsAndReapsOrphanedRuntimes -> ga-of7dfv; clause3(a) MECHANISM`; `policy_attribution: none`.

All commands used the pinned canonical merge, a private Bazel output base, jobs=4, read-only fork cache, explicit private ICU runtime and `--nocache_test_results`. The unit command is `make check` (whole `//...`, shell guards included); the other full required scopes are:

```text
bazel test --config=ci --config=fork-cache --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel test --config=ci --config=fork-cache --config=integration --keep_going //test:integration_packages
bazel test --config=ci --config=fork-cache --config=integration-smoke --keep_going //test/integration:integration_test
```

Literal argv/environment and exits: `/var/tmp/ga-4na8yd-fresh-suite-runtime.sh`; detached run `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3`. Each lane retained its BEP, every configured target summary, every test-result shard, and raw log/XML with verified SHA256. Requested scope, configured targets, summaries and result labels agree exactly; no retry or cached result was credited.

| Lane | Bazel targets | Named PASS | FAIL | SKIP | Top-level PASS / FAIL / SKIP |
|---|---:|---:|---:|---:|---|
| unit | 233 | 54506 | 0 | 214 | 29629 / 0 / 164 |
| acceptance | 7 | 410 | 0 | 13 | 116 / 0 / 10 |
| integration-packages | 26 | 36088 | 1 | 53 | 18898 / 1 / 43 |
| integration-smoke | 1 | 16 | 0 | 1 | 13 / 0 / 1 |
| Total | 267 | 91020 | 1 | 281 | 48656 / 1 / 218 |

Counts include repeated named executions across lanes and embedded subprocess evidence; they are not distinct-test counts. Bazel shard jobs: 408.

Diff-owned tests, all PASS in retained full-suite output:

- `TestBeginDisableBindReadTornByReplaceIsStateConflict` — PASS.
- `TestHardLinkedConfigStaysUnsafeNotStateConflict` — PASS.
- `TestLockFreeConfigReadTornByReplaceIsTagged` — PASS.
- `TestLockFreeConfigReadTornByReplaceIsTagged/replace_after_descriptor_stat` — PASS.
- `TestLockFreeConfigReadTornByReplaceIsTagged/replace_after_open` — PASS.
- `TestLockFreeConfigReadTornByReplaceIsTagged/unlinked_between_lookup_and_stat` — PASS.

Both existing CAS roots PASS by name. The process lane actually ran `TestTutorial01` and `TestTutorial01/08-agent-pools` with GC_FAST_UNIT=0. OpenAPI comparison and schema freshness roots also PASS.

## Skip evidence

`skip_justification`: all 281 actual SKIP rows individually reviewed. Exact sites are UNCHANGED; skipped source files are byte-identical to base, with no owned test body or shared hunk. The only changed test helper is replaceConfigFixture, called solely by the new owned torn-read table and bind regression; all consumers PASS. No package init, TestMain or global fixture changed.

The complete default/integration/acceptance test-binary closures acknowledge cmd/gc and productmetrics reach. Metrics error tagging and joining do not alter the recorded platform, opt-in, helper-entry, executable/runfile discovery, fixed fixture error or process-lane guard inputs. Every unit process omission is paired with a fresh same-name/package integration PASS. Other opt-in/platform/fixture skips remain SKIP and are not counted as executed.

| Actual skip category | Rows |
|---|---:|
| unchanged external executable, runfile or capability precondition | 40 |
| unchanged explicit external fixture or opt-in guard | 27 |
| unchanged subprocess helper entry guard | 9 |
| unchanged platform guard | 30 |
| unit process coverage executed in integration | 104 |
| unchanged host permission or procfs capability guard | 4 |
| unchanged provider conformance fixture | 20 |
| unchanged dependency feature guard | 2 |
| unchanged skip-ledger example fixture | 1 |
| unchanged unsupported fixture capability | 17 |
| unchanged retired characterization condition | 1 |
| unchanged upstream fixture connection guard | 2 |
| unchanged narrowly tracked conformance row | 4 |
| unchanged explicit historical placeholder or disabled characterization | 20 |

Individual names, raw log lines, reasons, source guards, whole-file blobs, import reach and causal justifications: `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3/full-skip-site-proof.json` (SHA256 3cea9599f807540f11123e8f49c069525f34ffba4e095ecc94f6c66898d1f872). Alternate integration passes: `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3/unit-process-opt-outs-integration-pass.json`. Source explanations and helper consumers are retained alongside the proof.

## Policy, drift and required CI leaves

`policy_lane: PASS` — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks test-ci-policy lint; full nogo plus 12 fresh policy/format/generated-wire/OpenAPI targets. `drift_lane: PASS` — make bazel-sync followed by git diff --exit-code and empty git status including untracked, plus six fresh schema/spec/dashboard/client/lockfile drift targets. Each used its own fresh-tree-run view with gate-base.sh pinning this base; none wrote the canonical suite tree.

Fast-lane 18-target manifest: `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.fast/FAST_VERIFIED.json`. Fast lane completed before runtime preparation and C3 launch. An initial double runtime-wrapper setup error occurred before any check; the original logs are retained at /var/tmp/deploy-ga-4na8yd-fresh.yaaxivzm/fast-first-wrapper-error. The corrected fast lane completed all targets; no source failure was attributed or waived.

Required ci.yml leaf accounting (ga-1zgega): runner-policy LOCAL-PASS; changes LOCAL-PASS; credential-provider-windows NOT-TRIGGERED (no production reach); pack-gate NOT-TRIGGERED. Deferred-to-PR-CI: none; not-covered: none. CI-config diff: none (3c not applicable). Accounting proof: `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.ci-accounting/ACCOUNTING_VERIFIED.json`.

OpenAPI `form: workflow-equivalent per ga-3rtlng; standard form not run (ga-twhw87 open)`. Actual base spec is outside /tmp, passed as GC_OPENAPI_BREAKING_BASE_SPEC, with SHA256 04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912. TestCommittedSpecAgainstBase and TestGateAgainstOasdiff PASS; this is not self-comparison. Teeth proof: ga-2egszb, `/var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro`.

## Failure attribution

Integration ACP failure at proctable_linux_test.go:194: after agent kill found=[], expected escaped tool root pid 570. It is an untouched test/body and byte-identical file, zero shared hunks (clause1); new actual condition tracker ga-of7dfv created/readback verified after no covering tracker search hit, using gm-sf3238 discovery-run escape (clause2); complete integration ACP test-binary closure excludes the changed productmetrics package (mechanism proof3a landed); no package/path overlap (clause4). ga-q17kpi was opened and rejected as a different condition. No claim about the missing orphan root cause; no ACP rerun. Proof: /var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3/acp-failure-ownership-reach-proof.json.

The initial service collected the full integration scope and stopped on its real nonzero exit. A separate detached continuation runs only the remaining full integration-smoke lane against the identical canonical merge; it does not repeat prior targets. Both original failure logs and continuation results/load remain retained. This is attribution, no waiver or omitted required lane.

## Runtime and load

Before C3: active rootless podman socket verified; cached dolthub/dolt-sql-server:2.2.0 image verified; testcontainer-sweep enabled; Ryuk disabled with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Pinned test bd v1.3.1, private ICU runtime, isolated-test-run wrapper and fresh checkout used. No live-city selectors supplied to tests.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=6.85 run_max_load=18.11 run_mean_load=14.90 run_readings=28 wait_first_load=6.85 wait_max_load=6.85 wait_mean_load=6.85 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.40 run_max_load=27.18 run_mean_load=16.95 run_readings=53 wait_first_load=12.40 wait_max_load=12.40 wait_mean_load=12.40 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.68 run_max_load=14.51 run_mean_load=14.37 run_readings=4 wait_first_load=14.68 wait_max_load=14.68 wait_mean_load=14.68 wait_readings=1 read_errors=0
```

The summaries record load_threshold, load_waited_seconds, load_wait_timed_out, run_start_load, run_max_load, run_mean_load and run_readings (run distinct from wait). Heavy classifier mode=none; ordinary load-gate policy retained. No full suite was used as a load generator.

## Retained evidence and landing

`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3/logs`; official gate-test-evidence.py result: OK. Full verification: `/var/tmp/gc-heavy-gate/runs/ga-4na8yd-fresh.c3/FULL_SUITE_VERIFIED.json`. All raw per-job logs/XML/BEP and hash manifests remain available; none deleted.

Historical author A/B: baseline 4 failures /20,000 versus patched 0/20,000. Earlier investigation: baseline 61/20,000 versus patched 0/20,000, plus 0/56,000 patched. Independent review: package and race each 1,370 named PASS/0 FAIL/0 SKIP; four concurrent compiled binaries, each 2,000 repetitions of each CAS root, total 16,000 PASS/0 FAIL/0 SKIP. Historical evidence is separately attributed and does not replace this fresh full-scope gate.

ga-va482s repair tracker closed no-op after mayor cache repair; this run used a new owned output base and completed the entire gate fresh. ga-mghdmc and ga-20w0hj remain OPEN; ga-iufo27 blocks on ga-mghdmc until actual landing. Only mayor closes the fix shipped after merge. Merge belongs to mpr; this gate does not assert the change has landed.
