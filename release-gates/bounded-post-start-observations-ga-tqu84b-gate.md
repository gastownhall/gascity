# Release gate: bounded post-start session observations

**Verdict:** **PASS**

- Deploy bead: `ga-tqu84b`; build bead: `ga-ka32kn`; review bead: `ga-oc60ah`.
- Reviewed source: `cab9c26803fa783a7523c84d8e7fd86c995b105a` (resolved commit).
- `gate_base`: `b0c59549c6547653c2176c583489fa0bc83f9d0b` (`origin/main` when criterion 6 passed).
- Tested merge tree: `6e3da17f2ea0942c0d22cccf95d0bccbd8fa0350`, materialized in a clean scratch worktree with the pinned base as first parent and the reviewed source as second parent.
- Current `origin/main` at final freshness check: `9fdc52d82c7d5289e9e3af7749c00fa17999b35f`; merging the reviewed source remains conflict-free. The full suite and fast lanes ran on the pinned merge tree, as required by the gate-base protocol.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | **PASS.** `ga-oc60ah` records PASS at the resolved source commit; no review carryover is claimed. |
| 2 | Acceptance criteria met | **PASS.** C1: `worker.ObserveBounded` returns an explicit unavailable error on expiry and preserves an answer ready at the bound. C2: the three post-start observations use a fixed 10-second bound from the controller context and defer without rollback or holding the start slot. C3: failure-path screen and identity probes share one 10-second budget and do not guess an identity when unanswered. C4: one outstanding observation per city/session key limits abandoned goroutines. C5: fake-time and edge-case tests cover each call site, budget sharing, prompt answers, late answers, panic, key independence, and slot release. C6: worker boundary and session requirements are documented. C7: the audited PR description inventories the remaining calls and tracks follow-ups. C8: build, vet, lint and full tests pass. C9: the PR references issue #7170 without claiming its other parts are closed. |
| 3 | Tests pass | **PASS.** Documented full-scope `make test-local-full-parallel` on the pinned merge tree: 40/40 jobs passed, **57,728 PASS, 0 FAIL, 235 SKIP** at top-level test granularity. All 22 diff-owned tests passed by name in both applicable lanes; 0 failed, skipped, or absent. `TestTutorial01` passed in `cmd-gc-process-4-of-6.log`; `TestSessionEventPumpLiveHerdr` passed in `cmd-gc-process-3-of-6.log:16609`. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 4 | No unresolved high-severity findings | **PASS.** Reviewer reports 0 blocking or HIGH findings; N1-N3 are nonblocking comment, observability, and PR-text notes. The audited review-bead PR text is carried verbatim. |
| 5 | Final branch clean | **PASS.** Materialized test worktree was clean after the suite. The isolated deploy branch was clean after the gate commit (`git status --porcelain` empty before push). |
| 6 | Branch diverges cleanly from main | **PASS.** `git merge-tree --write-tree` exited 0 at the pinned base and again against current `origin/main`; no conflict. `go build ./...` and `go vet ./...` passed on the pinned merge tree. |
| 7 | Single feature theme | **PASS.** The nine changed files implement and test one boundary: bounded observations after `provider.Start`. BUILD registration and worker/session documentation support that same change. |

## Criterion 3 detail

