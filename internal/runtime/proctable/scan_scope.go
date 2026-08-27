package proctable

// SessionScope carries facts the caller has already established from its own
// same-generation runtime observation, widening which unreadable owned
// processes an exact-incarnation scan may prove outside the session's
// reachable scope (ga-lp5w6). The zero value licenses nothing beyond the
// per-process proofs, so plain callers keep the narrower verdict.
type SessionScope struct {
	// TmuxSessionProvenAbsent reports that a COMPLETE tmux snapshot taken in
	// the same fresh observation generation did not contain the target
	// session. That fact licenses the live-pane-scope proof: with no live
	// target pane, an unreadable process whose parent chain is anchored in a
	// unique tmux pane spawn scope — its own or, for a fork that escaped it,
	// the nearest ancestor's — whose scope chain exits to a live,
	// pre-incarnation spawner not carrying the target session ID belongs to
	// some other pane, and cannot poison this session's absence proof.
	TmuxSessionProvenAbsent bool
}
