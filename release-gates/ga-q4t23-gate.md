# Release Gate: ga-q4t23

Status: PASS

## Context

- Release bead: `ga-q4t23` - Review: reaper mail-wisp anomaly split recheck
- Source bead: `ga-fkdeq.2` - As an operator, the canonical reaper stops mail-wisp false escalations
- Source review bead: `ga-nmop3` - prior request-changes, closed after builder fix
- Reviewed branch: `builder/ga-fkdeq-2`
- Reviewed commit: `be8cd0a4980a99193e286b4a8b8bef376e121a66`
- PR branch: `release/ga-q4t23`
- Base: `origin/main` at `ed237aa31b6b0838e745c8759ff5c3fdb509ae2b`
- Project release manifest: `docs/PROJECT_MANIFEST.md` was not present in this checkout; this gate applies the active deployer role criteria.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-q4t23` notes contain `Reviewer verdict: pass` for commit `be8cd0a49`; reviewer reports findings none. |
| 2 | Acceptance criteria met | PASS | Checked source bead `ga-fkdeq.2` acceptance against `examples/gastown/packs/maintenance/assets/scripts/reaper.sh` and `examples/gastown/maintenance_scripts_test.go`; details below. |
| 3 | Tests pass | PASS | `bash -n examples/gastown/packs/maintenance/assets/scripts/reaper.sh`; `git diff --check origin/main...HEAD`; focused reaper `go test`; `go test ./examples/gastown -count=1`; `go vet ./...`; `make test` all passed. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-q4t23` list `Findings: none`; prior `ga-nmop3` MEDIUM coverage finding was fixed by adding `TestReaperMailAlertThresholdPositiveBranchEmitsMailBacklogAnomaly`. |
| 5 | Final branch is clean | PASS | Working tree was clean before writing this checklist; final cleanliness is rechecked after committing the gate file. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of `be8cd0a49`; `git merge-tree $(git merge-base builder/ga-fkdeq-2 origin/main) builder/ga-fkdeq-2 origin/main` produced no conflicts. |

## Acceptance Evidence

| Acceptance criterion | Evidence |
|---------------------|----------|
| `reaper.sh` defines `MAIL_ALERT_THRESHOLD` from `GC_REAPER_MAIL_ALERT_THRESHOLD` with default `0` and disabled note. | `reaper.sh:23` defines `MAIL_ALERT_THRESHOLD="${GC_REAPER_MAIL_ALERT_THRESHOLD:-0}"  # 0 = disabled`. |
| Script initializes `TOTAL_MAIL_WISPS` alongside other counters. | `reaper.sh:118` initializes `TOTAL_MAIL_WISPS=0`. |
| Step 5 splits reapable count using `issue_type NOT IN ('message')` and mail count using `issue_type = 'message'`. | `reaper.sh:499` filters non-message wisps; `reaper.sh:511` filters message wisps. |
| Reapable anomalies keep existing wording: `open wisps (threshold: N)`. | `reaper.sh:504` keeps `open wisps (threshold: $ALERT_THRESHOLD)`. |
| Mail backlog anomalies emit only when mail alert threshold is enabled and exceeded, using `open mail-wisps (mail threshold: N)`. | `reaper.sh:516-517` gates on threshold > 0 and emits the mail-specific wording. |
| Summary appends `mail_wisps:N` after `skipped_non_city_issues`. | `reaper.sh:577` appends `mail_wisps:$TOTAL_MAIL_WISPS` after `skipped_non_city_issues`. |
| Guardrail: Steps 1-4 not filtered by `issue_type`; no schema migration; escalation subject unchanged. | Diff is limited to `reaper.sh` Step 5/summary and `maintenance_scripts_test.go`; no migration files or escalation subject changes. |

## Test Evidence

- `git diff --check origin/main...HEAD`: PASS
- `bash -n examples/gastown/packs/maintenance/assets/scripts/reaper.sh`: PASS
- `go test ./examples/gastown -run 'TestReaper(MessageWispsAboveAlertThresholdDoNotTriggerReapFailureAnomaly|NonMessageWispsAboveAlertThresholdStillTriggerReapFailureAnomaly|MailAlertThresholdPositiveBranchEmitsMailBacklogAnomaly|MailWispsSummaryFieldAlwaysPresent)' -count=1`: PASS
- `go test ./examples/gastown -count=1`: PASS (`ok github.com/gastownhall/gascity/examples/gastown 25.392s`)
- `go vet ./...`: PASS
- `make test`: PASS (`observable go test: PASS`)
