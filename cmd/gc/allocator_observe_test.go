package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

const observeMaxAge = time.Minute

// observeCache is an observation cache with one backend pass published per
// call to publish.
type observeCache struct {
	*ObservationCache
	seq uint64
}

func newObserveCache() *observeCache {
	return &observeCache{ObservationCache: NewObservationCache(&clock.Fake{Time: censusNow}, observeMaxAge, "e1")}
}

func (c *observeCache) publish(at time.Time, attrs map[string]InventoryAttrs, backends ...BackendPass) *ObservationSnapshot {
	return c.publishMerged(at, nil, attrs, backends...)
}

// publishMerged publishes a pass whose merged listing returned mergedErr.
func (c *observeCache) publishMerged(at time.Time, mergedErr error, attrs map[string]InventoryAttrs, backends ...BackendPass) *ObservationSnapshot {
	c.seq++
	var merged []string
	for _, b := range backends {
		merged = append(merged, b.Names...)
	}
	c.PublishInventory(InventoryPass{Epoch: "e1", Seq: c.seq, ProviderGen: 1, StartedAt: at, FinishedAt: at, MergedNames: merged, MergedErr: mergedErr, Backends: backends}, attrs)
	return c.Snapshot()
}

// observeRows reads a census of session rows named by their session_name.
func observeRows(t *testing.T, named map[string]string) *sessionCensus {
	t.Helper()
	var rows []beads.Bead
	for id, name := range named {
		rows = append(rows, censusSession(id, map[string]string{"session_name": name, "state": "active"}))
	}
	return readCensus(t, censusNow, censusLegs("class:sessions", censusStore(rows...)))
}

func observed(t *testing.T, snap *ObservationSnapshot, c *sessionCensus, now time.Time, id string) rowObservation {
	t.Helper()
	got, ok := observeCensus(snap, c, now, observeMaxAge)[rowKey{"class:sessions", id}]
	if !ok {
		t.Fatalf("no observation for %s", id)
	}
	return got
}

// Kills: ACP and subprocess sessions kept forever (F1). A backend with no
// batched inventory records attach as unsupported, which reads as not
// attached and leaves the row certain.
func TestObserveUnsupportedAttachIsNotAttached(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {}}, completeBackend("acp", "s1"))
	got := observed(t, snap, c, censusNow, "gc-1")
	if got.Liveness != livenessAlive || got.Attached || got.Uncertain {
		t.Fatalf("observation = %+v, want alive, not attached, certain", got)
	}
}

