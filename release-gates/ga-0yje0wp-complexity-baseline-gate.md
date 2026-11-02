# Complexity baseline release gate

**Verdict:** **PASS**

This standalone gate satisfies the candidate's carried architect ruling: D1-D5 at the pinned base, with D6 recorded. Two unrelated integration failures are attributed under criterion 3a; their real FAIL results remain in the raw evidence. No waiver, rebase, source change, runtime deployment or merge is authorized by this record.

- Bead: `ga-0yje0wp`; review: `ga-5a06rtd`; true build: `ga-sjd1o7r`; rework carrier: `ga-s3mkmk1`.
- Reviewed source: `b4ad811ca3b5478ceaf63454e46b9466ceada20b`; provenance branch is not a push target.
- Frozen main: `648f3b5e3fc4d1ee2bf17d57a4502bec0cfd765a`; pinned snapshot base B: `f27e0ac4d2d502eec6d13cd885ec5a0d84cd0c8d`.
- Canonical gated merge: `bcb1c47ace86f7d6a3ce6116926917520db5932e`; tree: `63069df9eb4bae8565672c6fbe612b4f0f26d0f7`.
- `DEPLOY_MODE: remote`; `BASE_REF: origin/main`; `gate_base_kind: current-tip`.
- `merge_state: clean (origin/main@648f3b5e3fc4)`.
- `merge_validation: MPR tests and merges the exact cleared head; repeat D4 before merging`.
- Evidence root: `/var/tmp/gc-gate.ga-0yje0wp.m6bisjt0`; finalized 2026-10-23T20:26:01.923650+00:00.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Round-2 reviewer PASS names the resolved source. No carryover or reviewer-added tests. `criteria1-review-audit.json`. |
| 2 | Acceptance criteria met | PASS | D1-D5 independently reproduced; D6 recorded; D7 honored. D8 remains an explicit landing obligation. |
| 3 | Tests pass | PASS | Five fresh remote phases plus all 16 native floor jobs complete. Two qualified attributions below; no other failure. `gate-final-completion.json`, `final-evidence-audit.json`. |
| 4 | No unresolved HIGH findings | PASS | Latest style/security findings none; unresolved HIGH 0. `criteria4-findings-audit.json`. |
| 5 | Final branch is clean | PASS | Canonical scratch clean; only this gate record is committed above the reviewed source. Other bead drafts are preserved outside the deploy tree. Publication requires a clean final index/worktree. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge; remote Bazel build/nogo, active hooks and ICU74 provenance PASS. `build-vet.log`, `check-hooks.log`, `icu74.log`. |
| 7 | Single feature theme | PASS | Both source commits touch only the generated baseline JSON; citation/denylist guards PASS. `criteria7-scope-audit.json`. |

## Complexity contract and landing obligations

D1: exactly `engdocs/complexity-baseline.json`, +1235/-533. D2: B is an ancestor of frozen main and the candidate/main merge base. D3: verified gocyclo v0.6.0; `make complexity-check` on the reviewed source exits 0; two real `make complexity-update` runs reproduce identical committed bytes. Schema, threshold 20, 604 unique identities, sorting, relative paths and canonical serialization pass. Reviewed baseline SHA256: `fe850111dfedb0d84d370147018e338b45c7caddb99a2b76c0f5f39945569aae`. D4: merge clean; `git log B..main -- engdocs/complexity-baseline.json` empty. D5: `COMPLEXITY_BASE_REF=<frozen-main> make complexity-diff` on the merge prints `complexity: no threshold changes`. D7: no chasing the tip or hand edits.

D6 is NON-GATING under the architect's 2026-10-21 ruling: absolute `make complexity-check` exits 2 and reports 27 new + 37 regressed = 64 rows on the merge versus 161 new + 122 regressed = 283 on untouched main. These do **not** fail identically. This is neither a criterion-3b mirror attribution nor a waiver; literal rc0 on current main's merge is not this candidate's criterion.

Before merge, MPR must recheck D4 against then-current main: clean merge AND no baseline history after B. If either trips, return to the builder to regenerate once at the new tip; never hand-resolve the file. After landing, D8 requires comparing the landing commit's baseline bytes with SHA256 `fe850111dfedb0d84d370147018e338b45c7caddb99a2b76c0f5f39945569aae`. `gc.work_outcome` remains `blocked` until that comparison succeeds. Tracker `ga-q7p6is` stays OPEN even at landing; its named exit is follow-up `ga-41e6k8r`.

## Full-suite execution evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `ci_lane_run: n/a (no CI-config change)`; `waiver_ref: none`; `diff_tests_executed: none (no diff-owned tests)` (`owned.tsv` and official `owned-explain.txt`).

The detached canonical `gate-criterion3.sh` chain ran `bazel test //...`, acceptance, integration packages, full integration minus `gate-latency-tests.txt`, then the latency list alone. Every remote phase used `--config=remote-exec --config=ci --keep_going --nocache_test_results --noremote_local_fallback`, with each tier's configuration, verbose by-name results and retained BEP. Native-only continuation completed the required floor after qualified failure attribution; it reexecuted no remote phase. No narrowed substitute suite or combined local fallback is claimed.

