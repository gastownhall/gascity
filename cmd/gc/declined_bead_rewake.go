package main

import "github.com/gastownhall/gascity/internal/beads"

// noteAuthorLookup resolves the actor who authored the most recent
// notes-changing history event for a bead. Returns "" when no such event was
// found. A non-nil error means the lookup itself failed (e.g. the bd history
// subprocess errored) — callers must treat that as "skip", never as a match,
// so a transient lookup failure can never block or misreport the wake path.
type noteAuthorLookup func(beadID string) (actor string, err error)

// logDeclinedBeadRewakeIfDetected is a stub for GREEN (ga-jzb1as). It
// intentionally never logs yet, so TestLogDeclinedBeadRewakeIfDetected's
// positive-match case stays RED until the real detection lands.
func logDeclinedBeadRewakeIfDetected(_ beads.Bead, _ noteAuthorLookup, _ func(format string, args ...any)) {
}
