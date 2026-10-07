package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
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

// readIdentity is a clean identity read: a token, and sessionID when set.
func readIdentity(sessionID string) runtimeIdentity {
	return runtimeIdentity{Known: true, SessionID: sessionID, Token: "tok"}
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
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {Identity: readIdentity("")}}, completeBackend("acp", "s1"))
	got := observed(t, snap, c, censusNow, "gc-1")
	if got.Liveness != livenessAlive || got.Attached || got.Uncertain {
		t.Fatalf("observation = %+v, want alive, not attached, certain", got)
	}
}

// Kills: a stale tmux attach read as detached, which would let the allocator
// propose sleeping an attached session. On a backend that reports attach, an
// attach fact older than maxAge comes from a pass that last enriched the
// name, so its identity is not current either: the row is unknown, which is
// uncertain. A fresh No is detached and certain.
func TestObserveStaleAttachOnReporterIsUncertain(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	snap := cache.publish(censusNow, map[string]InventoryAttrs{"s1": {AttachedKnown: true, Attached: false, Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Attached || got.Uncertain {
		t.Fatalf("fresh detach: %+v, want alive, detached, certain", got)
	}

	// The next pass lists s1 but its inventory omits it: attach keeps the
	// first pass's stamp and ages past maxAge while the listing stays fresh.
	later := censusNow.Add(50 * time.Second)
	snap = cache.publish(later, nil, completeBackend("tmux", "s1"))
	got := observed(t, snap, c, censusNow.Add(observeMaxAge+time.Second), "gc-1")
	if got.Liveness != livenessUnknown || !got.Uncertain || got.Reason != observeReasonIdentity {
		t.Fatalf("stale attach: %+v, want unknown and uncertain (identity not current)", got)
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
	snap := cache.publish(censusNow, map[string]InventoryAttrs{"s1": {AttachedKnown: true, Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Pending || got.Uncertain {
		t.Fatalf("unknown pending: %+v, want not pending and certain", got)
	}
	cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
	if got := observed(t, cache.Snapshot(), c, censusNow, "gc-1"); !got.Pending {
		t.Fatalf("noted pending: %+v, want pending", got)
	}

	shared := observeRows(t, map[string]string{"gc-1": "w-2-pool", "gc-2": "w-2-pool"})
	cache = newObserveCache()
	cache.publish(censusNow, map[string]InventoryAttrs{"w-2-pool": {AttachedKnown: true, Identity: readIdentity("gc-2")}}, completeBackend("tmux", "w-2-pool"))
	cache.Note("w-2-pool", FactPending, ObsYes, censusNow, SourceProbe, "")
	if owner, sibling := observed(t, cache.Snapshot(), shared, censusNow, "gc-2"), observed(t, cache.Snapshot(), shared, censusNow, "gc-1"); !owner.Pending || sibling.Pending {
		t.Fatalf("shared name: owner pending=%v sibling pending=%v, want only the owner", owner.Pending, sibling.Pending)
	}

	cache = newObserveCache()
	cache.publish(censusNow.Add(-time.Second), map[string]InventoryAttrs{"s1": {AttachedKnown: true, Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
	cache.publish(censusNow, nil, completeBackend("tmux"))
	cache.Note("s1", FactPending, ObsYes, censusNow, SourceProbe, "")
	if got := observed(t, cache.Snapshot(), c, censusNow, "gc-1"); got.Liveness != livenessGone || got.Pending {
		t.Fatalf("runtime gone: %+v, want gone and not pending", got)
	}
}

// Kills: a dead pane read as unknown (tmux remain-on-exit keeps exited panes
// listed, so an exited session would be Kept with no grant and never
// restart), a dead pane read as alive (BEHAVIORS #7: a zombie is not alive),
// and attach or pending read for a dead row (both facts are Yes in the
// fixture). A listed, fresh runtime whose pane or process is dead is dead for
// the row it is attributed to, or for the only row with its name; another
// row's dead pane is occupied, and a shared name with no known owner stays
// unknown, as does a corpse whose identity is unread or ownerless (v5 O1).
func TestObserveDeadPaneIsAStartCandidateNotUncertain(t *testing.T) {
	deadPane := InventoryAttrs{DeadKnown: true, AllPanesDead: true, AttachedKnown: true, Attached: true, Identity: readIdentity("")}
	owned := func(owner string) InventoryAttrs {
		a := deadPane
		a.Identity = readIdentity(owner)
		return a
	}
	unread := deadPane
	unread.Identity = runtimeIdentity{}
	cases := []struct {
		name  string
		rows  map[string]string
		attrs InventoryAttrs
		want  map[string]rowLiveness
	}{
		{"unique-token-only", map[string]string{"gc-1": "s1"}, deadPane, map[string]rowLiveness{"gc-1": livenessDead}},
		{"unique-unread", map[string]string{"gc-1": "s1"}, unread, map[string]rowLiveness{"gc-1": livenessUnknown}},
		{"unique-ownerless", map[string]string{"gc-1": "s1"}, InventoryAttrs{DeadKnown: true, AllPanesDead: true, Identity: runtimeIdentity{Known: true}}, map[string]rowLiveness{"gc-1": livenessUnknown}},
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
	cache.publish(censusNow, map[string]InventoryAttrs{"s1": {DeadKnown: true, AttachedKnown: true, Attached: true, Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
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
		{livenessGone, false, true},
		{livenessDead, false, true},
	}
	for _, tc := range cases {
		if tc.l.alive() != tc.alive || tc.l.startCandidate() != tc.start {
			t.Errorf("%v: alive=%v start=%v, want %v %v", tc.l, tc.l.alive(), tc.l.startCandidate(), tc.alive, tc.start)
		}
	}
}

// Kills: a runtime listed on a backend that cannot attest absence read as
// gone, which would start a second copy next to it (exec, ssh,
// herdr and t3bridge never prime). Listed by the latest fresh pass on a
// backend that did not fail is present: alive for its row, occupied for
// another bead's, and unknown again once that pass is stale.
func TestObserveListedOnUnprimedBackendIsPresent(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {Identity: readIdentity("")}}, completeBackend("tmux"), unattestedBackend("exec", "s1"))
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
	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {Identity: readIdentity("")}}, completeBackend("tmux"), partial)
	if snap.Primed["exec"] {
		t.Fatal("fixture: the partial backend primed")
	}
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Uncertain {
		t.Fatalf("listed on a partial exec pass: %+v, want alive and certain", got)
	}

	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {Identity: readIdentity("gc-9")}}, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessOccupied {
		t.Fatalf("exec runtime owned by another bead: %+v, want occupied", got)
	}
	snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {DeadKnown: true, AllPanesDead: true, Identity: readIdentity("")}}, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessDead {
		t.Fatalf("dead exec runtime: %+v, want dead", got)
	}

	// The latest pass listed s1 without enriching it: an older pass's dead
	// pane is stale, and an older pass's identity is not this incarnation's,
	// so the row holds as unknown (v5 O1).
	cache := newObserveCache()
	cache.publish(censusNow.Add(-90*time.Second), map[string]InventoryAttrs{"s1": {DeadKnown: true, AllPanesDead: true, Identity: readIdentity("")}}, unattestedBackend("exec", "s1"))
	snap = cache.publish(censusNow, nil, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessUnknown {
		t.Fatalf("stale dead pane on exec: %+v, want unknown", got)
	}
	cache = newObserveCache()
	cache.publish(censusNow.Add(-10*time.Second), map[string]InventoryAttrs{"s1": {Identity: readIdentity("gc-9")}}, unattestedBackend("exec", "s1"))
	snap = cache.publish(censusNow, nil, unattestedBackend("exec", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessUnknown {
		t.Fatalf("owner from an earlier pass on exec: %+v, want unknown (identity not current)", got)
	}
}

// Kills: every row that shares a name read as alive (R-43). The runtime is
// alive for its attributed owner and occupied for the other rows; with no
// session ID, or a runtime whose identity is unread or ownerless, no row can
// claim it, so all are uncertain rather than alive.
func TestObserveSharedNameAttributesRuntimeToOwnerOnly(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "w-2-pool", "gc-2": "w-2-pool", "gc-3": "w-2-pool"})
	snap := newObserveCache().publish(censusNow,
		map[string]InventoryAttrs{"w-2-pool": {Identity: readIdentity("gc-2")}}, completeBackend("tmux", "w-2-pool"))
	want := map[string]rowLiveness{"gc-1": livenessOccupied, "gc-2": livenessAlive, "gc-3": livenessOccupied}
	for id, liveness := range want {
		if got := observed(t, snap, c, censusNow, id); got.Liveness != liveness {
			t.Errorf("owned: %s = %v, want %v", id, got.Liveness, liveness)
		}
	}

	for name, attrs := range map[string]InventoryAttrs{"unread": {}, "ownerless": {Identity: runtimeIdentity{Known: true}}, "token-only": {Identity: readIdentity("")}} {
		snap = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"w-2-pool": attrs}, completeBackend("tmux", "w-2-pool"))
		for id := range want {
			if got := observed(t, snap, c, censusNow, id); got.Liveness != livenessUnknown || !got.Uncertain {
				t.Errorf("%s: %s = %+v, want unknown and uncertain", name, id, got)
			}
		}
	}
}

// Kills: a bead's duplicate copy on a later leg counted as a second row with
// its name (C2.11). The copy is not a sharer, so a runtime with no session ID
// is alive for the bead's canonical row rather than unknown.
func TestObserveDuplicateCopyIsNotASharer(t *testing.T) {
	row := func() beads.Bead {
		return censusSession("gc-1", map[string]string{"session_name": "s1", "state": "active"})
	}
	c := readCensus(t, censusNow,
		censusLegs("class:sessions", censusStore(row()), "city:mc", censusStore(row())))
	if len(c.Rows) != 2 || len(c.RowsNamed("s1")) != 1 {
		t.Fatalf("fixture: %d rows, %d named s1, want 2 rows of which one canonical", len(c.Rows), len(c.RowsNamed("s1")))
	}
	snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
	if got := observed(t, snap, c, censusNow, "gc-1"); got.Liveness != livenessAlive || got.Uncertain {
		t.Fatalf("runtime without a session ID: %+v, want alive and certain", got)
	}
}

// Kills: starts while a backend failed or before the first pass, and an
// unattested backend proving a name gone (TestUnattestedBackendNeverProvesGone,
// I13). A name the latest pass did not list is gone only when that pass is
// fresh and every backend listed completely and attested, whether or not the
// name was ever listed.
func TestUnattestedBackendNeverProvesGone(t *testing.T) {
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
		}, censusNow, livenessGone},
		{"complete-stopped-listing", func() *ObservationSnapshot {
			cache := newObserveCache()
			cache.publish(censusNow.Add(-time.Second), nil, completeBackend("tmux", "s1"))
			return cache.publish(censusNow, nil, completeBackend("tmux"))
		}, censusNow, livenessGone},
		{"unattested-not-listed", func() *ObservationSnapshot {
			return newObserveCache().publish(censusNow, nil, completeBackend("tmux"), unattested)
		}, censusNow, livenessUnknown},
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

// Kills: history-dependent absence (I13). A name unlisted for 11 complete
// passes is pruned from the cache and still gone, and a name never listed is
// gone the same way.
func TestPrunedNameStillGone(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1", "gc-2": "never"})
	cache := newObserveCache()
	at := censusNow.Add(-12 * time.Second)
	cache.publish(at, map[string]InventoryAttrs{"s1": {DeadKnown: true, Identity: readIdentity("")}}, completeBackend("tmux", "s1"))
	var snap *ObservationSnapshot
	for i := 0; i < observationRetentionPasses+1; i++ {
		at = at.Add(time.Second)
		snap = cache.publish(at, nil, completeBackend("tmux"))
	}
	if _, kept := snap.ByName["s1"]; kept {
		t.Fatal("fixture: s1 was not pruned")
	}
	for _, id := range []string{"gc-1", "gc-2"} {
		if got := observed(t, snap, c, censusNow, id); got.Liveness != livenessGone || got.Uncertain {
			t.Errorf("%s = %+v, want gone and certain", id, got)
		}
	}
}

// Kills: an arm acting on a runtime before its identity is read (v5.1 O1).
// A listed name with a live pane is unknown until a clean identity read, and
// unknown again when that read failed or found no session ID and no token.
func TestListedUnreadIdentityIsUnknown(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	for _, tc := range []struct {
		name   string
		id     runtimeIdentity
		want   rowLiveness
		reason string
	}{
		{"unread", runtimeIdentity{}, livenessUnknown, observeReasonIdentity},
		{"read-failed", runtimeIdentity{ReadAt: censusNow}, livenessUnknown, observeReasonIdentity},
		{"ownerless", runtimeIdentity{Known: true, Epoch: "1"}, livenessUnknown, observeReasonOwnerless},
		{"own-session", runtimeIdentity{Known: true, SessionID: "gc-1"}, livenessAlive, ""},
		{"token-only", readIdentity(""), livenessAlive, ""},
		{"other-session", readIdentity("gc-9"), livenessOccupied, ""},
	} {
		snap := newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s1": {DeadKnown: true, Identity: tc.id}}, completeBackend("tmux", "s1"))
		got := observed(t, snap, c, censusNow, "gc-1")
		if got.Liveness != tc.want || got.Reason != tc.reason || got.Uncertain != (tc.want == livenessUnknown) {
			t.Errorf("%s: %+v, want %v (reason %q)", tc.name, got, tc.want, tc.reason)
		}
	}
}

// Kills: the census and a fresh read disagreeing on a corpse (I23). The lane
// keeps a remain-on-exit corpse listed, never probes it, and the row reads
// dead (present, pane dead), on every pass: what LL2's fresh read reports as
// Present with Running false (TestFreshReadCorpseIsPresent, tmux). A corpse
// is a start candidate, never gone and never unknown.
func TestCensusAndFreshReadAgreeOnCorpse(t *testing.T) {
	sp := newProbingProvider(func(string, []string) (runtime.Liveness, error) {
		return runtime.Liveness{Corpse: true, ObjectID: "$7"}, nil
	}, "s1")
	sp.inventory["s1"] = runtime.InventoryEntry{Incarnation: "s1:1", DeadKnown: true, AllPanesDead: true}
	sp.env["s1"]["GC_SESSION_ID"] = "gc-1"
	cr := inventoryLaneTestRuntime(t, sp, nil)
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	for pass := 1; pass <= observationRetentionPasses+1; pass++ {
		runTestInventoryPass(cr)
		got := observed(t, cr.inventoryLane.cache.Snapshot(), c, cr.inventoryLane.clock.Now(), "gc-1")
		if got.Liveness != livenessDead || !got.Liveness.startCandidate() {
			t.Fatalf("pass %d: corpse row = %+v, want dead", pass, got)
		}
	}
	if n := sp.probes.Load(); n != 0 {
		t.Fatalf("lane probes on the corpse = %d, want none", n)
	}
	if fresh, err := sp.ObserveLivenessWithError("s1", nil); err != nil || !fresh.Present() || fresh.Running {
		t.Fatalf("fixture fresh read = %+v, %v; want a present corpse", fresh, err)
	}
}
