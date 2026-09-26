# Release gate: ga-ea5y0t — re-gate PR #6663

**Verdict:** **PASS**

PR: https://github.com/gastownhall/gascity/pull/6663. Team author: quad341.
Exact gated PR head: 7085dca2f251bb8952932bf0a4459af65fbb1ddd, branch deploy/ga-nse6wa-gate.
Source deploy bead: ga-nse6wa; build bead: ga-e4bhca.
DEPLOY_MODE: remote. Tested origin/main: 77844c68b420c7175c2a47da34b414413e02fe8c.
Canonical merged scratch: c4c2e3c1a76da99d2f900e414a79ee05fa4eb14f, at /var/tmp/gc-merge-ga-ea5y0t.A2v9jc.
This re-gate record belongs to the separate deploy/ga-ea5y0t-gate audit branch; the existing PR head stays pinned to the commit above.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS present | PASS | Reviewer ga-2h5j3v passed the implementation and BUILD follow-up at ed49f25550aab50204595d6a4c759d8dcf8dc885. The subsequent source change is comment-only. Fresh MPR review at this exact PR head has verdict auto-merge, three reviewers OK, and no requested code changes; observed team review comment dated 2026-09-26T07:59:55Z. |
| 2. Acceptance criteria met | PASS | Post-control bound derives from the shared 60s hangBudget. Launch, interaction, outcome, identity and ordering checks remain. All five additional startup repetitions passed during the concurrent full sweep (19.869s). Full-suite tests prove the budget invariant, a correct 30s transition, and rejection of an over-budget transition. |
| 3. Tests pass | PASS | All 40 documented full-suite jobs passed; wrapper exit 0, no tripwire. Test/subtest events including repeated lanes: 95518 PASS, 0 FAIL, 327 SKIP. Every changed test passed twice, with no FAIL/SKIP. Policy, affected static analysis, native guards, full build and vet passed. Exact-head required GitHub CI also passed. |
| 4. No high-severity findings open | PASS | Original reviewer PASS and current exact-head MPR review have no unresolved HIGH findings. All three engagement streams were rechecked: only team quad341 comments, no review threads or external engagement. |
| 5. Final branch is clean | PASS | Exact PR-head checkout was clean before recording this gate. This record is the only audit-file addition; it introduces no implementation change. |
| 6. Branch diverges cleanly from main | PASS | merge-tree exit 0. Materialized tree equals the canonical merge-tree result; full go build ./... and go vet ./... passed there. Publication fetch and ls-remote confirm main still equals the tested base. No self-rebase needed. |
| 7. Single feature theme | PASS | Four workertest test/BUILD files implement one startup-outcome budget fix, accompanied only by the original gate record. Stack-aware ancestry guard returned 0 for ga-ea5y0t/ga-nse6wa/ga-e4bhca; no stack or denied paths. |

test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4

test_cmd_scope: full-suite

test_counts: 95518 PASS, 0 FAIL, 327 SKIP (test/subtest events; repeated lanes counted)

job_counts: 40 PASS, 0 FAIL, 0 SKIP

waiver_ref: none

failure_attribution: none required for the fresh full-suite run; zero failures.

policy_lane: make test-ci-policy PASS; make lint-affected fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=77844c68b420c7175c2a47da34b414413e02fe8c PASS (zero issues). Additional check-gomod-replace, check-native-dependency-surface, check-eventexport-isolation, check-core-boundary and test-native-doltlite-beads PASS. Full merged-tree build/vet PASS. Active .githooks verified with make check-hooks.

ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list change in this PR).

CI evidence: https://github.com/gastownhall/gascity/actions/runs/36227287725 — PASS, exact head 7085dca2f251bb8952932bf0a4459af65fbb1ddd. Matching required preflight/static/generated/acceptance, cmd/gc process, worker-core, integration and dashboard jobs passed.

Optional Bazel evidence: https://github.com/gastownhall/gascity/actions/runs/36227287793 — BUILD sync and bazel build PASS; changed //internal/worker/workertest:workertest_test PASS in 3.6s. Five unrelated sandbox-failure targets remain tracked by opened ga-okzuoh, whose pre-existing notes describe these exact failures (root UID65534, read-only /var/tmp, absent sandbox HOME). A sighting was appended; these are informational, outside the required CI fan-in, and not failures of this fresh local command. No waiver or blanket CI-success claim is made.

skip_justification: Unchanged platform/root-permission cases, helper subprocess entries, optional live services/provider profiles, legacy characterization tests, and fast/process/integration lane routing. The unrelated bd-store conformance skip explicitly cites pinned-bd compatibility tracker ga-e7z613; the upstream missing-issue probe lacks its optional CLI server fixture. Required process-backed cases execute in their documented process lanes. No changed test skipped.

Runtime: real host HOME, TMPDIR=/var/tmp, GOFLAGS=-v, normal shared Go/module caches. Rootless Podman socket active; cached Dolt 2.1.7 and SQL-server 2.1.7/2.2.0 tags verified; Ryuk disabled under the fleet cleanup recipe. Canonical merged-tree command ran through load and isolation wrappers.

Setup note: r1 did not execute tests because the explicit shard-log directory did not exist; it is zero test evidence. The directory was created and the fresh full r2 command ran to completion. Only r2 counts support this verdict.

load_threshold: 15

load_waited_seconds: 0

load_wait_timed_out: 0

load_start: 8.78

load_max: 43.85

load_mean: 23.03

Load sampler: samples=70, read_errors=0.

diff_tests_executed:

- TestPhase2StartupOutcomeBoundStaysAHangDetector: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeBounds: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeResultStillFailsWhenGenuinelyBroken: PASS (2 full-suite appearances).
- TestPhase2StartupOutcomeResultToleratesASlowButCorrectTransition: PASS (2 full-suite appearances).

Additional acceptance command: isolated-test-run.sh -- go test -count=5 -v -run ^TestPhase2StartupOutcomeBounds$ ./internal/worker/workertest; focused scope, 5/5 top-level PASS, exit 0, observed during the full sweep. Excluded from the full-suite counts above.

Evidence: /var/tmp/ga-ea5y0t-full-r2.log, /var/tmp/ga-ea5y0t-full-shards-r2/, /var/tmp/ga-ea5y0t-build.log, /var/tmp/ga-ea5y0t-vet.log, /var/tmp/ga-ea5y0t-policy.log, /var/tmp/ga-ea5y0t-static.log, /var/tmp/ga-ea5y0t-extra.log, /var/tmp/ga-ea5y0t-repeat.log, /var/tmp/ga-ea5y0t-bazel-ci.log.
