# Release gate: Dolt compact notification cadence

- Deploy bead: `ga-6dgrtc`
- Build bead: `ga-0fdnyn`
- Reviewed source: `b764a390f99c519c11b5fdc53119ec39d4c6113d`
- Base evaluated: `origin/main@7e72e01ab00974f43ebc7695767e2290deda3662`
- Deploy mode: `remote`
- Result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `gascity/reviewer` recorded a full PASS pinned to the exact reviewed source SHA. |
| 2 | Acceptance criteria met | **PASS** | Unchanged quarantine reasons now re-notify only after the configurable backstop, changed reasons still notify immediately, and stale pending-push/pending-GC alerts share the same marker-state deduplication. Alert reasons include `seen` and `age` context. Both new tests and all nine named regression tests passed in the full-suite output. |
| 3 | Tests pass | **PASS with attributed failures and one authorized waiver** | The documented 40-job CI-equivalent union completed 34 job PASS / 6 job FAIL. Test-level results were **46,675 PASS / 6 FAIL / 188 SKIP**. Both diff-owned tests passed twice. All six raw failures are preserved and resolved below; none is diff-owned. |
| 3a | Pre-existing failures attributable | **PASS** | Five failures have predating trackers plus exact-base/cross-PR or structural mechanism evidence and no path overlap. The sixth is the exact Beads #4566 signature covered by the mayor's standing authorization `ga-6bnc42`; it remains **FAIL-WAIVED** and was logged on `ga-lpfjhc` as required. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `make lint-affected LINT_BASE=origin/main`, `git diff --check origin/main...HEAD`, `gofmt -l` on the added test, and `bash -n` on the changed script all passed. The affected-lint target reported no lintable changed production package; full-repo vet covered the Go change. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no blocking or HIGH finding; only a non-blocking warning that open PR #5493 will require textual reconciliation if it lands later. |
| 5 | Final branch clean | **PASS** | The reviewed source worktree was clean at `b764a390f99c519c11b5fdc53119ec39d4c6113d` before adding this gate artifact. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main b764a390f99c519c11b5fdc53119ec39d4c6113d` returned 0 and produced tree `1c5dc185ab169075710bc30dfd3f96496b3d39b2`; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two commits change one subsystem: compact marker notification cadence in `examples/bd/dolt`, plus its package-local regression tests. |

## Acceptance evidence

The full-suite integration package log records PASS for:

- `TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle`
- `TestCompactScriptQuarantineRenotifiesAfterBackstopElapses`
- `TestCompactScriptQuarantineReasonChangeReMails`
- `TestCompactScriptExistingQuarantineMarkerAlertsOnceAcrossRepeatedCycles`
- `TestCompactScriptQuarantineMailFailureIsRetriedNextCycle`
- `TestCompactScriptUnreadableQuarantineMarkerIsNotClobbered`
- `TestCompactScriptQuarantineAlertRecipientCanBeOverridden`
- `TestCompactScriptSkipsRemotePhaseForNoSyncDatabase`
- `TestCompactScriptClearsPendingPushMarkerWhenDatabaseBecomesNoSync`
- `TestCompactScriptDryRunPreservesPendingPushMarkerForNoSyncDatabase`
- `TestCompactScriptPendingPushMarkerBlocksFlattenEvenWhenFresh`

## Test evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-6dgrtc-full-gate make test-local-full-parallel
test_cmd_scope: full-suite
test_jobs: 34 PASS, 6 FAIL
test_counts: 46675 PASS, 6 FAIL, 188 SKIP
diff_tests_executed: TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle PASS (unit + integration); TestCompactScriptQuarantineRenotifiesAfterBackstopElapses PASS (unit + integration)
waiver_ref: ga-6bnc42 — mayor standing authorization dated 2026-08-18, exact gastownhall/beads#4566 signature only
```

The rootless Podman socket was live and `dolthub/dolt-sql-server:1.32.4` was pulled and inspected before the run. The 188 skips are pre-existing suite-controlled opt-ins and platform/privilege exclusions; none belongs to the diff. They include live provider/Kubernetes tests, persistence tests requiring an opt-in installed Beads build, Darwin/root-only cases, and test helpers. The count matches the immediately preceding full-suite gate run on an unrelated candidate.

This diff adds two tests, so `added_test_load=yes`. No failure used the inconclusive attribution path: each has structural, exact-base, or cross-PR proof, except the one signature explicitly covered by a pre-existing merge-authority waiver.

### Raw failures and disposition

| Raw failing test | Disposition |
|---|---|
| `TestBdFlagManifestCurrent` | Attributed to `ga-f0uceo`, created 2026-08-15. Clause 3(a): the installed `bd --help` surface and `internal/bdflags` manifest are unreachable from this compact-script diff; the identical signature has exact-base and many unrelated-candidate reproductions. No path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding` | Attributed to `ga-afqddr`, created 2026-08-15. Clause 3(a): host tmux 3.7b returns an empty filtered default key table; this compact-script diff cannot alter `internal/runtime/tmux` or host tmux. No path overlap. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | Attributed to `ga-afqddr`, same proof and no path overlap. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | **FAIL-WAIVED**, not rewritten green. Tracker `ga-lpfjhc`; exact signature: pending schema migrations alter pre-existing dirty table `dependencies` (gastownhall/beads#4566). The failure occurs during fixture `gc init`, before the formula path; the compact command is not invoked by store bootstrap. Authorized by `ga-6bnc42`, and this occurrence was appended to `ga-lpfjhc` before gate completion. |
| `TestE2E_SuspendResume_City` | Attributed to `ga-yc0e3a`, created 2026-08-18. Clause 3(d)/cross-run proof: exact base previously failed with the same missing `citysus.report` timeout, and unrelated candidates reproduce it. This test does not invoke the compact command; no path overlap. |
| `TestCleanInstallTutorialPath` | Attributed to `ga-2ywyyf`, created before this run. Clause 3(b): the exact unexpected pre-existing `.beads` store signature occurred on unrelated reviewed candidates, including the immediately preceding gate. The current failure occurs at `gc rig add`, before compact notification code can execute; no path overlap. |

Full raw logs are retained in `/var/tmp/ga-6dgrtc-full-gate` on the gate host.
