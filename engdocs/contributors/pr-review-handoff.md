# PR Review Handoff Notes

## Squash and Post-Merge Review Scope

When finalizing an adopted PR, the squash title and body must name every
substantive behavior change that lands in the commit. If a maintainer fixup
extends beyond the original PR title, include a short bullet for each added
scope in the squash body so post-merge reviewers, operators, and future bisects
can see the full change.

For PR #1513, the landed commit was titled for the polecat-to-refinery routing
fix, but it also changed two additional runtime behaviors:

- Supervisor-managed cities now keep per-city API routes unavailable until
  startup reconciliation has completed and `CityRuntime.OnStarted` marks the
  city running.
- Session transcript streams now rely on the log watcher loop's immediate first
  read after the caller's initial history load, so writes in that gap are
  reloaded before the stream blocks for later file notifications.

Future review finalization should record comparable bundled changes in the
public review comment or maintainer handoff notes before applying
`status/merge-ready`.
