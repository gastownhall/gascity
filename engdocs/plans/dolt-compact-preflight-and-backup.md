# Dolt Compact Preflight and Backup

Date: 2026-06-17

PM intake source:
- Designer handoff mail: gm-wisp-bxr97qg
- Root beads: ga-wfdunn, ga-hn88dw
- Source: actual designer completed builder contracts

## Goal

Implement two completed `gc dolt compact` builder contracts:
- Stand down safely when the Dolt data volume is critically low on disk.
- Optionally sync a configured Dolt backup remote before destructive compact
  work begins.

Because both changes touch `examples/bd/dolt/commands/compact/run.sh` and the
same compact script fixture, the backup-sync implementation is gated behind the
disk-preflight implementation.

## Work Packages

### Disk preflight guard

- ga-wfdunn.1 -> builder: Implement disk-CRITICAL preflight guard.

Acceptance focus:
- Parse `GC_DOLT_COMPACT_MIN_FREE_BYTES` with default 5 GiB, disable value `0`,
  and invalid input exiting 2.
- Probe free bytes on `DOLT_DATA_DIR` after lock acquisition and before DB
  discovery or mutation.
- Fail open with a warning when `df` probing fails.
- Log `compact: disk CRITICAL` and exit 0 when free bytes are below threshold.
- Proceed silently when free bytes meet or exceed the threshold.
- Cover sufficient disk, critical disk, probe failure, zero threshold, and
  invalid env var with isolated fake `df` tests.

### Optional backup sync

- ga-hn88dw.1 -> builder: Implement optional pre-GC backup sync.

Acceptance focus:
- Parse and document `GC_DOLT_COMPACT_BACKUP_REMOTE` and
  `GC_DOLT_COMPACT_BACKUP_TIMEOUT_SECS`.
- When a backup remote is configured, run `dolt backup sync <remote>` from each
  DB directory before both flatten and bare-GC paths.
- Abort compaction with exit 1 on the first backup failure before destructive
  work starts for that DB.
- Skip DB directories without `.dolt` with a clear log.
- Preserve current behavior when backup remote is unset.
- Cover unset, success/failure for flatten, success/failure for bare GC,
  invalid remote, invalid timeout, and no-`.dolt` skip with isolated fake Dolt
  tests.

Dependency:
- ga-hn88dw.1 depends on ga-wfdunn.1 to serialize shared compact script and
  fixture edits.

## Handoff Targets

Builder:
- ga-wfdunn.1
- ga-hn88dw.1

Both child beads carry `source:actual-pm`, `ready-to-build`, and
`gc.routed_to=gascity/builder`. No design or validator routing is needed because
the designer handoff already supplied complete builder and test contracts.
