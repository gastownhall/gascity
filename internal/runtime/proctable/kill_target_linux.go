//go:build linux

package proctable

// isInfrastructureKillTarget reports whether pid names tmux infrastructure (a
// tmux server or client) by its /proc comm — the same reading the Linux
// scanner uses to keep such processes out of the agent-root set, so the scan
// and the kill path cannot drift on what counts as infrastructure.
//
// It reads through scanRoot and deliberately bypasses liveScanGuard: that
// guard exists because a live /proc scan under go test can reap real agents
// (gastownhall/gascity#2839), and a single comm read reaps nothing.
//
// The predicate fails OPEN: an unreadable comm (the process is gone, or
// belongs to another user) reads as not infrastructure and the kill proceeds
// exactly as before the guard. Failing closed would silently disable orphan
// reaping on any procfs hiccup and let a genuine survivor race its
// replacement, which is the outage the kill exists to prevent.
func isInfrastructureKillTarget(pid int) bool {
	return isInfrastructureProcess(scanRoot, pid)
}