// Kills: a stale tmux attach read as detached, which would let the allocator
// propose sleeping an attached session. On a backend that reports attach, an
// attach fact older than maxAge leaves the live row uncertain; a fresh No is
// detached and certain.
func TestObserveStaleAttachOnReporterIsUncertain(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	snap := cache.publish(censusNow, map[string]InventoryAttrs{"s1": {AttachedKnown: true, Attached: false}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Attached || got.Uncertain {
		t.Fatalf("fresh detach: %+v, want alive, detached, certain", got)
	}

	// The next pass lists s1 but its inventory omits it: attach keeps the
	// first pass's stamp and ages past maxAge while the listing stays fresh.
	later := censusNow.Add(50 * time.Second)
	snap = cache.publish(later, nil, completeBackend("tmux", "s1"))
	got := observed(t, snap, c, censusNow.Add(observeMaxAge+time.Second), "gc-1")
	if got.Liveness != livenessAlive || !got.Uncertain || got.Reason != observeReasonAttach+obsReasonStale {
		t.Fatalf("stale attach: %+v, want alive and uncertain (attach-stale)", got)
	}
}

// Kills: every live session Keep (F1), and pending read for a row whose
// runtime is not alive (legacy probes pending only on live targets). The lane
// never publishes pending, so unknown pending is not pending and not
// uncertain; a fresh Yes a session key wrote through Note is pending for the
// row that owns the live runtime only: not for a sibling that shares its
// name, and not once the runtime is gone.
func TestObservePendingUnknownIsNotAnInput(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	snap := cache.publish(censusNow, map[string]InventoryAttrs{"s1": {AttachedKnown: true}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Pending || got.Uncertain {
		t.Fatalf("unknown pending: %+v, want not pending and certain", got)
	}
	cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
	if got := observed(t, cache.Snapshot(), c, censusNow, "gc-1"); !got.Pending {
		t.Fatalf("noted pending: %+v, want pending", got)
	}

	shared := observeRows(t, map[string]string{"gc-1": "w-2-pool", "gc-2": "w-2-pool"})
	cache = newObserveCache()
	cache.publish(censusNow, map[string]InventoryAttrs{"w-2-pool": {AttachedKnown: true, OwnerState: OwnerSession, OwnerID: "gc-2"}}, completeBackend("tmux", "w-2-pool"))
	cache.Note("w-2-pool", FactPending, ObsYes, censusNow, SourceProbe, "")
	if owner, sibling := observed(t, cache.Snapshot(), shared, censusNow, "gc-2"), observed(t, cache.Snapshot(), shared, censusNow, "gc-1"); !owner.Pending || sibling.Pending {
		t.Fatalf("shared name: owner pending=%v sibling pending=%v, want only the owner", owner.Pending, sibling.Pending)
	}

	cache = newObserveCache()
	cache.publish(censusNow.Add(-time.Second), map[string]InventoryAttrs{"s1": {AttachedKnown: true}}, completeBackend("tmux", "s1"))
	cache.publish(censusNow, nil, completeBackend("tmux"))
	cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
	if got := observed(t, cache.Snapshot(), c, censusNow, "gc-1"); got.Liveness != livenessAbsent || got.Pending {
		t.Fatalf("runtime gone: %+v, want absent and not pending", got)
	}
}

// Kills: a dead pane read as unknown (tmux remain-on-exit keeps exited panes
// listed, so an exited session would be Kept with no grant and never
// restart), a dead pane read as alive (BEHAVIORS #7: a zombie is not alive),
// and attach or pending read for a dead row (both facts are Yes in the
// fixture). A listed, fresh runtime whose pane or process is dead is dead for
// the row it is attributed to, or for the only row with its name; another
// row's dead pane is occupied, and a shared name with no known owner stays
// unknown.
func TestObserveDeadPaneIsAStartCandidateNotUncertain(t *testing.T) {
	deadPane := InventoryAttrs{DeadKnown: true, AllPanesDead: true, AttachedKnown: true, Attached: true}
	owned := func(owner string) InventoryAttrs {
		a := deadPane
		a.OwnerState, a.OwnerID = OwnerSession, owner
		return a
	}
	cases := []struct {
		name  string
		rows  map[string]string
		attrs InventoryAttrs
		want  map[string]rowLiveness
	}{
		{"unique-unattributed", map[string]string{"gc-1": "s1"}, deadPane, map[string]rowLiveness{"gc-1": livenessDead}},
		{"unique-ownerless", map[string]string{"gc-1": "s1"}, InventoryAttrs{DeadKnown: true, AllPanesDead: true, OwnerState: OwnerNone}, map[string]rowLiveness{"gc-1": livenessDead}},
		{"unique-owned-by-row", map[string]string{"gc-1": "s1"}, owned("gc-1"), map[string]rowLiveness{"gc-1": livenessDead}},
		{"unique-owned-by-another-bead", map[string]string{"gc-1": "s1"}, owned("gc-9"), map[string]rowLiveness{"gc-1": livenessOccupied}},
		{"shared-owned", map[string]string{"gc-1": "s1", "gc-2": "s1"}, owned("gc-2"), map[string]rowLiveness{"gc-1": livenessOccupied, "gc-2": livenessDead}},
		{"shared-unattributed", map[string]string{"gc-1": "s1", "gc-2": "s1"}, deadPane, map[string]rowLiveness{"gc-1": livenessUnknown, "gc-2": livenessUnknown}},
	}
	for _, tc := range cases {
		c := observeRows(t, tc.rows)
		cache := newObserveCache()
		cache.publish(censusNow, map[string]InventoryAttrs{"s1": tc.attrs}, completeBackend("tmux", "s1"))
		cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
		snap := cache.Snapshot()
		for id, want := range tc.want {
			got := observed(t, snap, c, censusNow, id)
			if got.Liveness != want || got.Uncertain != (want == livenessUnknown) || got.Attached || got.Pending {
				t.Errorf("%s: %s = %+v, want %v (uncertain iff unknown)", tc.name, id, got, want)
			}
		}
	}

	// A live pane whose agent process a session key found dead is dead too.
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	cache.publish(censusNow, map[string]InventoryAttrs{"s1": {DeadKnown: true, AttachedKnown: true, Attached: true}}, completeBackend("tmux", "s1"))
	cache.Note("s1", FactProcessAlive, ObsNo, censusNow, SourceProbe, "")
	cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
	if got := observed(t, cache.Snapshot(), c, censusNow, "gc-1"); got.Liveness != livenessDead || got.Uncertain || got.Attached || got.Pending {
		t.Fatalf("process dead in a live pane: %+v, want dead, certain, neither attached nor pending", got)
	}
}

// Kills: a dead row that is not a start candidate, or that counts as alive
// for dependencies; and an unknown row read as a start candidate.
func TestObserveLivenessPredicates(t *testing.T) {
	cases := []struct {
		l            rowLiveness
		alive, start bool
	}{
		{livenessUnknown, false, false},
		{livenessAlive, true, false},
		{livenessOccupied, false, false},
		{livenessAbsent, false, true},
		{livenessAbsentUnconfirmed, false, true},
		{livenessDead, false, true},
	}
	for _, tc := range cases {
		if tc.l.alive() != tc.alive || tc.l.startCandidate() != tc.start {
			t.Errorf("%v: alive=%v start=%v, want %v %v", tc.l, tc.l.alive(), tc.l.startCandidate(), tc.alive, tc.start)
		}
	}
}

// Kills: a runtime listed on a backend that cannot attest absence read as
// absent-unconfirmed, which would start a second copy next to it (exec, ssh,
// herdr and t3bridge never prime). Listed by the latest fresh pass on a
// backend that did not fail is present: alive for its row, occupied for
// another bead's, and unknown again once that pass is stale.
func TestObserveListedOnUnprimedBackendIsPresent(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {}}, completeBackend("tmux"), unattestedBackend("exec", "s1"))
	if snap.Primed["exec"] {
		t.Fatal("fixture: the unattested backend primed")
	}
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Uncertain {
		t.Fatalf("listed on exec: %+v, want alive and certain", got)
	}
	if got := observed(t, snap, c, censusNow.Add(observeMaxAge+time.Second), "gc-1"); got.Liveness != livenessUnknown {
		t.Fatalf("listed on exec by a stale pass: %+v, want unknown", got)
	}
	// A partial listing has not primed either, but what it listed is there.
	partial := BackendPass{Label: "exec", Outcome: OutcomePartial, Names: []string{"s1"}, Err: errors.New("one host unanswered")}
	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {}}, completeBackend("tmux"), partial)
	if snap.Primed["exec"] {
		t.Fatal("fixture: the partial backend primed")
	}
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Uncertain {
		t.Fatalf("listed on a partial exec pass: %+v, want alive and certain", got)
	}

	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {OwnerState: OwnerSession, OwnerID: "gc-9"}}, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessOccupied {
		t.Fatalf("exec runtime owned by another bead: %+v, want occupied", got)
	}
	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {DeadKnown: true, AllPanesDead: true}}, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessDead {
		t.Fatalf("dead exec runtime: %+v, want dead", got)
	}

	// The latest pass listed s1 without enriching it: an older pass's dead
	// pane is stale, and an older pass's owner is not this incarnation's.
	cache := newObserveCache()
	cache.publish(censusNow.Add(-90*time.Second), map[string]InventoryAttrs{"s1": {DeadKnown: true, AllPanesDead: true}}, unattestedBackend("exec", "s1"))
	snap = cache.publish(censusNow, nil, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive {
		t.Fatalf("stale dead pane on exec: %+v, want alive", got)
	}
	cache = newObserveCache()
	cache.publish(censusNow.Add(-10*time.Second), map[string]InventoryAttrs{"s1": {OwnerState: OwnerSession, OwnerID: "gc-9"}}, unattestedBackend("exec", "s1"))
	snap = cache.publish(censusNow, nil, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive {
		t.Fatalf("owner from an earlier pass on exec: %+v, want alive (attribution not current)", got)
	}
}

// Kills: every row that shares a name read as alive (R-43). The runtime is
// alive for its attributed owner and occupied for the other rows; with no
// attribution, or a runtime that carries no owner at all, no row can claim it,
// so all are uncertain rather than alive.
func TestObserveSharedNameAttributesRuntimeToOwnerOnly(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "w-2-pool", "gc-2": "w-2-pool", "gc-3": "w-2-pool"})
	snap := newObserveCache().publish(censusNow,
		map[string]InventoryAttrs{"w-2-pool": {OwnerState: OwnerSession, OwnerID: "gc-2"}}, completeBackend("tmux", "w-2-pool"))
	want := map[string]rowLiveness{"gc-1": livenessOccupied, "gc-2": livenessAlive, "gc-3": livenessOccupied}
	for id, liveness := range want {
		if got := observed(t, snap, c, censusNow, id); got.Liveness != liveness {
			t.Errorf("owned: %s = %v, want %v", id, got.Liveness, liveness)
		}
	}

	for name, attrs := range map[string]InventoryAttrs{"unattributed": {}, "ownerless": {OwnerState: OwnerNone}} {
		snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"w-2-pool": attrs}, completeBackend("tmux", "w-2-pool"))
		for id := range want {
			if got := observed(t, snap, c, censusNow, id); got.Liveness != livenessUnknown || !got.Uncertain {
				t.Errorf("%s: %s = %+v, want unknown and uncertain", name, id, got)
			}
		}
	}
}

