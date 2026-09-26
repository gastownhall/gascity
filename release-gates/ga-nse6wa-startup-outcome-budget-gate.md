# Release gate: ga-nse6wa — startup outcome hang budget

**Verdict:** **PASS**

Reviewed source: ed49f25550aab50204595d6a4c759d8dcf8dc885. Remote deploy; tested base origin/main@2d7d33954459e0da1aec2feacfb60b810f02afb5; canonical merged scratch d335275af0dbd8781632f60f80fb6b3df4f1f801 at /var/tmp/gc-merge-ga-nse6wa.6Y4dgj. Exact reviewer PASS: ga-2h5j3v. Build bead ga-e4bhca is stamped gc.fixes_tracker=ga-hvjhay. No existing target PR was found before criterion 6.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS present | PASS | Fresh exact-candidate PASS on ed49f25550aab50204595d6a4c759d8dcf8dc885, including the final Bazel source/dependency follow-up; stale tdd_green was not substituted. |
| 2. Acceptance criteria met | PASS | Shared 60s hang budget replaces the post-control 2s literal; outcome, identity and ordering assertions remain. All five additional startup-outcome iterations passed while the full suite was observed active. New tests prove the minimum budget, a correct 30s transition, and a genuinely over-budget transition that still fails. |
| 3. Tests pass | PASS | All 40 documented full-suite jobs completed: 38 PASS, 2 FAIL, 0 job SKIP; real wrapper exit 2. The unrelated tmux fixture failures are attributed below under the verified fix-carrying exception. Test/subtest events, including repeated lanes: 95395 PASS, 3 FAIL, 327 SKIP. All four named diff-related results below PASS without FAIL/SKIP. Required policy/static/native lanes PASS. |
| 4. No high-severity findings open | PASS | Exact-source reviewer PASS has no unresolved HIGH findings. Its cosmetic citation note resolves because the sibling phase-2 deadline file is now on tested main. |
| 5. Final branch is clean | PASS | Source checkout clean before adding this gate; only this process record is committed above the reviewed source, with status verified after commit. |
| 6. Branch diverges cleanly from main | PASS | Merge-tree exit 0 on tested base; canonical scratch materialization, active pre-commit, full go build ./... and go vet ./... PASS. No self-rebase required. Base freshness is rechecked before publishing. |
| 7. Single feature theme | PASS | Four workertest test/BUILD files, one startup-outcome deadline theme. All three source commits cite confirmed build bead ga-e4bhca. Ancestry guard PASS for ga-nse6wa/ga-e4bhca; no stack or denied paths. |

test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4

test_cmd_scope: full-suite

waiver_ref: none

failure_attribution: TestIsAgentRunning and its multiple_process_names_with_match subtest -> ga-vkhfnj | clause 3(a) MECHANISM; all four clauses satisfied. ga-nse6wa attribution update: the provisional repeat hold is superseded by the verified fix-carrying contract. Own build_bead ga-e4bhca is gc.fixes_tracker=ga-hvjhay. Blocking condition tracker ga-vkhfnj has designated fix ga-sxtkmu (mayor stamp 2026-09-25), successor ga-piz22t still OPEN/ready-to-build; tested main still has doltServerStartupLimit=10s while resolved b496ce84d493681cc4dec16cdc31f4fee5d7f5dc sets 30s, proving that stamped fix has not landed. This is the tracker-level repeat exception, not a claim the Dolt fix directly repairs the shell subtest. Clauses 1 and 4: TestIsAgentRunning/runtime-tmux files untouched, separate package. Clause 2: tracker predates run, exact 2026-09-06 sh-to-zsh sighting read. Clause 3(a): tmux tests do import the normal workertest package, but go list shows all changed Go files are TestGoFiles only; its nine normal compiled GoFiles are unchanged. The modified test-only constant/tests and workertest_test BUILD sources cannot execute in the tmux test binary. No production Go changed. No isolated rerun used as attribution evidence. Continue the full gate, preserving raw exit and counts.

