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
// is never that session's runtime: gc's managed Dolt scope watchdog or bd's
// db-proxy-child. Both are spawned Setpgid/Setsid, reparent to init once their
// spawner exits, and supervise a Dolt server the whole city depends on, so
// terminating one as a session orphan takes the city's store down with it.
//
// argv[0] is deliberately ignored: both are re-execs of whatever the operator's
// binary is called on disk.
func IsCityInfrastructureArgv(argv []string) bool {
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
// such a process as a root (so its children are not promoted to roots in its
// place), and the callers that would terminate a scanned root — the orphan
// sweep and a session's pre-start orphan kill — consult this first and leave
// it alone. An unreadable argv reports false, which keeps those callers'
// pre-fence behavior. Under `go test` without an injected procfs root it
// reports false rather than read the host's live process table.
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
