# Release gate: local Bazel runtime guidance (ga-73zctil)

**Verdict:** **PASS**

Reviewed source: `293dd40cd4f604bda50df02a84364b65be862823` (build ga-ylttci, review ga-ksicwwk).
Pinned main: `afff5f04f6867f939f1d3df9c8b7f2b98e674d40`. Canonical tested merge: `dceb1249c029f47921ddf58a29881cde1232ee7e`,
tree `07f96ed58b297280e2ae6cb4b536aa8b559b7b0d`. Deploy branch: `deploy/ga-73zctil-gate`, cut at
the exact reviewed source; its only additional content is this gate record.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Review ga-ksicwwk records PASS for the exact resolved source; no carryover or source substitution. |
| 2 | Acceptance criteria | PASS. Four contributor Markdown files document the implemented hook matrix, both explicit selectors, warnings, missing ICU sonames, recovery choices and the limits of Go fallback evidence. The actual banner matches TESTING.md byte-for-byte. All nine prerequisite runtime/hook regression tests pass by name, covering absent/resolvable runtime and explicit/invalid choices. The runtime implementation landed in PR #7941; both required landing files exist at the pinned main. Fresh remote docsync passes with worker ICU74. The PR body carries the exact builder bypass disclosure. |
| 3 | Tests pass | PASS. Four complete forced-uncached remote phases plus the mandatory native floor completed successfully. Counts and execution proofs below; no owned tests, failure attribution or waiver. |
| 4 | No unresolved HIGH review findings | PASS. Review ga-ksicwwk reports no security/style blocker and no unresolved HIGH finding. Its wording observations are nonblocking and do not change the reviewed patch. |
| 5 | Final branch clean | PASS. Canonical merge scratch and isolated source branch are clean; this record is committed before publication and final branch cleanliness is checked again before push. Pre-existing role-worktree gate drafts are retained outside this branch. |
| 6 | Clean divergence from main | PASS. Canonical merge against the pinned main succeeds with tree `07f96ed58b297280e2ae6cb4b536aa8b559b7b0d`; fresh gc/scripts build and make vet/nogo pass. No self-rebase. |
| 7 | One feature theme | PASS. CONTRIBUTING.md, TESTING.md, engdocs/bazel-quickstart.md and engdocs/contributors/docs-organization.md describe contributor hook engine selection when the local pinned runtime is unavailable. No runtime implementation, public docs or unrelated feature is included. |

The code-before-guidance condition is satisfied by the verified landing of
PR #7941 at `afff5f04f6867f939f1d3df9c8b7f2b98e674d40`. Both
`.githooks/lib/local-runtime.sh` and `scripts/bazel_local_runtime_pin_test.go`
resolve at that main commit. Squash landing is checked by content, not by
ancestry of the original builder commit.

## Commands and retained results

`test_cmd_scope: full-suite`

`test_cmd`: canonical `gate-criterion3.sh` from the pinned merge scratch,
running the documented complete Bazel unit, acceptance, integration-packages
and full-integration lanes through `gate-rbe-run.py`, followed by its
computed native floor. The four phases force `--nocache_test_results` and
`--noremote_local_fallback`; the integration phase has no smoke filter.

| Phase | Remote shard results / native steps | PASS | FAIL | SKIP | Invocation |
|---|---:|---:|---:|---:|---|
| unit | 366 | 65754 | 0 | 230 | d1d5f441-ac24-4985-a51b-f0c5b83ad1bc |
| acceptance | 24 | 410 | 0 | 13 | 25475517-dfe3-4b61-b883-ba2bd1d03afc |
| integration-packages | 77 | 43661 | 0 | 64 | 12b5f221-6ac5-44d5-999a-6c2e25616210 |
| integration-full | 14 | 188 | 0 | 14 | 6d71b3dc-eb18-4a3d-ae9a-2e1bdf708d5f |
| Native floor | 16 steps | 21 | 0 | 14 | retained floor logs |

`test_counts`: 110034 PASS, 0 FAIL,
335 SKIP. These are top-level Go result lines,
summed across retained jobs; a test executed in multiple phases counts in
each. Native floor results supplement the remote phases, not a fallback or
a replacement local suite. All 481 remote shard results are PASSED.
Non-Go targets are included in the successful Bazel phase verdicts.

`diff_tests_executed: none (no diff-owned tests)`; the mechanical ownership
reader found no test hunks in this four-Markdown-file patch.
`waiver_ref: none`; `failure_attribution: none`; `heavy_mode: none`.
`ci_lane_run: n/a (no CI configuration changed)`.

`//test/docsync:docsync_test`: 16 PASS, 0 FAIL, 0 SKIP, executed remotely
with the pinned ICU74 worker environment. This is fresh deploy evidence.

All nine prerequisite tests pass by name in the fresh unit output:

- `TestBazelLocalRuntimePinGuardRejectsDrift`: PASS
- `TestBazelLocalRuntimePinMatchesProbe`: PASS
- `TestLocalRuntimeLibraryIsSafeToSource`: PASS
- `TestLocalRuntimeProbeNeverRunsUnbounded`: PASS
- `TestLocalRuntimeProbePlatformSelection`: PASS
- `TestLocalRuntimeProbeRealLoader`: PASS
- `TestLocalRuntimeProbeTimeoutIsUnknown`: PASS
- `TestPreCommitLocalRuntimeDocsSelection`: PASS
- `TestPrePushSuiteLocalRuntimeSelection`: PASS

