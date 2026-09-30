package main

import (
	"fmt"
	"io"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Drain-skip lines ("Skipping drain for 'x': live assigned work found", ...)
// describe standing conditions. The reconciler is level-triggered, so a seat
// that legitimately holds live work re-reaches the same skip on every tick and
// used to print the identical line each time (about 190 lines/hour per held
// seat). The skip decision itself is unchanged and the per-tick trace records
// still carry every decision; only the stdout line is deduplicated per session:
// it prints when the skip starts or its message changes, and is re-armed as
// soon as a tick sees the session without that skip.

// drainSkipAbsentRetention bounds how long a mark survives for a session that
// is absent from the reconcile feed. The control-dispatcher tick reconciles a
// filtered feed, so absence alone does not mean the session is gone; the
// retention only has to outlive the patrol interval between full ticks while
// still pruning marks left behind by sessions that closed mid-skip.
const drainSkipAbsentRetention = 24 * time.Hour

// drainSkipMark records the last drain-skip line printed for one session.
type drainSkipMark struct {
	message string
	notedAt time.Time
	noted   bool // re-observed since the last sweep
}

// noteDrainSkip records that the session hit a drain skip described by
// message and reports whether the line should be printed: true for the first
// sighting, for a changed message, or when there is no tracker to dedupe
// against.
func (dt *drainTracker) noteDrainSkip(beadID, message string, now time.Time) bool {
	if dt == nil || beadID == "" {
		return true
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if dt.drainSkips == nil {
		dt.drainSkips = make(map[string]*drainSkipMark)
	}
	mark, ok := dt.drainSkips[beadID]
	if ok && mark.message == message {
		mark.notedAt = now
		mark.noted = true
		return false
	}
	dt.drainSkips[beadID] = &drainSkipMark{message: message, notedAt: now, noted: true}
	return true
}

// sweepDrainSkips runs once at the end of a reconcile pass. A session that was
// in this pass's feed but did not hit a skip has left the skip condition, so
// its mark is dropped and a later skip prints again. A session absent from the
// feed keeps its mark until drainSkipAbsentRetention passes.
func (dt *drainTracker) sweepDrainSkips(present map[string]sessionpkg.Info, now time.Time) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	for id, mark := range dt.drainSkips {
		_, inFeed := present[id]
		switch {
		case mark.noted:
			mark.noted = false
		case inFeed:
			delete(dt.drainSkips, id)
		case now.Sub(mark.notedAt) > drainSkipAbsentRetention:
			delete(dt.drainSkips, id)
		}
	}
}

// logDrainSkip prints a drain-skip line on transition only (see
// noteDrainSkip). GC_DEBUG restores the per-tick line.
func logDrainSkip(dt *drainTracker, w io.Writer, beadID, message string, now time.Time) {
	if dt.noteDrainSkip(beadID, message, now) || gcDebugEnabled() {
		fmt.Fprintln(w, message) //nolint:errcheck // best-effort diagnostics
	}
}
