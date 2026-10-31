# ICU-aware local hook validation release gate

**Verdict:** **PASS**

Evaluated 2026-10-15 01:52 UTC. Deploy bead `ga-75c2dam`; build `ga-3nnp7h`; independent review `ga-hqj1fnl`. Remote deploy, fork `quad341/gascity`, target `gastownhall/gascity:main`. Merge authority remains MPR. This record supersedes the historical incomplete/held runs; it credits only the fresh run after mayor release `gm-wisp-qggp0ps`.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer round 1 verdict pass on the exact resolved source below; style/security findings none, zero unresolved HIGH findings. Reviewer evidence is input; this gate independently executes the complete suite. |
| 2 — Acceptance | PASS | Five criteria mapped below, including public PR end-to-end evidence prepared from this run. |
| 3 — Tests | PASS | Fresh `make check` / Bazel whole tree, acceptance, all integration packages and unfiltered full integration; all 481 farm result attempts pass, plus all 16 native floor steps. `criterion3_valid=true`, command exit 0. |
| 3b — Policy and drift | PASS | Before the suite, separate fresh views ran pinned format, all `make check` shell guards, hooks, CI policy and whole-tree nogo/lint; 11 policy targets, 6 regeneration/drift targets and OpenAPI breaking check all pass. `make bazel-sync` plus empty tracked and untracked diff. |
| 3c — Changed CI lane | PASS | Not applicable: no workflow, CI matrix, timeout or required-check change. |
| 3d — Heavy classification | PASS | `HEAVY_MODE mode=none`; the diff owns/reaches no heavy-list package. |
| 4 — Review findings | PASS | No open HIGH findings; source and scoped acceptance unchanged since independent review. |
| 5 — Clean branch | PASS | Canonical candidate is clean. Only this gate record is added on the isolated deploy branch; a clean final index/worktree is required and verified before push. Historical untracked gate records are preserved outside the branch. |
| 6 — Merge and build | PASS | Normal-hook canonical merge has the exact predicted tree below. Real pre-commit ran scripts nogo and Go docsync; scripts test binary build and full-tree `make vet`/nogo build pass. Full CI-equivalent tests build the remaining tested targets. A later-base clean-merge check is recorded separately below. |
| 7 — One theme | PASS | Fourteen source paths implement the shared loader probe, both hook engine selections, their fixture/test/BUILD wiring and owning invariant. No unrelated feature or `.claude` path. Ancestry guard accepts only this deploy and its build bead; no stack. |

## Exact candidate and provenance

- Reviewed source: `1266e6688191e726fa89aa24e4a6bb384a17add8` (resolved commit, never a provenance branch tip).
- Gate BASE_REF: `fb80026bf7155841a3ef9e795670fe740df645d5`; tree `f2afd12ec302123e867342ac76c02ed5dae2a522`.
- Canonical merge commit: `47868f35eb3f790fb9965879d7d21c957de6f491`; tree `ed8537ff7794f606c7cc518857ca0ef0a17ed867`.
- Canonical parents: pinned gate base, then exact reviewed source. Recomputed `git merge-tree --write-tree` equals this tree; scratch remains clean.
- Builder TDD RED: `7324d16770d37fd3dc73ff381ee08f86b7b53dee`; GREEN: `98466fa797aea390212b4b80a9a158021744e601`. Both resolve and are ancestors of reviewed source. Reviewer separately reverted production hooks and observed 8 of 9 owners fail, then all 9 pass (64 cases). These historical RED proofs are not credited as this gate's fresh GREEN.
- Evidence root: `/var/tmp/deploy-ga-75c2dam.fixture-landed.j4v1xc5y`. Driver output ends `GATE_RUN_EXIT rc=0 state=complete`; driver and notifier are inactive/dead with MainPID 0.
- Final verification: `FINAL_EVIDENCE_VERIFIED.json`, `recovery-bep-verification.json`, `recovery-artifact-verification.json`, `final-skip-site-proof.json`, `final-skip-inventory.json` under that root. All 962 farm log/XML artifacts and 36 fast-lane artifacts independently match their SHA256/BEP digests.
- Current main advanced after the gate began to `3e3350d0d38f62d4204600275efd74e229580dfe` (one tmux inventory fix). Supplemental merge-tree check succeeds with tree `f752c8e353bb5dd61496933cd75aacc051dcda12`; this is mergeability evidence only. The full-suite evidence remains bound to the pinned base and canonical merge above.

