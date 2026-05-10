# Release Gate — ga-9shf (gc port-drift detection)

**Bead:** ga-9shf — Fix: gc should detect/warn on bd-vs-gc port drift in managed-city mode
**Review bead:** ga-3hgw
**Branch:** `release/ga-9shf`
**Commit on branch:** a1e4b49a (cherry-pick of 85957381 onto `origin/main`)
**Evaluator:** gascity/deployer-1 on 2026-04-23

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-3hgw notes: `review_verdict: pass` from gascity/reviewer-1, single-pass (gemini second-pass disabled). |
| 2 | Acceptance criteria met | PASS | See matrix below. |
| 3 | Tests pass | PASS (w/ caveats) | Targeted ga-9shf tests all PASS on `release/ga-9shf`. Two pre-existing failures (`TestSyncConfiguredDoltPortFilesSkipsNonBDProviders`, `TestDoRigSetEndpointRejectsNonBDProvider`) are confirmed to fail on `origin/main` at `adaa6f47` as well, so they are not regressions from this change. |
| 4 | No high-severity review findings open | PASS | All three findings in ga-3hgw are `severity: info` (ux-message polish, doc-consistency, PID-recycle race — reviewer marked not blocking). |
| 5 | Final branch is clean | PASS | `git status` clean (no tracked modifications). One untracked `.gitkeep` is not part of the release; not staged. |
| 6 | Branch diverges cleanly from main | PASS | `git log origin/main..HEAD` = one commit, no merge conflicts with `origin/main`. |

## Acceptance criteria matrix (ga-9shf done-when)

| Criterion | Met | Evidence |
|-----------|-----|----------|
| `gc doctor` drift detector covers three drift patterns and exits non-zero | YES | `cmd/gc/cmd_doctor_drift.go` registers `dolt-drift` check; covers Case A (live rig-local Dolt under inherited_city → Error), Case B (port-file mismatch → Error, names both ports), Case C (stale sql-server.info → Warning). Tests: `TestDoltDriftCheck*` all pass. |
| `gc start` logs WARN for each rig whose port file it rewrote | YES | `writeDoltPortFile` now accepts a warn writer; `startBeadsLifecycle → normalizeCanonicalBdScopeFiles → syncConfiguredDoltPortFiles` threads stderr through. Silent callers pass `io.Discard`. Test: `TestSyncConfiguredDoltPortFilesWarnsOnRigPortFileRewrite` passes. |
| `gc rig set-endpoint --self` under managed_city requires `--force` | YES | `cmd/gc/cmd_rig_endpoint.go` adds `--self/--port/--force` flags; validation rejects `--self+host/user`, requires `--port`, rejects `--force` outside `--self`. Managed-city guard emits one-line WARN on success. Tests: `TestValidateRigEndpointOptionsSelf*`, `TestDoRigSetEndpointSelf*` pass. |
| Integration tests exist for detector and start-time warning | YES | `cmd/gc/cmd_doctor_drift_test.go`, `cmd/gc/beads_provider_lifecycle_test.go` (warn test), `cmd/gc/cmd_rig_endpoint_test.go` (self/force tests). |
| No behavior change to `syncConfiguredDoltPortFiles` rewrite semantics | YES | Only warn-writer plumbing added; rewrite path untouched (reviewer confirmed by tests on unmodified defense checks). |

## Test evidence

```
$ go test ./cmd/gc -run "TestDoltDriftCheck|TestRigLocalDoltPIDFromSQLServerInfo|TestSyncConfiguredDoltPortFiles|TestValidateRigEndpointOptions|TestDoRigSetEndpoint" -count=1
... 2 FAILURES: TestSyncConfiguredDoltPortFilesSkipsNonBDProviders, TestDoRigSetEndpointRejectsNonBDProvider
```

Baseline check at `origin/main` (adaa6f47):

```
$ git checkout origin/main
$ go test ./cmd/gc -run "TestSyncConfiguredDoltPortFilesSkipsNonBDProviders|TestDoRigSetEndpointRejectsNonBDProvider" -count=1
... same 2 FAILURES
```

Both failures are pre-existing on main and not introduced by a1e4b49a. Reviewer flagged them in ga-3hgw `pre_existing_failures`. A separate bead should track them.

```
$ go vet ./cmd/gc/...
(clean)

$ go build ./...
(clean)
```

## Security review

From ga-3hgw, OWASP Top 10 walkthrough: no new attack surface. Port parsed via `strconv.Atoi` with `>0` guard. `--force` is an operator-mistake guard, not a security boundary. The change reduces misconfig risk by making silent port-file rewrites visible. No blocking findings.

## Verdict: PASS

Cleared for PR.
