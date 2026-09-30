package main

import (
	"context"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookBlockerStoreOpener opens the bd workspace a work query just read, so the
// blocked-outcome veto reads a candidate's blockers from the store that holds
// them.
type hookBlockerStoreOpener func(ctx context.Context, dir string, env []string) beads.ExactBatchGetter

// filterBlockedOutcomeHookCandidates is the red-phase stub: it vetoes nothing.
func filterBlockedOutcomeHookCandidates(candidates string, _ beads.ExactBatchGetter) (string, error) {
	return candidates, nil
}

// withHookBlockedOutcomeVeto is the red-phase stub: it wraps nothing.
func withHookBlockedOutcomeVeto(run hookStoreRunner, _ hookBlockerStoreOpener, _ io.Writer) hookStoreRunner {
	return run
}
