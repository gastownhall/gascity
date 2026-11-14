package main

import "github.com/gastownhall/gascity/internal/beads"

// noteAuthorLookup resolves the actor who authored the most recent
// notes-changing history event for a bead. Returns "" when no such event was
// found. A non-nil error means the lookup itself failed (e.g. the bd history
// subprocess errored) — callers must treat that as "skip", never as a match,
// so a transient lookup failure can never block or misreport the wake path.
type noteAuthorLookup func(beadID string) (actor string, err error)

// logDeclinedBeadRewakeIfDetected logs when a bead's gc.routed_to matches the
// actor who authored its newest note -- the signature of the declined-bead
// re-wake loop (gm-6a5dt): a role declines a bead in its own note, but the
// wake path only reads gc.routed_to and re-wakes the same role anyway. It is
// a diagnostic side channel only: it never mutates or reroutes the bead, and
// a lookup failure degrades to a silent skip rather than blocking the wake
// path that calls it.
func logDeclinedBeadRewakeIfDetected(b beads.Bead, lookup noteAuthorLookup, logf func(format string, args ...any)) {
	routedTo := routedToOrLegacyWorkflowTarget(b)
	if routedTo == "" {
		return
	}
	actor, err := lookup(b.ID)
	if err != nil || actor == "" {
		return
	}
	if actor == routedTo {
		logf("declined-bead re-wake loop detected: bead %s is routed_to=%q but its newest note was authored by the same actor %q -- the wake path may be re-waking a role that already declined this bead (gm-6a5dt)", b.ID, routedTo, actor)
	}
}
