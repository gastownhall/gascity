# gc-5kx7ug — gascity: bound workflow-root reaper census

## Overview

Bounds the HQ Reaper's recursive workflow-root census so a large city store cannot monopolize Dolt past the connection timeout. The Reaper now evaluates deterministic candidate pages and fails closed: it performs no workflow-root mutation if any page or the candidate count fails.

## Changes

### Reaper query pressure

- Count eligible workflow-root candidates with a non-recursive query.
- Evaluate wisp and issue roots in deterministic `ORDER BY id` pages, configurable with `GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE` and defaulting to 25.
- Collect the complete closeable-root census before closing any roots.
- Replace the second recursive wisp update with an ID-bounded update after census completion.

### Safety and diagnostics

- Track SQL count and row-query success independently from empty results.
- Emit a `workflow ... root census incomplete` anomaly when any page fails and skip all root closes for that census.
- Fall back to the safe default batch size when the environment value is invalid.

### Regression coverage

- Prove a three-root census is assembled across two pages.
- Inject a failure on the second page and prove no partial root closes occur.
- Update the existing close, live-descendant preservation, and dry-run fixtures for the bounded query contract.

## Testing

- `bash -n internal/bootstrap/packs/core/assets/scripts/reaper.sh`
- `shellcheck internal/bootstrap/packs/core/assets/scripts/reaper.sh` (only the pre-existing dynamic include warning SC1091)
- `go test ./examples/gastown -run '^TestReaper' -count=1`
- `go test ./examples/gastown -count=1`
- `git diff --check`

## Notes

- Production evidence showed the unbounded `workflow_issue_root_candidates_base` / `workflow_descendants` query driving the central Dolt pressure during the controlled Flow Concourse canary.
- The production estate remains suspended until this change is built, installed through the governed release path, and passes the same pressure canary.
