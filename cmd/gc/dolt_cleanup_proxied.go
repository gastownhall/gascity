package main

import (
	"fmt"
	"io"
)

// proxiedScopeNoOpMessage is the one line every managed-Dolt verb prints when
// it is asked to operate on a scope bd owns. The dolt pack's orders fire on
// every city, including proxied ones, so the verbs have to be no-ops there
// rather than failures.
const proxiedScopeNoOpMessage = "dolt lifecycle is owned by bd for proxied scopes; nothing to do"

// cleanupSkipReasonProxiedScope tags the gc.dolt.cleanup.v1 skip envelope so
// automation can tell "bd owns this scope" apart from "nothing was found".
const cleanupSkipReasonProxiedScope = "bd-owned-proxied-scope"

// newProxiedScopeCleanupSkipReport builds the typed zero envelope a
// bd-owned proxied scope reports instead of running any cleanup stage.
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
			Message: proxiedScopeNoOpMessage,
		},
	}
}

// emitProxiedScopeCleanupSkip writes the no-op report in whichever shape the
// caller asked for. It probes nothing and writes no managed-Dolt state.
func emitProxiedScopeCleanupSkip(jsonOut bool, stdout io.Writer) {
	if jsonOut {
		emitReport(newProxiedScopeCleanupSkipReport(), PortResolution{}, cleanupOptions{JSON: true}, stdout, io.Discard)
		return
	}
	fmt.Fprintf(stdout, "%s\n", proxiedScopeNoOpMessage) //nolint:errcheck
}