## Acceptance evidence

1. RED reproduces the old local Bazel startup selection with missing ICU; builder RED and reviewer independent revert evidence are recorded above. Fresh GREEN executes the changed owners by name.
2. The owned selector matrices cover missing/usable/inconclusive runtime evidence, other platforms, absent tools, current/stale executor pin and explicit cache/rbe/go choices. Explicit modes retain their behavior; real failures block.
3. One ICU-major source generates the sonames. `TestBazelLocalRuntimePinMatchesProbe` verifies `MODULE.bazel`, loader output and quickstart pin; `TestBazelLocalRuntimePinGuardRejectsDrift` kills pin/retained-library/literal/document drift fixtures.
4. Real pre-commit entry point tests use isolated staged-docs repos; missing ICU selects Go docsync with a banner, usable ICU keeps Bazel, beads rejection and docs validation failures block. Fresh real-host smoke uses the actual missing-ICU loader and real hook scripts. Command recorders observe `make check-docs-go` and `make test-fast-parallel`; injected make exit 7 propagates through pre-commit. `hook-smoke.json` explicitly limits this smoke to selection/failure propagation, not a complete passing Go fallback push suite. The canonical merge's real staged pre-commit separately ran Go docsync successfully.
5. Owning `scripts/AGENTS.md` and hook comments are included; syntax/format/policy/nogo/BUILD sync and fresh Bazel-tier scripts/docsync all pass. The PR body includes the actual smoke output and Bazel evidence with those limits. The docs sibling follows this code; its work is not bundled here. No public issue was recorded in the source/review/condition beads, so no invented issue closure is claimed. Tracker `ga-sdon3o` stays open until actual landing.

## Fresh full-scope test execution

`test_cmd_scope: full-suite`  
`test_exec_mode: remote-exec`  
`test_exec_bep: all result strategies remote; cachedLocally=false; cachedRemotely=false; attempt=1; starts within each phase window; all terminal verdicts PASS`  
`farm_test_seconds: 7923.603`  
`fallback_reason: none`  
`waiver_ref: none`  
`failure_attribution: none`  
`ci_lane_run: n/a (no CI-config change)`

| Phase | CI-equivalent command | Result attempts |
|---|---|---|
| Unit/policy | `make check` → `bazel test //...` | 366 PASS (281 targets, including sharded results) |
| Acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 24 PASS |
| Integration packages | `bazel test --config=integration //test:integration_packages` | 77 PASS |
| Full integration | `bazel test --config=integration --test_arg=-test.v //test/integration:integration_test` | 14 PASS; no smoke/test-name filter |

All phases use `--config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --keep_going --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1`, plus pinned nogo flags and retained BEP. `gate-base.sh run` supplies this exact pinned base. Commands/raw logs: `/var/tmp/gc-heavy-gate/runs/deploy-ga-75c2dam.fixture-landed.j4v1xc5y/criterion3/evidence/remote`. Final execution record: `/var/tmp/gc-heavy-gate/runs/deploy-ga-75c2dam.fixture-landed.j4v1xc5y/criterion3/evidence/execution.json`.

`test_counts: 110021 PASS / 0 FAIL / 335 SKIP` from `gate-test-evidence.py`, summed over 497 per-job logs. Counts are result lines across jobs, not unique test names; native fixed shell/compile checks are separately recorded. All nine diff-owned tests pass by name, with zero owned SKIP/FAIL/MISSING:

- TestBazelLocalRuntimePinGuardRejectsDrift — PASS
- TestBazelLocalRuntimePinMatchesProbe — PASS
- TestLocalRuntimeLibraryIsSafeToSource — PASS
- TestLocalRuntimeProbeNeverRunsUnbounded — PASS
- TestLocalRuntimeProbePlatformSelection — PASS
- TestLocalRuntimeProbeRealLoader — PASS
- TestLocalRuntimeProbeTimeoutIsUnknown — PASS
- TestPreCommitLocalRuntimeDocsSelection — PASS
- TestPrePushSuiteLocalRuntimeSelection — PASS

`test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-75c2dam.fixture-landed.j4v1xc5y/criterion3/evidence/remote/logs`

### Native floor and skips