`policy_lane: PASS`: isolated fresh views ran pinned fmt-check,
check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows,
check-residency-boundary, check-hooks, test-ci-policy and lint/nogo, plus
the required Bazel format, release, dependency, boundary and CI-policy targets.
`drift_lane: PASS`: make bazel-sync followed by git diff --exit-code and an
empty full porcelain status; generated spec/schema, SPA bundle/client and
lockfile drift targets also pass. `openapi-breaking-check: PASS` against the
pinned base. These checks completed before criterion 3 launched and each
used its own fresh view. Together these cover make check's shell guards and
its full Bazel unit suite. No dashboard/API implementation was changed.
`make check-hooks` confirms `.githooks` is active. The real staged docs
pre-commit ran successfully during canonical merge creation.

Smoke uses real hook entry points and the real host loader with command
recorders: missing runtime selects the documented Go engines, the warning
matches exactly, and an injected failing Go command rejects the commit
with exit 7. Recorders prove selection/error propagation only; they do not
claim that a fallback suite passed.

## Execution, skips and host load

`test_exec_mode: remote-exec`; `test_exec_bep`: each phase has strategy remote,
no cached flags, attempt 1, start within its phase window and terminal verdicts
for every shard. No mixed execution, cached test credit or retries.
`fallback_reason: none`; `farm_test_seconds: 7252.49`.
`admission`: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 106.83209991455078, "waited_seconds": 0.001}.
`rbe_state`: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}.
`owned_topup: []`; `floor_topup: complete`, all 16 steps exit 0.
The floor selects every environment-absence skip by package and parent test,
plus beadstest, Darwin compile and both runner self-tests.

`skip_inventory`: remote 250 other and
71 environment-absence events; every
name/reason is retained in `remote/skip-inventory.json`. Native floor SKIPs
remain inventoried, never reported as PASS. They are existing optional-tool,
fixture-input, platform and intentional skip-contract cases; this patch
changes no test/helper/package and no skipped behavior. The doc-reading
docsync and runtime-pin checks execute and pass. Native skip result lines:

```text
--- SKIP: TestReplay (0.00s)
--- SKIP: TestSimCoverageGate (0.00s)
--- SKIP: TestSessionEventPumpLiveHerdr (0.00s)
--- SKIP: TestBrProviderConformance (0.00s)
--- SKIP: TestActivityLive (0.00s)
--- SKIP: TestHerdrConformance (0.00s)
--- SKIP: TestSessionEventsLive (0.00s)
--- SKIP: TestProviderLiveClaudeKindPath (0.00s)
--- SKIP: TestProviderLiveOccupantSwapKeepsLiveness (0.00s)
--- SKIP: TestProviderLive (0.00s)
--- SKIP: TestGetKeyBinding_SkipsGasTownBindings (0.00s)
--- SKIP: TestGetKeyBinding_CapturesUserBinding (0.00s)
--- SKIP: TestIsGTBinding_DetectsGasTownBindings (0.00s)
--- SKIP: TestSetBindings_PreserveFallbackOnRepeatedCalls (0.00s)
```

Load is measured by the canonical wrapper (5-minute host load). Native floor
keeps the threshold-15, 1800-second bounded soft wait; remote phases use
remote admission. Actual wait/run measurements, including timeout state:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=63.26 run_max_load=70.53 run_mean_load=45.87 run_readings=90 wait_first_load=63.26 wait_max_load=63.26 wait_mean_load=63.26 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=51.70 run_max_load=70.00 run_mean_load=64.88 run_readings=12 wait_first_load=23.71 wait_max_load=69.94 wait_mean_load=44.07 wait_readings=61 read_errors=0
```

`test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-73zctil.5x_9ysqy/criterion3/evidence/remote/logs`.
Native logs: `/var/tmp/gc-heavy-gate/runs/deploy-ga-73zctil.5x_9ysqy/criterion3/evidence/remote/floor-logs`. All 36 native SKIP events, including
subtests, retain their individual reasons in `remote/native-skip-inventory.json`.
The remaining top-level SKIPs are missing replay/coverage inputs, optional br,
interactive tmux bindings, and deliberately disabled live-herdr journeys.
The other nested SKIPs include absent PostgreSQL DSN and intentional
provider-start-error skip-contract cases. None is owned by this Markdown diff.
Evidence is retained and not deleted.
Execution/BEP/skip/floor records: `/var/tmp/gc-heavy-gate/runs/deploy-ga-73zctil.5x_9ysqy/criterion3/evidence`.
Build, smoke, fast-lane manifests and independent verification:
`/var/tmp/deploy-ga-73zctil.5x_9ysqy`. Context: `context.json`; combined verification:
`recovered-remote-proof.json`. Commands are preserved in c6.sh,
fast-lane.sh, fast.sh and criterion3.sh. The merge scratch is temporary;
its output base can be released after all artifacts have been retained.

## Builder push disclosure

The builder made a one-off `git push --no-verify` of exactly
`293dd40cd4f604bda50df02a84364b65be862823` to the new `builder/ga-ylttci`
branch under mayor ruling gm-wisp-k450vpd. Scope: four docs files, a passing
Go docsync pre-commit, local ICU74 loader failure and two scripts-test shard
timeouts at host load 115–170. Authorization covered that SHA and that plain
push only, with no force. It does not replace any deployment check; this
gate independently ran fresh remote docsync and the complete suite.
The same disclosure is included in the PR body.
