package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/session"
)

// The fresh cycle's defer (SESS-612, plan C7a-2c), in the stop verb's
// first section: the previous bead's live read, then legacy's defer on it.

// causeFreshCycleDefer: the previous bead is still open, or this
// incarnation already woke after it closed.
const causeFreshCycleDefer = "fresh-cycle-defer"

// freshCyclePrev is the fresh cycle's live read of the row's previous bead.
type freshCyclePrev struct {
	none  bool // the row records no previous bead
	found bool
	bead  beads.Bead
}

// freshCycleProbe reads the fresh cycle's previous bead live over the
// pass's work legs (readLive: the store that owns it by residency); any
// other stop reads nothing.
func freshCycleProbe(_ context.Context, r effectReads, v txView) (freshCyclePrev, error) {
	if v.It.Reason != decideFreshCycle {
		return freshCyclePrev{}, nil
	}
	prev := strings.TrimSpace(v.Row.CurrentlyProcessingBeadID)
	if prev == "" {
		return freshCyclePrev{none: true}, nil
	}
	b, found, err := r.legs.readLive(prev)
	return freshCyclePrev{found: found, bead: b}, err
}

// stopDecideAfterProbe is stopDecide, after the fresh cycle's defer.
func stopDecideAfterProbe(v txView, prev freshCyclePrev, err error) txStep {
	if v.It.Reason == decideFreshCycle && freshCycleDefers(v.Row, prev, err) {
		return txStep{Refuse: causeFreshCycleDefer}
	}
	return stopDecide(v)
}

// freshCycleDefers is legacy's defer, on the probe's answer: the previous
// bead still open, or this incarnation awake since after it closed. A read
// that failed, expired included, defers: it proves the bead neither open
// nor closed (the #7879 review; legacy cycles). No previous bead, or one no
// store holds, cycles.
func freshCycleDefers(row session.Info, prev freshCyclePrev, err error) bool {
	switch {
	case err != nil:
		return true
	case prev.none, !prev.found:
		return false
	case !convoycore.IsTerminalStatus(prev.bead.Status):
		return true
	}
	closedAt := beadClosedAt(prev.bead)
	awake, perr := time.Parse(time.RFC3339Nano, row.AwakeStartedAt)
	return perr == nil && !closedAt.IsZero() && awake.After(closedAt)
}