- `test_cmd`: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env HOME=/tmp/gtq.Dcc3pD XDG_CONFIG_HOME=/tmp/gtq.Dcc3pD/.config DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOCACHE=/var/tmp/gc-heavy-gate/runs/ga-iufo27.c3-diskcache/build-cache TMPDIR=/var/tmp/gotmp SHELL=/bin/sh GOFLAGS=-v LOCAL_TEST_JOBS=4 LOCAL_TEST_LOG_DIR=/var/tmp/gc-heavy-gate/runs/ga-tqu84b.c3-short-home/logs make test-local-full-parallel`, detached as `gc-heavy-ga-tqu84b.c3-short-home-1791311941-1224195.service`; `GATE_RUN_EXIT rc=0 state=complete`.
- `test_log_dir`: `/var/tmp/gc-heavy-gate/runs/ga-tqu84b.c3-short-home/logs` (retained). `gate-test-evidence.py` read all 40 job logs; 36 carried test result lines and four were non-test checks.
- `diff_tests_executed`: nine `cmd/gc` and thirteen `internal/worker` tests, each PASS by name. The owned-test list was generated from the reviewed diff; the parser found each in both unit/process and integration output as applicable.
- `skip_justification`: all 235 skips are outside the 22 diff-owned tests. The only changed test files are new and contain no skip calls. Existing skips include platform/provider availability, helper-process guards, explicit lane gates, and optional external-service requirements; none is used to excuse a changed test.
- `failure_attribution`: not applicable; corrected full suite has zero failures. Initial attempt at `/var/tmp/gc-heavy-gate/runs/ga-tqu84b.c3` was stopped after the existing `ga-0og0ry` herdr Unix-socket path condition appeared with an overlong private HOME. Its logs remain retained, but that invalid setup supplies no gate verdict. The corrected full suite used a 15-byte private HOME and the same pinned merge tree; the herdr test passed.
- `policy_lane`: PASS. In a separate fresh tree with `gate-base.sh run` pinned to `b0c59549c6547653c2176c583489fa0bc83f9d0b`, `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads` passed; log `/var/tmp/ga-tqu84b-policy.log`. Pinned golangci-lint 2.12.0 matched.
- `drift_lane`: PASS. Separate fresh tree ran `make bazel-sync` and `git diff --exit-code` with no generated BUILD drift; log `/var/tmp/ga-tqu84b-bazel-drift.log`. `git diff --check` passed.
- `ci_lane_run`: not applicable; the diff changes no workflow job, matrix, timeout, or required-check configuration. BUILD files were verified by the drift lane.
- `heavy_mode`: none (`heavy-composite-gate.sh classify`); the documented ordinary full suite applies.
- Test environment: rootless Podman socket configured, cached Dolt SQL server image `2.2.0` matches `deps.env`, pinned test bd v1.3.1 matched its reference. The isolation wrapper reported PASS-THROUGH for the disposable merge scratch with no inherited `BD_`, `BEADS_`, `GC_`, or `DOLT_` selectors; the wrapped suite used the explicit private HOME and container variables above.
- Load evidence: `load_threshold=15`, `load_waited_seconds=1801`, `load_wait_timed_out=1`; `run_start_load=21.16`, `run_max_load=67.35`, `run_mean_load=33.52`, `run_readings=86`; `wait_first_load=33.60`, `wait_max_load=48.30`, `wait_mean_load=32.90`, `wait_readings=61`, `read_errors=0`. The ordinary gate proceeded after its bounded wait, as documented.

### Diff-owned tests verified by name

```text
cmd/gc TestEnqueuePreparedStartWaveReleasesItsSlotWhenAPostStartObservationHangs PASS
cmd/gc TestObserveSessionBoundedLimitsOutstandingObservationsPerCity PASS
cmd/gc TestRunPreparedStartCandidateBoundsThePendingCreateIdentityReads PASS
cmd/gc TestRunPreparedStartCandidateBoundsTheRateLimitScreenPeek PASS
cmd/gc TestRunPreparedStartCandidateDefersWhenAPostStartObservationHangs PASS
cmd/gc TestRunPreparedStartCandidateFailureHelpersShareOneBudget PASS
cmd/gc TestRunPreparedStartCandidateLeavesOneAbandonedObservationPerSession PASS
cmd/gc TestRunPreparedStartCandidateSkipsTheCollisionObservationAfterTheStartDeadline PASS
cmd/gc TestRunPreparedStartCandidateUsesAnObservationAnswerJustBeforeTheBound PASS
internal/worker TestObserveBoundedAlreadyExpiredContextStartsNothing PASS
internal/worker TestObserveBoundedAnswerJustBeforeTheBoundIsUsed PASS
internal/worker TestObserveBoundedAnswerReadyWhenTheContextEndsIsUsed PASS
internal/worker TestObserveBoundedForwardsObservationPanicToCaller PASS
internal/worker TestObserveBoundedKeysAreIndependent PASS
internal/worker TestObserveBoundedLeavesOneAbandonedObservationPerKey PASS
internal/worker TestObserveBoundedObservationThatEndsWithoutReturningIsUnavailable PASS
internal/worker TestObserveBoundedOutstandingKeyAnswersUnavailableWithoutSpawning PASS
internal/worker TestObserveBoundedRejectsBlankKey PASS
internal/worker TestObserveBoundedReservationClearsWhenAbandonedObservationReturns PASS
internal/worker TestObserveBoundedReturnsPromptAnswer PASS
internal/worker TestObserveBoundedSequentialAnswersNeverReportOutstanding PASS
internal/worker TestObserveBoundedWedgedObservationReturnsUnavailableAtTheBound PASS
```
