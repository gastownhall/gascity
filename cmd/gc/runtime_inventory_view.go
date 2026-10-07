package main

import "time"

// runtimeInventoryView is one tick's read of the inventory lane for the
// runtime reapers (cleanupDeadRuntimeSessionCorpses and
// reapRuntimesBoundToClosedBeads). It may only nominate candidates (listing)
// and filter them (livePane, owner): every Stop and close still follows the
// reaper's own fresh confirmation of that name. A filter only skips a name
// for this tick, so a wrong filter delays a reap by a pass; it never causes
// one.
//
// A nil view means the lane is missing or its last pass is stale or failed,
// and the reapers list live, exactly as without a lane.
type runtimeInventoryView struct {
	snap   *ObservationSnapshot
	now    time.Time
	maxAge time.Duration

	// Per-reaper counts for the tick's phase records; the reapers write
	// them.
	corpses     runtimeReapStats
	closedBound runtimeReapStats
}

// Inventory sources a reaper's phase record names.
const (
	inventorySourceLane = "lane"
	inventorySourceLive = "live"
)

// runtimeReapStats counts one reaper's use of the view: the listed names it
// considered, the fresh confirmations it issued, and the names a lane filter
// skipped. The reaper sets source to lane when it takes the view's listing;
// otherwise its phase record names a live listing.
type runtimeReapStats struct {
	source     string
	candidates int
	confirms   int
	filtered   int
}

// inventoryViewForTick returns the view the reapers read this tick: the
// lane's current snapshot when its pass finished within maxAge (two patrol
// intervals) and its merged listing did not fail, nil otherwise.
func (cr *CityRuntime) inventoryViewForTick() *runtimeInventoryView {
	lane := cr.inventoryLane
	if lane == nil {
		return nil
	}
	snap, ok := lane.cache.FreshSnapshot(lane.cache.maxAge)
	if !ok {
		return nil
	}
	return &runtimeInventoryView{snap: snap, now: lane.clock.Now(), maxAge: lane.cache.maxAge}
}

// listing returns the pass's merged (names, err): exactly what
// sp.ListRunning("") returned for the same backend answers. The reapers
// apply their partial-list rule to it as to a live listing.
func (v *runtimeInventoryView) listing() ([]string, error) {
	return v.snap.Inventory.MergedNames, v.snap.Inventory.MergedErr
}

// listedNow returns name's observation when the pass this view serves listed
// it, on a primed backend, within maxAge. Its owner fields are then those of
// the incarnation that pass listed, or cleared (ObservationSnapshot.Observation).
func (v *runtimeInventoryView) listedNow(name string) (RuntimeObservation, bool) {
	obs, ok := v.snap.Observation(name, v.now, v.maxAge)
	if !ok || obs.Listed.Value != ObsYes || !obs.Listed.ObservedAt.Equal(v.snap.Inventory.StartedAt) {
		return RuntimeObservation{}, false
	}
	return obs, true
}

// livePane reports whether the pass saw name's listed incarnation with a
// live pane. Unknown, a corpse, or a fact from an earlier pass is false.
func (v *runtimeInventoryView) livePane(name string) bool {
	obs, ok := v.listedNow(name)
	return ok && obs.Running.Value == ObsYes && obs.Running.ObservedAt.Equal(v.snap.Inventory.StartedAt)
}

// owner returns the GC_SESSION_ID the lane read for name's listed
// incarnation. ok is false when the name is not listed by this pass, its
// facts are stale or unprimed, or that incarnation's attribution is unread,
// failed or ownerless.
func (v *runtimeInventoryView) owner(name string) (string, bool) {
	obs, ok := v.listedNow(name)
	if !ok || obs.OwnerState != OwnerSession || obs.Owner.SessionID == "" {
		return "", false
	}
	return obs.Owner.SessionID, true
}

// corpsePhaseFields and closedBoundPhaseFields are the reapers' phase-record
// fields. A nil view records a live listing.
func (v *runtimeInventoryView) corpsePhaseFields() map[string]any {
	if v == nil {
		return liveReapPhaseFields()
	}
	return v.reapPhaseFields(v.corpses)
}

func (v *runtimeInventoryView) closedBoundPhaseFields() map[string]any {
	if v == nil {
		return liveReapPhaseFields()
	}
	return v.reapPhaseFields(v.closedBound)
}

// reapPhaseFields names the observation a reaper used (spec §4.5) and what
// the lane saved it.
func (v *runtimeInventoryView) reapPhaseFields(s runtimeReapStats) map[string]any {
	if s.source != inventorySourceLane {
		return liveReapPhaseFields()
	}
	return map[string]any{
		"inventory_source":           inventorySourceLane,
		"inventory_epoch":            v.snap.Epoch,
		"inventory_pass_seq":         v.snap.PassSeq,
		"inventory_gen":              v.snap.Gen,
		"inventory_age_ms":           v.now.Sub(v.snap.At).Milliseconds(),
		"inventory_backend_outcomes": inventoryBackendOutcomes(v.snap.Inventory.Backends),
		"candidates":                 s.candidates,
		"confirms":                   s.confirms,
		"filtered":                   s.filtered,
	}
}

func liveReapPhaseFields() map[string]any {
	return map[string]any{"inventory_source": inventorySourceLive}
}