failure_attribution: TestGastown_ConfigStartStop -> ga-vkhfnj | clause 3(a) MECHANISM; all four clauses satisfied. ga-nse6wa full gate additional sighting | TestGastown_ConfigStartStop | integration-rest-full-6-of-8 | /var/tmp/ga-nse6wa-full-shards-r1/integration-rest-full-6-of-8.log | expected tmux sessions never appeared after the common 20s wait: boot and deacon active, mayor creating; test 26.33s. Prior comment 92b0520e-e8fe-5b4b-8d09-e28ef02af947 (2026-09-21 ga-uhbj9p) records TestGastown_ControllerStartStop timing out after 20s with deacon creating and only mayor present. Both tests call setupGasTownCityNoGuard -> setupGasTownCity(t, nil, agents) -> waitForExpectedTmuxSessions; same root fixture readiness condition, not a new per-test tracker. Exact ConfigStartStop name is a first sighting in the 191 predating comments; condition already tracked. Clauses 1/4: no integration fixture, runtime or production Go paths changed. Clause 3(a): this candidate changes only workertest TestGoFiles and BUILD test declarations, excluded from the compiled gc binary and the integration fixture code. Clause 2: this tracker and matching helper-condition sighting predate the run. Repeat fix-carrying contract applies as already verified: own ga-e4bhca fixes ga-hvjhay; ga-sxtkmu is stamped for ga-vkhfnj and remains unlanded (ga-piz22t OPEN; tested main readiness budget still10s versus its30s fix). Raw FAIL retained; no isolated rerun used as evidence.

policy_lane: make test-ci-policy PASS; make lint-affected fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=2d7d33954459e0da1aec2feacfb60b810f02afb5 PASS (zero issues, including affected-package standalone vet). Private disk lint cache; shared Go/module caches preserved. Additional check-gomod-replace, check-native-dependency-surface, check-eventexport-isolation, check-core-boundary and test-native-doltlite-beads PASS. Canonical merged-tree full build and vet PASS; active hooks verified.

ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list change).

Bazel limitation: Bazel and Bazelisk are unavailable. The independently read reviewer PASS explicitly covers the manual BUILD follow-up. Both new test sources appear in the ordered go_test source list and the new //internal/testutil import is declared in its dependencies. No Bazel/Gazelle execution is claimed; CI retains its independent generation/build/test checks.

Runtime: short private HOME with empty .zshrc, TMPDIR=/var/tmp, GOFLAGS=-v and normal shared Go/module caches. Rootless Podman socket active; cached Dolt 2.1.7 and SQL-server 2.1.7/2.2.0 tags verified; Ryuk disabled under the fleet cleanup recipe. Full command covers fast unit, all six cmd/gc process shards, four core integration shards, runtime, review-formula, beads-store and REST categories; these cover the matching required CI path filters.

skip_justification: Unchanged platform/permission probes, helper subprocess entries, optional live services/provider profiles, legacy characterization tests and category routing. Process-backed cases run in their documented process/integration categories; no named diff-related test skipped.

load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
load_start: 8.21
load_max: 49.22
load_mean: 22.95

Load sampler: samples=74, read_errors=0.

diff_tests_executed:

- TestPhase2StartupOutcomeBoundStaysAHangDetector: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeResultToleratesASlowButCorrectTransition: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeResultStillFailsWhenGenuinelyBroken: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeBounds: PASS (2 full-suite appearances).

Additional acceptance command: isolated-test-run.sh -- go test -run ^TestPhase2StartupOutcomeBounds$ -count=5 -v ./internal/worker/workertest; scope focused, 5/5 top-level PASS, exit 0, observed concurrently with the full suite. Its results are excluded from the full-suite counts above.

Evidence: /var/tmp/ga-nse6wa-full-r1.log, /var/tmp/ga-nse6wa-full-shards-r1/, /var/tmp/ga-nse6wa-policy-r1.log, /var/tmp/ga-nse6wa-static-r1.log, /var/tmp/ga-nse6wa-repeat-r1.log.
