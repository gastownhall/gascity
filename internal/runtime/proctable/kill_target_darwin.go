//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/pidutil"

// killTargetCmdline is indirected so the classifier's argv handling can be
// covered without a live tmux process.
var killTargetCmdline = pidutil.Cmdline

// isInfrastructureKillTarget reports whether pid names tmux infrastructure (a
// tmux server or client) by argv[0] — the token the Darwin scanner classifies,
// since ps's first command token is argv[0], possibly a path, and tmux does not
// retitle itself on Darwin. argv comes from kern.procargs2, so the
// classification adds no subprocess to the kill path.
//
// The predicate fails OPEN: an unreadable argv (the process is gone, or
// belongs to another user) reads as not infrastructure and the kill proceeds
// exactly as before the guard. Failing closed would silently disable orphan
// reaping on any procargs2 hiccup and let a genuine survivor race its
// replacement, which is the outage the kill exists to prevent.
func isInfrastructureKillTarget(pid int) bool {
	argv, err := killTargetCmdline(pid)
	if err != nil || len(argv) == 0 {
		return false
	}
	return isInfrastructureCommand(argv[0])
}
