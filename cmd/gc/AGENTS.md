# CLI worktree safety

Worker directory pruning must preserve homes containing registered nested
worktrees or nested Git metadata, including ignored checkouts. Fail closed
when the safety scan cannot complete.
