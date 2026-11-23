//go:build windows

package main

// posixShellAutomationEnabled reports whether the POSIX shell automation Gas
// City ships is in play on this platform.
//
// It is not, on Windows. The core pack's .sh assets, the bd provider script,
// the dolt pack commands and the doctor checks are POSIX shell, and the port
// leaves them disabled until each is ported or explicitly refused. jq, pgrep,
// lsof, flock and GNU timeout appear in the hard-dependency list because that
// automation invokes them; nothing else in gc does. Demanding them here would
// block a city on tools for workflows this platform will not run, in the one
// check whose entire job is to tell an operator what Gas City actually needs.
//
// This does not soften the backends themselves. bd and dolt are still required
// whenever the city's beads provider uses them, so a bd-backed city is still
// gated on its real binaries — only the shell helpers are conditioned. And a
// shipped .sh that is invoked anyway fails at exec with the script named,
// rather than proceeding as if it had run.
func posixShellAutomationEnabled() bool { return false }

// backendUnavailableOnThisPlatform reports a backend Windows cannot run at
// all, as distinct from one that is merely not installed.
func backendUnavailableOnThisPlatform(name string) (string, bool) {
	switch name {
	case "tmux":
		return "tmux does not run natively on Windows. Select herdr, the session provider that does: [session] provider = \"herdr\" in city.toml", true

	case managedDoltLifecycleDep:
		return "Gas City cannot own a Dolt server's lifecycle on Windows: it has no way to prove which process holds a PID, a data directory or a port, so a server it started could not afterwards be stopped or even reported on. Point this city at an external Dolt endpoint ([dolt] host/port in city.toml)", true

	default:
		return "", false
	}
}

// managedDoltLifecycleDepApplies reports whether gc would run its own Dolt
// server for this city, which is when the managed lifecycle is a requirement.
//
// Windows cannot run one, so the requirement has to follow the lifecycle's own
// classification (managedDoltLifecycleOwned) rather than the need for a local
// Dolt author identity. gc runs no server for a doltlite city, which keeps its
// beads in bd's local store, nor for a provider-owned one, whose server belongs
// to bd. A city that cannot be classified keeps the requirement, and the
// lifecycle reports why when it runs.
func managedDoltLifecycleDepApplies(cityPath string) bool {
	owned, err := managedDoltLifecycleOwned(cityPath)
	return err != nil || owned
}