| Remote phase | Fresh terminal result attempts | Farm test seconds |
|---|---:|---:|
| unit | 373 | 2959.525 |
| acceptance | 24 | 1141.835 |
| integration-packages | 80 | 3157.852 |
| integration-full | 14 | 2632.991 |
| integration-latency | 1 | 122.224 |

`test_exec_mode: remote-exec`; `test_exec_bep`: 492/492 remote, uncached, attempt 1, inside each phase window, with terminal verdicts; cached remotely/locally, other strategy and retries all 0. All 984 remote artifact hashes and 515 preserved completed files verified. `farm_test_seconds: 10014.427`. `rbe_state`: configured, kill switch absent, certificate valid through 2027-01-09; worker environment tree/main both `sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45`. `fallback_reason: none`.

`admission`: remote floor 25 GiB, cap 2, initial free 49.414 GiB, waited 0.002s; native continuation free 56.066 GiB, waited 0.001s. Live parent-slot/child execution without duplicate enqueue was verified and reported to mayor. Wrapper was PASS-THROUGH in the private scratch; remote actions were hermetic, and the native runner explicitly scrubbed store/session selectors and used pinned bd v1.3.1. No claim of an isolated ambient shell is made.

`test_counts` are column-0 result lines summed per job, not unique test names: remote 125503 PASS / 2 FAIL / 356 SKIP; native 21 PASS / 0 FAIL / 14 SKIP; union 125524 PASS / 2 FAIL / 370 SKIP. `gate-test-evidence.py` final by-name result is OK. `test_log_dir: /var/tmp/gc-gate.ga-0yje0wp.m6bisjt0/criterion3/evidence/remote/logs`; logs and raw false strict reports are retained unchanged. Supplemental `gate-final-completion.json` proves complete/valid-after-attribution, no unexpected failures and unchanged raw hashes; it does not rewrite raw FAILs.

## Qualified failure attributions

`failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy | clause 3: a MECHANISM landed`. Untouched read-sweep fixture omits required assignee, HTTP422 instead of 200. Official site UNCHANGED/shared_hunks 0. Designated stamped remedy `ga-ze0aym0` remains unlanded. Proof: `failure-attribution.json`.

`failure_attribution: TestGcSeatHookLatencyBudget -> ga-bqsrp4o | clause 3: a MECHANISM landed`. Prime fastest 1210ms exceeds the unchanged 1000ms budget. Worker probe CLEAR (load1/5/15 1.58/0.55/0.20, waited 0, threshold 50); this is a real FAIL, not busy deferral, and was never retried. Official site UNCHANGED/shared_hunks 0. Designated stamped remedy `ga-yu01v6x` remains unlanded. Proof: `latency-failure-attribution.json`.

For both: trackers were opened and predate this run; sightings were readback-verified. Computed test-binary import closures have no changed input; the external gc binary edge was checked too (no runtime baseline reader; root embedded assets are schemas only). No path overlap or added test/census/target load. Repeat fix-carrying exception independently verified: this deploy's true build `ga-sjd1o7r` is stamped for `ga-q7p6is`, and each blocking condition has its named, stamped, still-unlanded fix. No self-waiver, source rejection or retry into PASS.

## Native floor, skips and policy

`floor_topup: complete`; 16/16 jobs exit 0: all 13 computed package selections (environment-absence parent tests plus pinned beadstest), Darwin compile, push-gate lock self-test and local-concurrency self-test. `owned_topup: []`. Native per-job commands, selected names and results are retained in `floor-topup.json`.

`skip_inventory`: 356 remote rows (71 environment-absence, 285 other). Every computed environment-absence package/parent was retried by the required native floor. Native skips (39 inventory rows) are retained with their actual reasons in `native-skip-inventory.json`; non-owned skips cover unavailable providers/platforms, prerequisites and deliberate suite lanes. This inert baseline-only change cannot reach those paths and owns no test; no skip is credited as execution of an owned test.

`policy_lane: PASS`: independent fresh views ran full format, remote nogo/lint, shell guards, runner-policy 5/5 and suite-coverage 15/15. `drift_lane: PASS`: actual `make bazel-sync`, empty tracked diff and empty tracked/untracked status in an independent fresh view. No lane modified the canonical suite scratch. Final record commit uses active normal pre-commit hooks.

Native soft load wait retained threshold 15 and max 1800s; actual completion summary:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=30.17 run_max_load=31.13 run_mean_load=29.73 run_readings=8 wait_first_load=73.06 wait_max_load=94.87 wait_mean_load=73.79 wait_readings=61 read_errors=0
```

No runtime deployment. Publishing this isolated branch and requesting MPR review does not assert main landing or close the complexity-debt tracker.
