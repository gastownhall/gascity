package proctable

// ManagedDoltScopeWatchdogVerb is the argv[1] re-exec marker of gc's managed
// Dolt scope watchdog (cmd/gc/dolt_scope_watchdog.go).
const ManagedDoltScopeWatchdogVerb = "__gc-managed-dolt-scope-watchdog"

// BDProxyChildVerb is the argv[1] verb of bd's database proxy supervisor
// (proxyendpoint.ChildVerb; duplicated here so this package stays free of the
// beads dependency tree, and pinned equal by a test).
const BDProxyChildVerb = "db-proxy-child"

// IsCityInfrastructureArgv reports whether argv is a long-lived city
// infrastructure process that can inherit an agent session's environment but
// is never that session's runtime: a tmux server or client, gc's managed Dolt
// scope watchdog, or bd's db-proxy-child. All are spawned Setpgid/Setsid,
// reparent to init once their spawner exits, and hold something the whole
// city depends on — one socket is one tmux server is every session in the
// city; the watchdog and proxy supervise its Dolt server — so terminating one
// as a session orphan takes the city down with it.
//
// tmux is classified by argv[0] with isInfrastructureCommand, the exact-match
// predicate both scanners use to keep it out of the agent-root set, so the
// scan and the kill paths cannot drift on what counts as tmux. The watchdog
// and proxy are classified by argv[1] alone: both are re-execs of whatever the
// operator's binary is called on disk.
func IsCityInfrastructureArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if isInfrastructureCommand(argv[0]) {
		return true
	}
	if len(argv) < 2 {
		return false
	}
	switch argv[1] {
	case ManagedDoltScopeWatchdogVerb, BDProxyChildVerb:
		return true
	}
	return false
}

// IsCityInfrastructureRoot reads pid's argv and reports whether it is city
// infrastructure per IsCityInfrastructureArgv.
//
// It is a kill-path fence, NOT part of the scanner: the scanner still reports
// a watchdog or proxy as a root (so its children are not promoted to roots in
// its place), and the callers that would terminate a scanned root — the orphan
// sweep, a session's pre-start orphan kill, and KillByPID itself — consult
// this first and leave it alone. An unreadable argv reports false, which keeps
// those callers' pre-fence behavior: failing closed would silently disable
// orphan reaping on any procfs hiccup and let a genuine survivor race its
// replacement. Under `go test` without an injected procfs root it reports
// false rather than read the host's live process table.
func IsCityInfrastructureRoot(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := liveScanGuard(); err != nil {
		return false
	}
	argv, err := rootArgv(pid)
	if err != nil {
		return false
	}
	return IsCityInfrastructureArgv(argv)
}

// RootStartIdentity returns pid's start-time token as the scanner would read
// it, or "" when it cannot be read. Callers use it to tell a recycled pid from
// the same process, e.g. to report a fenced root once rather than every sweep.
func RootStartIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	if err := liveScanGuard(); err != nil {
		return ""
	}
	return rootStartIdentity(pid)
}
