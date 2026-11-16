package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A13's pinned skip of the runtime's restart marker (SESS-521, mc-nm6yt),
// legacy's clearRestartRequested on its pinned skip: under the runtime
// lease, on the row the pass read (the premise) and its Current runtime read
// fresh, the marker is removed. Nothing is stopped.

// markerClearSections are the clear's one section: the redecide, then the
// Call.
var markerClearSections = []section{called(0, section{Decide: markerClearStep}, clearRuntimeRestartMarker)}

// markerClearStep passes only a runtime alive and Current on the fresh read.
// Alive holds only on one session object across the read (txRuntime.Same),
// so the name the Call clears is the runtime the read proved the row's.
// A row that moved since the pass (unpinned, reset, held) is the premise's
// refusal, before this step.
func markerClearStep(v txView) txStep {
	if v.RT == nil || !v.RT.Alive() || compareIdentity(v.Row, v.RT.Identity) != identityCurrent {
		return txStep{Done: true, Cause: causeNotCurrent}
	}
	return txStep{Pass: v.Row}
}

// clearRuntimeRestartMarker removes GC_RESTART_REQUESTED from the row's
// runtime (and tmux's option mirror). A session already gone takes the
// marker with it; any other error is the Call's, and the next pass retries.
func clearRuntimeRestartMarker(_ context.Context, c txCaps, row session.Info) (struct{}, error) {
	name := strings.TrimSpace(row.SessionName)
	if name == "" {
		return struct{}{}, nil
	}
	if err := c.start.sp.RemoveMeta(name, "GC_RESTART_REQUESTED"); err != nil && !runtime.IsSessionGone(err) {
		return struct{}{}, fmt.Errorf("clearing %s's restart marker: %w", name, err)
	}
	return struct{}{}, nil
}