// Kills: a bead's duplicate copy on a later leg counted as a second row with
// its name (C2.11). The copy is not a sharer, so an unattributed runtime is
// alive for the bead's canonical row rather than unknown.
func TestObserveDuplicateCopyIsNotASharer(t *testing.T) {
	row := func() beads.Bead {
		return censusSession("gc-1", map[string]string{"session_name": "s1", "state": "active"})
	}
	c := readCensus(t, censusNow,
		censusLegs("class:sessions", censusStore(row()), "city:mc", censusStore(row())))
	if len(c.Rows) != 2 || len(c.RowsNamed("s1")) != 1 {
		t.Fatalf("fixture: %d rows, %d named s1, want 2 rows of which one canonical", len(c.Rows), len(c.RowsNamed("s1")))
	}
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Uncertain {
		t.Fatalf("unattributed runtime: %+v, want alive and certain", got)
	}
}

// Kills: starts while a backend failed or before the first pass, and never
// starting on a backend whose listing cannot attest absence. A name the
// latest pass did not list is absent-unconfirmed only when that pass is fresh
// and every backend listed completely or unattested; a complete pass that
// stops listing a name proves it absent.
func TestObserveNotListedNoFailedBackendIsAbsentUnconfirmed_FailedOrStaleIsUnknown(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	unattested := unattestedBackend("acp", "other")
	failed := BackendPass{Label: "acp", Outcome: OutcomeFailed, Attested: true, Err: errors.New("acp down")}
	partial := BackendPass{Label: "acp", Outcome: OutcomePartial, Attested: true, Names: []string{"other"}}

	cases := []struct {
		name     string
		snap     func() *ObservationSnapshot
		at       time.Time
		liveness rowLiveness
	}{
		{"complete-never-listed", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux", "other"))
		}, censusNow, livenessAbsentUnconfirmed},
		{"complete-stopped-listing", func() *ObservationSnapshot {
			cache := newObserveCache()
			cache.publish(censusNow.Add(-time.Second), nil, completeBackend("tmux", "s1"))
			return cache.publish(censusNow, nil, completeBackend("tmux"))
		}, censusNow, livenessAbsent},
		{"unattested-not-listed", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux"), unattested)
		}, censusNow, livenessAbsentUnconfirmed},
		{"failed-backend", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux"), failed)
		}, censusNow, livenessUnknown},
		{"partial-backend", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux"), partial)
		}, censusNow, livenessUnknown},
		{"stale-pass", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux"), unattested)
		}, censusNow.Add(observeMaxAge + time.Second), livenessUnknown},
		{"no-pass-yet", func() *ObservationSnapshot { return newObserveCache().Snapshot() }, censusNow, livenessUnknown},
		{"merged-listing-failed", func() *ObservationSnapshot {
			return newObserveCache().publishMerged(censusNow, errors.New("list failed"), nil, completeBackend("tmux", "other"))
		}, censusNow, livenessUnknown},
	}
	for _, tc := range cases {
		got := observed(t, tc.snap(), c, tc.at, "gc-1")
		if got.Liveness != tc.liveness || got.Uncertain != (tc.liveness == livenessUnknown) {
			t.Errorf("%s: %+v, want %v (uncertain iff unknown)", tc.name, got, tc.liveness)
		}
	}
}
