# Release gate: compact quarantine operator alerts

Primary deploy bead: `ga-uz6mr1`  
Duplicate deploy bead for same release unit: `ga-e1wfde`  
Source implementation bead: `ga-f342m1.2`  
Review bead: `ga-6v9lg2`  
Release branch: `deploy/ga-uz6mr1-compact-alerts-v2`  
Reviewed head before gate: `29f847304baaf02497735833b77c12917cafce52`  
Current `origin/main` checked for mergeability: `cdcf685f54e956737941a7ca4654a76b545c8c9d`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist uses
the deployer release criteria from the active role prompt plus the repository
testing guidance in `TESTING.md`.

## Scope

The release branch contains one compact-script alerting slice:

| Path | Change |
| --- | --- |
| `examples/bd/dolt/commands/compact/run.sh` | Adds fire-and-forget compact quarantine event/mail alerts. |
| `examples/bd/dolt/dog_exec_scripts_test.go` | Adds validator coverage for alert delivery and recipient override. |

The broader `builder/ga-84xwd5.1` branch was not used because it carries
unrelated Dolt maintenance subsystem removal work. This gate uses the narrow
reviewed branch `origin/builder/ga-f342m1.2`.

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | `ga-6v9lg2` is closed `pass` and its notes record `VERDICT: PASS` for commit `29f847304baaf02497735833b77c12917cafce52` on `builder/ga-f342m1.2`. |
| 2 | Acceptance criteria met | PASS | `run.sh` now parses `GC_DOLT_COMPACT_ALERT_TO`, calls `send_compact_quarantine_alert`, emits `gc event emit dolt.compact.quarantine`, sends `gc mail`, alerts on fresh quarantine markers, existing quarantine markers in flatten/bare-GC paths, and stale pending-push markers. Validator tests cover default mayor, flatten/bare-GC existing markers, recipient override, and stale pending-push. |
| 3 | Tests pass | PASS | `go test ./examples/bd/dolt -run 'TestCompactScript(FreshQuarantineMarkerAlertsDefaultMayor\|ExistingQuarantineMarkerAlertsDefaultMayorBeforeFlattenAndBareGC\|QuarantineAlertRecipientCanBeOverridden\|StalePendingPushMarkerAlertsDefaultMayorBeforeManualReview)$' -count=1` passed in 1.377s. `go test ./examples/bd/dolt -run 'TestCompactScript' -count=1` passed in 43.422s. `make test-fast-parallel` passed all 8 fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-6v9lg2` list no findings and explicitly mark style, security, spec compliance, and coverage as passing. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file. This gate file is committed as the final release-gate evidence before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` succeeded and produced tree `f926f175b29b265cd9e76bb10c0fed03e381f4ff`; `git diff --check origin/main...HEAD` produced no output. |
| 7 | Single feature theme | PASS | The commit set touches only the Dolt compact script and its compact-script tests. It adds operator notifications for quarantine/pending-marker states without bundling maintenance removal, doctor, dashboard, API, or unrelated test-helper changes. |

## Test Commands

```bash
go test ./examples/bd/dolt -run 'TestCompactScript(FreshQuarantineMarkerAlertsDefaultMayor|ExistingQuarantineMarkerAlertsDefaultMayorBeforeFlattenAndBareGC|QuarantineAlertRecipientCanBeOverridden|StalePendingPushMarkerAlertsDefaultMayorBeforeManualReview)$' -count=1
go test ./examples/bd/dolt -run 'TestCompactScript' -count=1
make test-fast-parallel
go vet ./...
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
```

