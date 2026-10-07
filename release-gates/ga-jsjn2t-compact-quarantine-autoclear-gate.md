# Release gate: compact quarantine auto-clear with content-preservation proof

- Deploy bead: `ga-jsjn2t`
- Feature bead: `ga-wvarei`
- Feature review: `ga-4hwttw`
- Final review: `ga-mhq5yg`
- Reviewed source: `eadc0a9b4b4a77a237afad7daaa22a8ff468caa1`
- Base: `origin/main@1b991d75c3a2464296f939acd3dd712a25b0d89d`
- Isolated branch: `deploy/ga-jsjn2t-gate`
- Evaluated: 2026-09-11

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-4hwttw` records the round-2 feature verdict as PASS. `ga-mhq5yg` records `VERDICT: PASS` for the final two timeout-fixture commits and pins the exact reviewed source above. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | `flatten_database()` limits auto-clear to the four named race-class quarantine reasons. `diff_stat_preserved_tables()` re-runs `DOLT_DIFF_STAT` from the marker's pre-flight head to current HEAD and admits only tables with `rows_deleted=0` and `rows_modified=0`; invalid names, probe errors, unparseable values, unconfirmed tables, and all non-race-class reasons retain the marker and hard-block. A successful proof removes the marker, emits the alert/event, and falls through to flatten plus full GC in the same cycle. The six diff-owned tests below exercise the four success reasons, content-not-preserved drift, drift outside the proved set, probe failure, non-race reasons, same-cycle GC, marker retention/removal, and alert/event behavior; all passed. |
| 3 | Tests pass | **PASS** | The documented full-scope command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel` completed **37/40 jobs green**, with **49,985 PASS / 3 raw FAIL / 210 SKIP** top-level test executions. All three raw failures are attributed under criterion 3a. `test_cmd_scope: full-suite`. `diff_tests_executed: true` — `TestCompactScriptFixtureTimeoutsAccommodateLoadedHost`, `TestCompactScriptAutoClearsRaceClassQuarantineWhenDriftConfinedToKnownTables` (all four subtests), `TestCompactScriptQuarantineProbeFailureHardBlocksAutoClear`, `TestCompactScriptQuarantineDriftOutsideKnownTablesHardBlocksAutoClear`, `TestCompactScriptHardBlockQuarantineReasonsDoNotAutoClearAndAlert`, and `TestCompactScriptNonRaceClassQuarantineReasonsNeverAutoClear` all PASS with 0 diff-owned FAIL and 0 diff-owned SKIP. `waiver_ref: none`. Rootless Podman 5.8.4 was active before the run; Ryuk was disabled; cached `dolthub/dolt-sql-server:1.32.4` and `dolthub/dolt:2.1.7` matched the test pins. Full log: `/var/tmp/gc-deploy-ga-jsjn2t-full.log`; shard logs: `/var/tmp/gc-local-tests.QiWH4R`. |
| 3a | Pre-existing failures attributed | **PASS** | `failure_attribution: TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill -> ga-vkhfnj | clause 3(b), CROSS-PR` — the same 5-second `async starts did not finish` signature is recorded on `ga-rh76sz` from an unrelated container-policy diff. `failure_attribution: TestSendReloadControlRequestNoChange -> ga-vkhfnj | clause 3(b), CROSS-PR` — `ga-s9zeyf` records the same initial-reconcile timeout on a candidate changing only push-ownership shell guards; `ga-movzgb` explicitly consolidates this condition into `ga-vkhfnj`. `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a), MECHANISM` — the tracker independently proves the named-session suspend/reconciler retirement defect and authorizes attribution when a candidate does not touch that path. Both trackers predate this run and were opened; verified sightings were appended. None of the failing test paths overlaps this diff's two `examples/bd/dolt` paths. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` PASS. `go build ./...` PASS. `go vet ./...` PASS. `gofmt -l examples/bd/dolt/dog_exec_scripts_test.go` returned no paths, and `git diff --check origin/main...HEAD` PASS. |
| 3c | CI-config lane | **PASS** | `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The round-2 reviewer confirmed the original presence-only proof defect was resolved and recorded no remaining blocker. The final reviewer recorded no security, style, specification, or coverage blocker; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | Before the gate-file commit, the candidate worktree was clean at the exact reviewed source. Hooks path is `.githooks`; the isolated branch is committed and clean. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final fetch, `git merge-tree --write-tree origin/main eadc0a9b4b4a77a237afad7daaa22a8ff468caa1` returned 0 against `origin/main@1b991d75c3a2464296f939acd3dd712a25b0d89d`, producing tree `c24eec031d6c444e30c7ebfea3261525e40eb347`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The seven-commit range changes only the Dolt compact quarantine auto-clear proof and its directly coupled fixture/tests under `examples/bd/dolt`: one cohesive data-preservation safety feature. |

## Skip justification

The 210 skips are suite-controlled opt-ins and platform/provider exclusions already present outside this diff, including live-provider, Kubernetes, persistence, root-only, and helper-process cases. Every test added or modified by this change executed and passed; none skipped.

## Additional audit evidence

- `git diff --stat origin/main...eadc0a9b4b4a77a237afad7daaa22a8ff468caa1`: 2 files, 413 insertions, 4 deletions.
- Ancestry scope guard PASS for `ga-jsjn2t`, `ga-mhq5yg`, `ga-wvarei`, `ga-4hwttw`, `ga-jh1xqf`, and `ga-ssif8u`; the source range contains no `.claude/**` path or unrelated commit theme.
- No existing pull request carries the reviewed commit.
- No `PR-DESCRIPTION` block, `gc.pr_ping`, or unhandled PR-open instruction was present on the deploy bead.
