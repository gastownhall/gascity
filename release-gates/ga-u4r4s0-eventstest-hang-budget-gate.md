# Release gate: eventstest hang budget (round 4)

- Deploy bead: `ga-u4r4s0`
- Build bead: `ga-42mt5x.9`
- Review bead: `ga-xyv32m`
- Reviewed source: `eb8443960adb960206c1e72bb20776252d62e15e`
- Base checked: `origin/main@88e67b8b2fc6585af1543e5dad13e268b23e6509`
- Decision: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-xyv32m` is closed with an explicit PASS on the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | The two positive-delivery watcher waits in `internal/events/eventstest/conformance.go` now use the documented package `hangBudget = 6 * testutil.GoroutineRaceTimeout`. They remain hang detectors: passing delivery returns immediately, and no negative assertion window, timeout input, shared floor, or production path changed. |
| 3 | Tests pass | PASS | The full-suite run produced 45,547 PASS / 6 attributed FAIL / 188 SKIP. All conformance suites exercising the changed rotation waits passed in unit and integration lanes. Every raw failure is non-diff-owned, has an exact predated tracker, has a mechanism proof, and has no path overlap; attribution is recorded below. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no HIGH findings or other open blockers. |
| 5 | Final branch is clean | PASS | The exact reviewed source remained clean through the full-suite and static lanes; this checklist is the sole release-evidence addition. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded at the base above and produced tree `7707e0d840a4e345d97f59ce735aa1df02851864`; no self-rebase was required. |
| 7 | Single feature theme | PASS | One commit and one file implement a single test-infrastructure theme: derive positive-delivery rotation waits from a package hang-detector budget. |

## Test evidence

`test_cmd_scope: full-suite`

The first launch named the log directory before creating it and therefore ran no tests; it is a discarded setup attempt, not gate evidence. The valid run created the directory first and executed the unchanged full-scope command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-u4r4s0-full-gate-r4 \
make test-local-full-parallel
```

- `test_counts: 45547 PASS, 6 attributed FAIL, 188 SKIP`
- `diff_tests_executed: TestFileRecorderConformance/RotationPreservesInvariants PASS` in `unit-core` and `integration-packages-core-3-of-4`; `TestFakeConformance` and `TestExecConformance` also passed in every executed lane.
- `skip_justification:` the 188 SKIPs are pre-existing platform, optional-provider, or real-infrastructure cases. The diff adds no test file, and the conformance path containing both changed waits executed and passed.
- `waiver_ref: none`

### Failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestCatalogMatchesProductionWiringAndDocumentation` twice | `ga-1s16pf` | Clause 3(a): expired provider-ledger waivers in this reviewed checkout. The current base contains the independent expiry correction at `bd84f0172a5bc91097b262d8ba102f48ec01d96f`. Provider-ledger validation cannot import or execute eventstest, and paths do not overlap. |
| FAIL: `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clause 3(a): installed-`bd` flag-manifest drift. The candidate changes neither `internal/bdflags` nor the installed tool, eventstest is unreachable from this test, and paths do not overlap. |
| FAIL: both `TestGetKeyBinding_CapturesDefaultBinding` variants | `ga-afqddr` | Clause 3(a): host tmux 3.7b returns an empty filtered default keytable. Runtime/tmux cannot import eventstest, and paths do not overlap. |
| FAIL: `TestControllerDiscoversAddedCronOrderWithoutRestart` — initial reconcile timeout | `ga-64pxsy` | Clause 3(a): exact predated tracker for this full-suite load timeout. `cmd/gc` imports no eventstest helper, so the changed waits are unreachable; `cmd/gc/` and `internal/events/eventstest/` are disjoint paths. |

`failure_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-1s16pf | clause 3(a) mechanism — expired provider-ledger waiver rows; candidate unreachable`

`failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism — installed-bd manifest drift; candidate unreachable`

`failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr | clause 3(a) mechanism — host tmux keytable behavior; candidate unreachable`

`failure_attribution: TestControllerDiscoversAddedCronOrderWithoutRestart -> ga-64pxsy | clause 3(a) mechanism — tracked initial-reconcile load timeout; candidate unreachable`

## Required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go vet ./...` — PASS
- `make lint-new LINT_BASE=origin/main` with a fresh `/var/tmp` cache — PASS (`0 issues`)
- `make fmt-check` — PASS
- `git diff --check origin/main...HEAD` — PASS

## Round history

Rounds 1–3 failed on newly observed full-suite signatures whose trackers did not predate those runs. Builder bounce-backs independently established each non-diff-owned mechanism and recorded the resulting trackers. None of those same-run blockers recurred in round 4; the complete failure set above satisfies all four attribution clauses.
