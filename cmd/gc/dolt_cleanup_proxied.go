package main

import "io"

// proxiedScopeCleanupSkipMessage is what `gc dolt-cleanup` reports on a scope
// bd owns. The dolt pack's orders fire on every city, including proxied ones,
// so the command has to be a typed no-op there rather than a failure — but only
// the stages aimed at the city's own Dolt server are skipped. Naming which is
// which is the difference between an operator reading "nothing happened" and
// reading what did.
const proxiedScopeCleanupSkipMessage = "dolt lifecycle is owned by bd for proxied scopes; " +
	"skipped the port probe, database drop and disk purge, still reaped host orphans"

// cleanupSkipReasonProxiedScope tags the gc.dolt.cleanup.v1 skip envelope so
// automation can tell "bd owns this scope" apart from "nothing was found".
const cleanupSkipReasonProxiedScope = "bd-owned-proxied-scope"

// newProxiedScopeCleanupSkipReport builds the typed envelope a bd-owned
// proxied scope reports: no port, no drop, no purge.
func newProxiedScopeCleanupSkipReport() CleanupReport {
	return CleanupReport{
		OK:     true,
		Schema: CleanupSchemaVersion,
		// An explicit empty slice, not nil: the envelope is a contract and
		// its consumers iterate errors. A skip means "nothing went wrong",
		// which is not the same document as "errors": null.
		Errors: []CleanupError{},
		Skipped: &CleanupSkipped{
			Reason:  cleanupSkipReasonProxiedScope,
			Message: proxiedScopeCleanupSkipMessage,
		},
	}
}

// runProxiedScopeDoltCleanup is `gc dolt-cleanup` on a city whose Dolt server
// belongs to bd.
//
// bd starts that server, keeps it resident and stops it, so the stages aimed at
// it — resolving a managed port, probing it, dropping stale databases, purging
// their directories — have no subject here and are skipped by name. The orphan
// reap is a different job: it walks the host's process table for `dolt
// sql-server` processes nobody owns (leaked test servers under /tmp/Test*,
// gctest-*, deleted working directories) and never touches the city's own Dolt.
// Skipping it along with the rest meant the every-4h mol-dog-stale-db order,
// whose only front door is this command, stopped reaping anything at all on a
// default city while reporting a clean run. bd's own processes are protected
// where it counts, by the live proxy.pid ownership rule in classifyDoltProcess.
func runProxiedScopeDoltCleanup(opts cleanupOptions, stdout, stderr io.Writer) int {
	report := newProxiedScopeCleanupSkipReport()
	runReapStage(&report, opts)
	emitReport(report, PortResolution{}, opts, stdout, stderr)
	if opts.Force && hasFatalForceBlocker(&report) {
		return 1
	}
	return 0
}
