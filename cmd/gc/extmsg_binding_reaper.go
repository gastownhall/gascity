package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// reapStaleExtmsgBindings reconciles external-message conversation bindings
// against live session identity on each reconciler tick. A binding stores the
// session bead ID it was created against; when that session crashes and
// respawns under the same name it gets a fresh bead ID, leaving the binding
// pointing at a dead session so inbound triage silently drops and a fresh bind
// is rejected as a conflict. The reaper re-points bindings at the respawned
// session and clears bindings whose session is gone.
//
// It runs after session beads have been synced for the tick so a respawned
// session's replacement bead is already visible. Errors are logged and
// swallowed so a binding-store hiccup never stalls the reconciler loop.
func reapStaleExtmsgBindings(ctx context.Context, store beads.Store, now time.Time, stderr io.Writer) {
	if store == nil {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stats, err := extmsg.ReapStaleBindings(ctx, store, now)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: reaping stale extmsg bindings: %v\n", err) //nolint:errcheck
		return
	}
	if stats.Reassigned > 0 || stats.Cleared > 0 {
		fmt.Fprintf(stderr, "session reconciler: extmsg bindings reaped (reassigned=%d cleared=%d scanned=%d)\n", //nolint:errcheck
			stats.Reassigned, stats.Cleared, stats.Scanned)
	}
}