`floor_topup: complete=true, 16/16 exit 0` — 13 selected package invocations, including beadstest, plus Darwin filesystem compilation, push-gate-lock self-test (55/0) and concurrency self-test (25/0). Each environment-absence farm SKIP is selected by package/parent; all floor logs are retained and byte-identical to their copies in the final evidence directory. `owned_topup: []` because every owner passed in the original farm population.

`skip_inventory: farm 71 environment-absence / 250 other; native 36 additional events (14 additional top-level result lines)`; full per-event names, reasons and justification are retained. All 246 distinct skip source sites are mechanically UNCHANGED with zero shared hunks in their files. None is in the changed scripts test package. Non-owned skips describe opt-in replay/coverage/live-provider/Postgres/br tests, intentional conformance-skip fixtures, platform/root/session capabilities, subprocess-only helpers, existing tracked scenarios and fast-tier omissions covered by integration PASS. Native herdr remains an opt-in live journey; tmux user-binding tests require an enclosing tmux session. This hook-only diff does not change those conditions; it adds no production Go path into them. Existing base skips are disclosed, never counted as passes or waived for an owner.

Rootless Podman socket, pinned Dolt image 2.2.0 and enabled testcontainer sweep were verified before the suite. The isolation wrapper reported PASS-THROUGH for this scratch path: Bazel supplies the hermetic farm environment; the native floor explicitly strips live GC_/BD_/BEADS_/DOLT_ selectors and pins Git config and shell. No full local suite is stitched into the farm suite.

`rbe_state`: rc marker present, kill switch absent, certificate expires Jan 9 2027, candidate/base worker environment digest both `sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0`.

`admission`: remote, 25 GiB floor, cap 2, free 108.50981521606445 GiB, waited 0.001 seconds. Setup/fast lane used the 40 GiB floor.

Native load wait and run statistics (the documented soft timeout continued; no defer/waiver):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=48.70 run_max_load=51.16 run_mean_load=47.84 run_readings=8 wait_first_load=30.88 wait_max_load=48.70 wait_mean_load=24.27 wait_readings=61 read_errors=0
```

Outer remote-class observations, separate from native execution:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=60.25 run_max_load=63.01 run_mean_load=32.61 run_readings=93 wait_first_load=60.25 wait_max_load=60.25 wait_mean_load=60.25 wait_readings=1 read_errors=0
```

## Scoped complexity exit contract

Contract `base-relative-no-new-regression-v1`, authorized by mayor/PM `gm-wisp-6i95bts` and `gm-wisp-93ww8gt`. This replaces the old absolute baseline check for this scope only; the global stale-baseline tracker remains open.

Command, run on a fresh view of this exact canonical merge through `gate-base.sh run`:

```sh
make complexity-diff \
  COMPLEXITY_BASE_REF=fb80026bf7155841a3ef9e795670fe740df645d5 \
  COMPLEXITY_TOOL=/home/jaword/go/bin/gocyclo \
  COMPLEXITY_THRESHOLD=20
```

Tool module/version verified with `go version -m`: `github.com/fzipp/gocyclo v0.6.0`; executable SHA256 `9e8e74255442d2daf91c8766f7c91f7c7d04313ab4bca611e630fffc9c43182a`. Full version output is retained in `complexity-proof/tool-version.stdout`.

Actual command exit: **0**. Report:

```text
complexity: diff against fb80026bf7155841a3ef9e795670fe740df645d5
complexity: no threshold changes
```

`new:` rows: **0**. `regressed:` rows: **0**. Contract PASS requires both that successful valid scan and the absence of those rows; diff mode itself always exits 0 after reporting changes. Tool/threshold/shipped-code selection/exclusions/baseline are unchanged. Candidate/base commits and trees are pinned above. Exact argv, tool/version/hash, report rows and exit are retained in `contract-complexity.json`; complete stdout/stderr in `complexity-proof/candidate.stdout` and `candidate.stderr`. No baseline refresh or attribution substitutes for this check.

## Publication boundary

Only the isolated `deploy/ga-75c2dam-gate` branch is eligible for push and PR. It is cut at the exact reviewed source, followed only by this evidence commit. The public body carries the hook selection/rejection evidence and fresh Bazel coverage. Clearance must be success on the PR's exact verified head in the base repo before a peek-verified merge-request to mayor. No source tracker closes until MPR actually lands the change.
