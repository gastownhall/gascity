# Mail Inbox Assignees And Retention Deploy Split

Source PM beads: `ga-tftexi`, `ga-yinjm2`, `ga-ga91go`, `ga-a5muun`, `ga-93j6pj`
Owner: `gascity/pm`
Created: 2026-05-31
Priority: P2

## Goal

Unblock deployment of the reviewed `ga-2znrco` mail inbox and retention
slices without opening a multi-theme PR.

The deployer rejected the original deploy beads because the source branches
are stacked on coordstore/SQLite work that is not on `origin/main`. The
beadmail Assignees branch also carries unrelated session/events commits after
the reviewed target commit.

## Branch Findings

- `builder/ga-2znrco.1-listquery-assignees` ends at `105d0224d` but includes
  the coordstore/SQLite stack through PR #2738 before the target commit.
- `builder/ga-2znrco.2-hqstore-assignees` ends at `8a6de11d9` and is stacked
  on the ListQuery slice plus the coordstore/SQLite stack.
- `builder/ga-2znrco.3-beadmail-assignees` contains `7bd1edc8f`, then
  unrelated `d2f2e8bb9` and `64aa34ed0` commits. Only `7bd1edc8f` belongs to
  the beadmail Assignees deploy slice.
- `builder/ga-2znrco.4-mail-retention` ends at `30cfff225` but includes the
  coordstore/SQLite stack before the target commit.
- `builder/ga-2znrco.5-mail-retention-purge` ends at `83a724bc2` and is
  stacked on the retention config slice plus the coordstore/SQLite stack.

PR #2738 is currently open, clean, and mergeable, but these mail deploy beads
must not bundle it unless it has already landed on `main` before the deployer
processes the child bead.

## Work Packages

| Bead | Route | Source | Target commit | Acceptance focus |
|------|-------|--------|---------------|------------------|
| `ga-tftexi.1` | `gascity/deployer` | `ga-tftexi` | `105d0224d` | Isolated ListQuery.Assignees contract deploy |
| `ga-yinjm2.1` | `gascity/deployer` | `ga-yinjm2` | `8a6de11d9` | Isolated HQStore Assignees index-union deploy |
| `ga-ga91go.1` | `gascity/deployer` | `ga-ga91go` | `7bd1edc8f` | Isolated beadmail Assignees routing deploy |
| `ga-a5muun.1` | `gascity/deployer` | `ga-a5muun` | `30cfff225` | Isolated mail retention_ttl config deploy |
| `ga-93j6pj.1` | `gascity/deployer` | `ga-93j6pj` | `83a724bc2` | Isolated read-message purge deploy |

Each child bead requires the deployer to run the normal deploy gate against a
branch or PR whose diff from current main contains only that slice plus the
release-gate artifact. The coordstore/SQLite PR #2738 stack is allowed in the
diff only if it has already merged to main before the deployer handles the
child bead.

## Dependency Graph

- `ga-tftexi.1` blocks `ga-ga91go.1`.
- `ga-yinjm2.1` is superseded for the beadmail routing deploy because
  HQStore was removed from main. It no longer blocks `ga-ga91go.1`.
- `ga-yinjm2.1.1` tracked PR #2870 merge authority and is now moot because
  PR #2870 was closed unmerged after HQStore removal. It no longer blocks
  `ga-ga91go.1`.
- `ga-a5muun.1` blocks `ga-93j6pj.1`.
- The Assignees chain and retention chain can proceed independently once each
  child deploy branch is isolated from unrelated stacked work.

## Current Updates

- `ga-ga91go.1` remains valid after architecture decision `ga-ga91go.1.1`.
  PM removed the obsolete `ga-yinjm2.1` and `ga-yinjm2.1.1` blockers on
  2026-06-01 and routed the bead back to `gascity/deployer`.
- `ga-ga91go.1` remains current-main-only and must not proceed as a stacked
  deploy. The deployer must cherry-pick only `7bd1edc8f` from
  `builder/ga-2znrco.3-beadmail-assignees`; do not include `8a6de11d9`,
  `d2f2e8bb9`, or `64aa34ed0`.

## Handoff

The original failed deploy beads were closed as superseded by the PM split
children. The children carry `needs-deploy`, `source:actual-pm`, and
`gc.routed_to=gascity/deployer`.

The deployer should record the final gate result, PR or merge URL, and final
head commit on each child bead, then report the result to mayor.
