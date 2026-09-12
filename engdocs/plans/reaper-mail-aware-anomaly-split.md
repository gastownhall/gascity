# Reaper Mail-Aware Anomaly Split Plan

Source bead: `ga-fkdeq`
Architecture parent: `ga-tqryc`
Priority: P2

## Goal

Stop false `ESCALATION: Reaper anomalies detected [MEDIUM]` mails caused
by message wisps while preserving mail-backlog visibility in the reaper
summary and allowing an opt-in mail backlog threshold.

## Work Packages

1. `ga-fkdeq.1` - Tests: reaper pins mail-aware open-wisp anomaly behavior
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: cover message wisps above `ALERT_THRESHOLD` without a
     reap-failure anomaly, `mail_wisps:600` in the summary, non-message
     wisps still triggering `open wisps (threshold: 500)`, and
     `mail_wisps:0` in the zero-backlog summary.

2. `ga-fkdeq.2` - As an operator, the canonical reaper stops mail-wisp false escalations
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-fkdeq.1`
   - Acceptance: update `packs/maintenance/assets/scripts/reaper.sh` with
     `GC_REAPER_MAIL_ALERT_THRESHOLD` defaulting to disabled, split Step 5
     into reapable and mail-wisp counts, preserve existing reap-failure
     wording, add mail-specific threshold wording, and always append
     `mail_wisps:N` to the summary.

3. `ga-fkdeq.3` - As an operator, the Gastown example reaper mirrors the mail-aware split
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-fkdeq.2`
   - Acceptance: apply the same threshold, counter, Step 5 split, and
     summary semantics to
     `examples/gastown/packs/maintenance/assets/scripts/reaper.sh`, keeping
     it behaviorally in sync with the canonical script.

4. `ga-fkdeq.4` - Verify: reaper mail-aware split is green across scripts
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Depends on: `ga-fkdeq.3`
   - Acceptance: run targeted maintenance script tests, confirm message
     wisps no longer trigger reap-failure escalation, confirm opt-in mail
     threshold behavior, confirm `mail_wisps:N` for nonzero and zero
     backlog cases, and record relevant quality-gate results.

## Dependency Graph

`ga-fkdeq.1` blocks `ga-fkdeq.2`, which blocks `ga-fkdeq.3`, which
blocks `ga-fkdeq.4`.

## Guardrails

- Do not filter reaper Steps 1-4 by `issue_type`.
- Do not add Dolt schema migrations; `issue_type` already exists.
- Do not change the mayor escalation mail subject.
- Keep `GC_REAPER_MAIL_ALERT_THRESHOLD=0` as the default-disabled state.
- Do not scope-creep into `ga-6qed8` message-wisp TTL behavior.
- Preserve the existing reap-failure anomaly wording for non-message wisps.
