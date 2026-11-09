package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/session"
)

// The fresh cycle's defer (SESS-612, plan C7a-2c; ga-pvjbx3 Q1-Q3), in the
// stop verb's first section. The probe reads, live and in this order, the
// previous bead, the session's in_progress claims, and the allocation's
// anchor; a read that already settles the cycle stops the reads. The decision
// is pure over what the probe saw and fails closed: a cycle ends a live
// runtime, so anything it could not prove safe defers it.

// causeFreshCycleDefer: the previous bead is still the session's, or this
// incarnation already woke after it closed, or the session holds a claim, or
// the anchor is gone, finished or unreadable.
const causeFreshCycleDefer = "fresh-cycle-defer"

// errFreshCycleUnread is why a cycle that could not be read defers: no world
// or environment to name the session's identities, or no usable work legs.
var errFreshCycleUnread = errors.New("fresh cycle: no world to read the session's work in")

// freshCyclePrev is the fresh cycle's live read of the session's work.
type freshCyclePrev struct {
	none  bool // the row records no previous bead
	found bool
	bead  beads.Bead
	// owned: the previous bead's live, trimmed assignee is one of the
	// session's RefuseScope identities.
	owned bool
	// claimed: the session holds an in_progress assigned bead on some leg.
	claimed bool
	// anchorLive: the allocation's anchor was read live and is not terminal.
	anchorLive bool
}

// freshCycleProbe reads the fresh cycle's work live over the pass's work legs
// (readLive: the store that owns each bead by residency); any other stop
// reads nothing. The claim scan is a live per-seat read of the session's
// RefuseScope on every leg, never the tick's index.
func freshCycleProbe(_ context.Context, r effectReads, v txView) (freshCyclePrev, error) {
	if v.It.Reason != decideFreshCycle {
		return freshCyclePrev{}, nil
	}
	if v.World == nil || v.World.Env == nil {
		return freshCyclePrev{}, errFreshCycleUnread
	}
	if err := r.legs.unusable(); err != nil {
		return freshCyclePrev{}, err
	}
	scope := refuseScope(v.Row, v.World.Env.Cfg)
	prev, err := freshCyclePrevRead(r, v, scope)
	if err != nil || freshCyclePrevDefers(v.Row, prev) {
		return prev, err
	}
	claimed, err := sessionHasAssignedWorkInStoresForStatuses(seatWorkFor(r.legs, scope), scope, []string{"in_progress"})
	if err != nil || claimed {
		prev.claimed = claimed
		return prev, err
	}
	anchor, found, err := r.legs.readLive(freshCycleAnchorID(v))
	prev.anchorLive = err == nil && found && !convoycore.IsTerminalStatus(anchor.Status)
	return prev, err
}

// freshCyclePrevRead reads the row's previous bead live and whether the
// session owns it.
func freshCyclePrevRead(r effectReads, v txView, scope RefuseScope) (freshCyclePrev, error) {
	id := strings.TrimSpace(v.Row.CurrentlyProcessingBeadID)
	if id == "" {
		return freshCyclePrev{none: true}, nil
	}
	b, found, err := r.legs.readLive(id)
	prev := freshCyclePrev{found: found, bead: b}
	if assignee := strings.TrimSpace(b.Assignee); found && assignee != "" {
		for _, own := range scope.scopeIDs() {
			prev.owned = prev.owned || strings.TrimSpace(own) == assignee
		}
	}
	return prev, err
}

// freshCycleAnchorID is the allocation's anchor bead for the cycle, trimmed;
// empty when the allocation names none.
func freshCycleAnchorID(v txView) string {
	if v.Alloc == nil || v.Alloc.Snapshot == nil {
		return ""
	}
	e := v.Alloc.Snapshot.Entries[v.It.Key]
	if e == nil || e.AssignedWork == nil {
		return ""
	}
	return strings.TrimSpace(e.AssignedWork.BeadID)
}

// stopDecideAfterProbe is stopDecide, after the fresh cycle's defer.
func stopDecideAfterProbe(v txView, prev freshCyclePrev, err error) txStep {
	if v.It.Reason == decideFreshCycle && freshCycleDefers(v.Row, prev, err) {
		return txStep{Refuse: causeFreshCycleDefer}
	}
	return stopDecide(v)
}

// freshCycleDefers is the defer, on the probe's answer. A read that failed,
// expired included, defers: it proves nothing safe (the #7879 review). Then
// the previous bead, the session's claims, and the anchor, each of which
// defers unless the probe proved it safe; a zero answer defers.
func freshCycleDefers(row session.Info, prev freshCyclePrev, err error) bool {
	return err != nil || freshCyclePrevDefers(row, prev) || prev.claimed || !prev.anchorLive
}

// freshCyclePrevDefers is the previous bead's part of the defer: an open one
// defers only while the session owns it (another identity's, or nobody's, is
// not this session's work), a closed one iff this incarnation woke after the
// close, whatever its assignee. No previous bead, or one no store holds, does
// not defer.
func freshCyclePrevDefers(row session.Info, prev freshCyclePrev) bool {
	switch {
	case prev.none, !prev.found:
		return false
	case !convoycore.IsTerminalStatus(prev.bead.Status):
		return prev.owned
	}
	closedAt := beadClosedAt(prev.bead)
	awake, perr := time.Parse(time.RFC3339Nano, row.AwakeStartedAt)
	return perr == nil && !closedAt.IsZero() && awake.After(closedAt)
}
