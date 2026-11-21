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

// The fresh cycle's defer (SESS-612, plan C7a-2c; #9320), in the stop
// verb's first section: the previous bead's live read and legacy's defer on
// it, then one live read of the allocation's anchor, which must still be the
// session's own open work.

// causeFreshCycleDefer: the previous bead is still open, or this
// incarnation already woke after it closed, or the anchor's live row does
// not prove it the session's open work (released, handed to another
// identity, finished, or unreadable).
const causeFreshCycleDefer = "fresh-cycle-defer"

// errFreshCycleNoWorld is why a cycle with no world or environment to name
// the session's identities from cannot prove its anchor's owner.
var errFreshCycleNoWorld = errors.New("fresh cycle: no world to name the session's identities")

// freshCyclePrev is the fresh cycle's live read of the row's previous bead,
// and of its anchor.
type freshCyclePrev struct {
	none  bool // the row records no previous bead
	found bool
	bead  beads.Bead
	// anchorOwned: the allocation's anchor was read live, and that one row
	// is non-terminal and assigned to one of the session's ReleaseScope
	// identities (freshAnchorOwned).
	anchorOwned bool
}

// freshCycleProbe reads the fresh cycle's previous bead live over the
// pass's work legs (readLive: the store that owns it by residency); any
// other stop reads nothing. When the previous bead alone does not defer the
// cycle, it then reads the allocation's anchor live, once, past any cache,
// and judges that one row's status and assignee together: the allocation's
// snapshot can still assign the anchor to the session after the session
// released it or handed it off (#9320).
func freshCycleProbe(_ context.Context, r effectReads, v txView) (freshCyclePrev, error) {
	if v.It.Reason != decideFreshCycle {
		return freshCyclePrev{}, nil
	}
	prev, err := freshCyclePrevRead(r, v)
	if err != nil || freshCyclePrevDefers(v.Row, prev) {
		return prev, err
	}
	id := freshCycleAnchorID(v)
	if id == "" {
		return prev, nil
	}
	if v.World == nil || v.World.Env == nil {
		return prev, errFreshCycleNoWorld
	}
	anchor, found, err := r.legs.readLive(id)
	prev.anchorOwned = err == nil && found && freshAnchorOwned(anchor, releaseScope(v.Row, v.World.Env.Cfg).scopeIDs())
	return prev, err
}

// freshCyclePrevRead reads the row's previous bead live.
func freshCyclePrevRead(r effectReads, v txView) (freshCyclePrev, error) {
	id := strings.TrimSpace(v.Row.CurrentlyProcessingBeadID)
	if id == "" {
		return freshCyclePrev{none: true}, nil
	}
	b, found, err := r.legs.readLive(id)
	return freshCyclePrev{found: found, bead: b}, err
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
// expired included, defers: it proves nothing (the #7879 review; legacy
// cycles). Then legacy's defer on the previous bead, and last the anchor,
// which defers unless its live row proved it the session's open work.
func freshCycleDefers(row session.Info, prev freshCyclePrev, err error) bool {
	return err != nil || freshCyclePrevDefers(row, prev) || !prev.anchorOwned
}

// freshCyclePrevDefers is the previous bead's part of the defer: the
// previous bead still open, or this incarnation awake since after it
// closed. No previous bead, or one no store holds, does not defer.
func freshCyclePrevDefers(row session.Info, prev freshCyclePrev) bool {
	switch {
	case prev.none, !prev.found:
		return false
	case !convoycore.IsTerminalStatus(prev.bead.Status):
		return true
	}
	closedAt := beadClosedAt(prev.bead)
	awake, perr := time.Parse(time.RFC3339Nano, row.AwakeStartedAt)
	return perr == nil && !closedAt.IsZero() && awake.After(closedAt)
}
