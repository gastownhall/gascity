package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

var censusNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// censusSession is an open session row with metadata.
func censusSession(id string, meta map[string]string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: session.BeadType, Labels: []string{session.LabelSession}, Status: "open", CreatedAt: censusNow.Add(-time.Hour), Metadata: meta}
}

func censusStore(rows ...beads.Bead) *beads.MemStore {
	return beads.NewMemStoreFrom(0, rows, nil)
}

// censusErrStore fails every list with err, as an unreachable leg does. A
// partial-result err comes with the rows the store holds, as a partial read's
// do.
type censusErrStore struct {
	beads.Store
	err error
}

func (s censusErrStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !beads.IsPartialResult(s.err) {
		return nil, s.err
	}
	rows, _ := s.Store.List(q)
	return rows, s.err
}

// censusUntouchable is a non-exact leg's store. It embeds no store, so any
// call the census makes on it panics; name tells two of them apart.
type censusUntouchable struct {
	beads.Store
	name string
}

// fakeCensusFeed is a scripted P3-2 seam: legs are exact unless named, and
// non-exact legs serve the recordings set for their stores.
type fakeCensusFeed struct {
	nonExact   map[beads.Store]bool
	recordings map[beads.Store]censusRecording
}

func (f *fakeCensusFeed) feed() censusLegFeed {
	return censusLegFeed{
		exact: func(s beads.Store) bool { return !f.nonExact[s] },
		recorded: func(s beads.Store) (censusRecording, bool) {
			rec, ok := f.recordings[s]
			return rec, ok
		},
	}
}

// censusInfos projects session rows as the backstop lane records them: the
// session front door's ListAll.
func censusInfos(t *testing.T, rows ...beads.Bead) []session.Info {
	t.Helper()
	infos, err := sessionFrontDoor(censusStore(rows...)).ListAll(session.ListAllOptions{})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	return infos
}

func censusLegs(stores ...any) []classStoreCandidate {
	var legs []classStoreCandidate
	for i := 0; i < len(stores); i += 2 {
		legs = append(legs, classStoreCandidate{ref: stores[i].(string), store: stores[i+1].(beads.Store)})
	}
	return legs
}

func readCensus(t *testing.T, r *censusReader, now time.Time, cfg *config.City, legs []classStoreCandidate) *sessionCensus {
	t.Helper()
	c, err := r.read(now, cfg, legs)
	if err != nil {
		t.Fatalf("census read: %v", err)
	}
	return c
}

func censusLegNamed(t *testing.T, c *sessionCensus, ref string) censusLeg {
	t.Helper()
	for _, l := range c.Legs {
		if l.Ref == ref {
			return l
		}
	}
	t.Fatalf("census has no leg %q", ref)
	return censusLeg{}
}

// Kills: a non-first leg winning the fold for a shared bead ID (the
// pre-relocation residue in the work ledger standing in for the binding's
// row), and a duplicate counted twice in flight. The unfolded rows keep the
// duplicate, so a marker on either leg is visible to the ledger (C2.11).
func TestCensusSessionsBindingLeadsAndDuplicatesAreNone(t *testing.T) {
	woke := censusNow.Add(-10 * time.Second).Format(time.RFC3339)
	lease := map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": woke, "generation": "3"}
	// The residue copy a migration left behind carries a live lease of its
	// own; counting it too would count one start twice.
	relic := map[string]string{"state": "start-pending", "pending_create_claim": "true", "last_woke_at": woke, "generation": "1"}
	binding := censusStore(censusSession("gc-1", lease))
	// The work-only row's ID sorts before the binding's: canonical order is
	// leg first, then bead ID.
	work := censusStore(censusSession("gc-1", relic), censusSession("gc-0", map[string]string{"state": "active"}))
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{},
		censusLegs("class:sessions", binding, "city:mc", work))

	canonical := c.Canonical()
	if len(canonical) != 2 || canonical[0].Key != (rowKey{"class:sessions", "gc-1"}) || canonical[1].Key != (rowKey{"city:mc", "gc-0"}) {
		t.Fatalf("canonical rows = %+v, want binding gc-1 then work-only gc-0", canonical)
	}
	if got := canonical[0].Info.MetadataState; got != "creating" {
		t.Fatalf("canonical gc-1 state = %q, want the binding's creating", got)
	}
	if len(c.Rows) != 3 {
		t.Fatalf("unfolded rows = %d, want 3 (the duplicate kept)", len(c.Rows))
	}
	dup := c.Rows[rowKey{"city:mc", "gc-1"}]
	if dup.DuplicateOf != "class:sessions" || dup.StartLease || dup.PendingCreate {
		t.Fatalf("duplicate row = %+v, want DuplicateOf the binding and no in-flight facts", dup)
	}
	if !canonical[0].StartLease || canonical[0].Incarnation != 3 {
		t.Fatalf("canonical gc-1 = %+v, want a start lease at incarnation 3", canonical[0])
	}
}

// Kills: an error read as an empty city (P-3), and a rig leg error ignored
// (POOL-047). The sessions leg failing with nothing to serve fails the pass;
// another leg failing leaves the census incomplete and the leg out of the
// ledger's complete legs.
func TestCensusSessionsLegErrorFailsPassOtherLegErrorIsPartial(t *testing.T) {
	down := errors.New("store down")
	feed := &fakeCensusFeed{}
	ok := censusStore(censusSession("gc-1", map[string]string{"state": "active"}))

	if _, err := newCensusReader(feed.feed(), time.Minute).read(censusNow, &config.City{},
		censusLegs("class:sessions", censusErrStore{beads.NewMemStore(), down}, "rig:a", ok)); !errors.Is(err, down) {
		t.Fatalf("sessions-leg failure: err = %v, want the leg's error", err)
	}
	if c, err := newCensusReader(feed.feed(), time.Minute).read(censusNow, &config.City{}, nil); err == nil {
		t.Fatalf("no legs: census %+v, want an error", c)
	}

	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{},
		censusLegs("class:sessions", ok, "rig:a", censusErrStore{beads.NewMemStore(), down}))
	if !c.Incomplete() {
		t.Fatal("rig-leg failure: census complete, want incomplete (no fresh create)")
	}
	if leg := censusLegNamed(t, c, "rig:a"); leg.State != legMissing || !errors.Is(leg.Err, down) {
		t.Fatalf("rig leg = %+v, want missing with the error", leg)
	}
	if got := c.CompleteLegs(); !got["class:sessions"] || got["rig:a"] {
		t.Fatalf("complete legs = %v, want only the sessions leg", got)
	}

	// A partial read's rows still occupy their slots and names, but the leg
	// stays incomplete. On the sessions leg too: only a hard error with
	// nothing to serve fails the pass.
	partial := censusErrStore{censusStore(censusSession("rg-1", map[string]string{"state": "active"})), &beads.PartialResultError{Op: "list", Err: down}}
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{},
		censusLegs("class:sessions", ok, "rig:a", partial))
	if _, kept := c.Rows[rowKey{"rig:a", "rg-1"}]; !kept || !c.Incomplete() || c.CompleteLegs()["rig:a"] {
		t.Fatalf("partial rig read: row kept=%v incomplete=%v complete=%v, want the row, incomplete, not complete", kept, c.Incomplete(), c.CompleteLegs()["rig:a"])
	}
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{},
		censusLegs("class:sessions", partial, "rig:a", ok))
	if _, kept := c.Rows[rowKey{"class:sessions", "rg-1"}]; !kept || !c.Incomplete() || c.CompleteLegs()["class:sessions"] {
		t.Fatalf("partial sessions read: row kept=%v incomplete=%v, want the row kept and the census incomplete", kept, c.Incomplete())
	}

	// A hard error on another leg keeps none of the rows that came with it.
	rig := censusUntouchable{name: "rig"}
	feed = &fakeCensusFeed{nonExact: map[beads.Store]bool{rig: true}, recordings: map[beads.Store]censusRecording{
		rig: {At: censusNow, Err: down, Rows: censusInfos(t, censusSession("rg-2", map[string]string{"state": "active"}))},
	}}
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, censusLegs("class:sessions", ok, "rig:a", rig))
	if _, kept := c.Rows[rowKey{"rig:a", "rg-2"}]; kept || !c.Incomplete() {
		t.Fatalf("hard rig error: row kept=%v incomplete=%v, want no rows and incomplete", kept, c.Incomplete())
	}
}

// Kills: a sessions-leg error failing the pass while its last good is within
// the bound, and a stale sessions leg failing the pass rather than leaving the
// census incomplete. Only a sessions leg with nothing to serve fails it.
func TestCensusSessionsLegLastGoodServesThenStaleIsIncomplete(t *testing.T) {
	feed := &fakeCensusFeed{}
	backing := censusStore(censusSession("gc-1", map[string]string{"state": "active"}))
	broken := censusErrStore{backing, errors.New("cache closed")}
	r := newCensusReader(feed.feed(), time.Minute)
	readCensus(t, r, censusNow, &config.City{}, censusLegs("class:sessions", backing))

	steps := []struct {
		at         time.Duration
		state      censusLegState
		incomplete bool
	}{
		{at: censusLastGoodBound, state: legLastGood},
		{at: censusLastGoodBound + time.Second, state: legStale, incomplete: true},
	}
	for _, s := range steps {
		c, err := r.read(censusNow.Add(s.at), &config.City{}, censusLegs("class:sessions", broken))
		if err != nil {
			t.Fatalf("at +%v: err = %v, want the last good served", s.at, err)
		}
		if leg := c.Legs[0]; leg.State != s.state || c.Incomplete() != s.incomplete {
			t.Fatalf("at +%v: leg %v incomplete=%v, want %v incomplete=%v", s.at, leg.State, c.Incomplete(), s.state, s.incomplete)
		}
		if _, ok := c.Rows[rowKey{"class:sessions", "gc-1"}]; !ok {
			t.Fatalf("at +%v: last good row dropped", s.at)
		}
	}
}

// Kills: unbounded stale reads. A failed read serves the leg's last good rows
// whole until the bound, then the leg is stale: rows kept (Keep), out of the
// complete legs (a missing row no longer proves a close), and reported. A
// backstop recording ages on its own clock, the lane's read time.
func TestCensusNonExactLegLastGoodAgesIntoPartialAfterBound(t *testing.T) {
	sessions := censusStore()
	rig := censusUntouchable{name: "rig"}
	feed := &fakeCensusFeed{nonExact: map[beads.Store]bool{rig: true}, recordings: map[beads.Store]censusRecording{
		rig: {Rows: censusInfos(t, censusSession("rg-1", map[string]string{"state": "asleep"})), At: censusNow},
	}}
	r := newCensusReader(feed.feed(), 30*time.Second)
	legs := censusLegs("class:sessions", sessions, "rig:a", rig)

	steps := []struct {
		at    time.Duration
		rec   *censusRecording
		state censusLegState
	}{
		{at: 10 * time.Second, state: legRead},
		{at: 20 * time.Second, rec: &censusRecording{Err: errors.New("bd timeout")}, state: legLastGood},
		{at: 31 * time.Second, state: legStale},
	}
	for _, s := range steps {
		if s.rec != nil {
			feed.recordings[rig] = *s.rec
		}
		c := readCensus(t, r, censusNow.Add(s.at), &config.City{}, legs)
		leg := censusLegNamed(t, c, "rig:a")
		if leg.State != s.state || !leg.ReadAt.Equal(censusNow) {
			t.Fatalf("at +%v: leg = %+v, want %v read at the recording's time", s.at, leg, s.state)
		}
		if _, ok := c.Rows[rowKey{"rig:a", "rg-1"}]; !ok {
			t.Fatalf("at +%v: last good row dropped", s.at)
		}
		complete := c.CompleteLegs()["rig:a"]
		if complete != (s.state != legStale) || c.StaleLegs()["rig:a"] != (s.state == legStale) {
			t.Fatalf("at +%v: complete=%v stale=%v for state %v", s.at, complete, c.StaleLegs()["rig:a"], s.state)
		}
		// A stale leg leaves the census incomplete (owner decision
		// 2026-10-03): no fresh create anywhere, as legacy.
		if c.Incomplete() != (s.state == legStale) {
			t.Fatalf("at +%v: incomplete=%v for state %v", s.at, c.Incomplete(), s.state)
		}
	}

	// An exact leg ages the same way, on censusLastGoodBound.
	exactRig := censusStore(censusSession("rg-2", nil))
	r = newCensusReader(feed.feed(), 30*time.Second)
	readCensus(t, r, censusNow, &config.City{}, censusLegs("class:sessions", sessions, "rig:b", exactRig))
	broken := censusLegs("class:sessions", sessions, "rig:b", censusErrStore{exactRig, errors.New("closed")})
	if leg := censusLegNamed(t, readCensus(t, r, censusNow.Add(censusLastGoodBound), &config.City{}, broken), "rig:b"); leg.State != legLastGood {
		t.Fatalf("exact leg at the bound = %v, want last-good", leg.State)
	}
	if leg := censusLegNamed(t, readCensus(t, r, censusNow.Add(censusLastGoodBound+time.Second), &config.City{}, broken), "rig:b"); leg.State != legStale {
		t.Fatalf("exact leg past the bound = %v, want stale", leg.State)
	}
}

// Kills: bd subprocess I/O in the pass (F6), and a recording served to the
// wrong leg. A non-exact leg is served from the backstop lane's recording of
// its own store and the store is never called; with no recording the leg is
// missing, not read.
func TestCensusNeverDoesBackingIOOnNonExactLegs(t *testing.T) {
	rig, other := censusUntouchable{name: "rig"}, censusUntouchable{name: "other"}
	closed := censusSession("rg-closed", nil)
	closed.Status = "closed"
	feed := &fakeCensusFeed{nonExact: map[beads.Store]bool{rig: true, other: true}, recordings: map[beads.Store]censusRecording{
		rig:   {At: censusNow, Rows: censusInfos(t, censusSession("rg-1", map[string]string{"state": "active"}), closed)},
		other: {At: censusNow, Rows: censusInfos(t, censusSession("ot-1", map[string]string{"state": "active"}))},
	}}
	legs := censusLegs("class:sessions", censusStore(), "rig:a", rig, "rig:b", other)

	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, legs)
	if len(c.Rows) != 2 {
		t.Fatalf("rows = %v, want the open session row from each recording", c.Rows)
	}
	if _, ok := c.Rows[rowKey{"rig:a", "rg-1"}]; !ok {
		t.Fatal("recorded session row missing")
	}
	if _, ok := c.Rows[rowKey{"rig:b", "ot-1"}]; !ok {
		t.Fatal("second leg's recorded row missing")
	}

	delete(feed.recordings, rig)
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, legs)
	if leg := censusLegNamed(t, c, "rig:a"); leg.State != legMissing || !c.Incomplete() {
		t.Fatalf("no recording: leg = %+v incomplete=%v, want missing and incomplete", leg, c.Incomplete())
	}
}

// Kills: reading an exact leg strict (dirty ⇒ decline), which would fail the
// pass whenever one row is dirty, and serving a dirty row's stale copy. The
// census reads the exact leg through the cache's bounded dirty overlay, so a
// dirty row reads current and the leg counts as read this pass.
func TestCensusExactLegReadsDirtyRowsThroughCacheOverlay(t *testing.T) {
	backing := &cacheBackingCounter{MemStore: censusStore(censusSession("gc-1", map[string]string{"state": "asleep"}), censusSession("gc-2", map[string]string{"state": "asleep"}))}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	backing.armed.Store(true)
	// A lost conditional write evicts the row and leaves it dirty.
	if err := cache.UpdateIfMatch("gc-1", 999, beads.UpdateOpts{Metadata: map[string]string{"x": "y"}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch = %v, want a precondition failure", err)
	}
	if _, clean := cache.CachedList(beads.ListQuery{Type: session.BeadType}); clean {
		t.Fatal("fixture: cache still clean, want a dirty row")
	}
	if err := backing.SetMetadata("gc-1", "state", "creating"); err != nil {
		t.Fatal(err)
	}

	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, censusLegs("class:sessions", cache))
	if leg := c.Legs[0]; leg.State != legRead {
		t.Fatalf("sessions leg = %+v, want read this pass", leg)
	}
	if got := c.Rows[rowKey{"class:sessions", "gc-1"}].Info.MetadataState; got != "creating" {
		t.Fatalf("dirty row state = %q, want the backing's current creating", got)
	}
	if _, ok := c.Rows[rowKey{"class:sessions", "gc-2"}]; !ok {
		t.Fatal("clean row missing")
	}
	if n := backing.reads.Load(); n != 0 {
		t.Fatalf("backing listed %d times, want 0 (one dirty row refreshes by Get)", n)
	}
}

// Kills: name-keyed rows (BEHAVIORS #8, F8). Rows that share an enterprise
// slot-scoped session name are one census row each, and the name index finds
// every one of them.
func TestCensusKeysRowsThatShareANameByBeadID(t *testing.T) {
	var rows []beads.Bead
	for _, id := range []string{"gc-3", "gc-1", "gc-2"} {
		rows = append(rows, censusSession(id, map[string]string{"session_name": "rig--worker-2-pool", "state": "creating"}))
	}
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, censusLegs("class:sessions", censusStore(rows...)))
	canonical := c.Canonical()
	if len(canonical) != 3 || canonical[0].Key.ID != "gc-1" || canonical[2].Key.ID != "gc-3" {
		t.Fatalf("canonical = %+v, want three rows by bead ID", canonical)
	}
	if got := c.RowsNamed("rig--worker-2-pool"); len(got) != 3 {
		t.Fatalf("RowsNamed = %v, want all three", got)
	}

	// A row with no session_name is indexed under the runtime name it
	// derives from its ID, which is the name the inventory lists.
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, censusLegs("class:sessions", censusStore(censusSession("gc-7", map[string]string{"state": "asleep"}))))
	k := rowKey{"class:sessions", "gc-7"}
	if name := c.Rows[k].Info.SessionName; name == "" || len(c.RowsNamed(name)) != 1 || c.RowsNamed(name)[0] != k {
		t.Fatalf("derived name %q: RowsNamed = %v, want gc-7", name, c.RowsNamed(name))
	}
}

// Kills (P3-4 obligation): a start lease counted from last_woke_at alone. A
// row that woke recently but holds no claim and is not creating has no start
// in flight (START-043); the never-started pending create is its own fact,
// within its lease only.
func TestCensusLedgerFactsStartLeaseNeedsClaimOrCreating(t *testing.T) {
	woke := censusNow.Add(-10 * time.Second).Format(time.RFC3339)
	oldWoke := censusNow.Add(-5 * time.Minute).Format(time.RFC3339)
	cases := []struct {
		name                 string
		meta                 map[string]string
		created              time.Time
		startLease, pendingC bool
	}{
		{name: "active-recently-woke", meta: map[string]string{"state": "active", "last_woke_at": woke}},
		{name: "creating-recently-woke", meta: map[string]string{"state": "creating", "last_woke_at": woke}, startLease: true},
		{name: "claim-recently-woke", meta: map[string]string{"state": "active", "pending_create_claim": "true", "last_woke_at": woke}, startLease: true},
		{name: "creating-lease-expired", meta: map[string]string{"state": "creating", "last_woke_at": oldWoke}},
		{name: "claim-unparseable-woke", meta: map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": "soon"}},
		{name: "never-started-fresh", meta: map[string]string{"state": "start-pending", "pending_create_claim": "true"}, created: censusNow.Add(-time.Minute), pendingC: true},
		{name: "never-started-expired", meta: map[string]string{"state": "start-pending", "pending_create_claim": "true"}, created: censusNow.Add(-11 * time.Minute)},
		{name: "claim-on-stopped-row", meta: map[string]string{"state": "stopped", "pending_create_claim": "true"}, created: censusNow.Add(-time.Minute)},
		// Started: its claim's start is in flight, but it is no longer a
		// never-started pending create.
		{name: "claim-started-fresh", meta: map[string]string{"state": "start-pending", "pending_create_claim": "true", "last_woke_at": woke}, created: censusNow.Add(-time.Minute), startLease: true},
	}
	var rows []beads.Bead
	for _, tc := range cases {
		b := censusSession(tc.name, tc.meta)
		if !tc.created.IsZero() {
			b.CreatedAt = tc.created
		}
		rows = append(rows, b)
	}
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, &config.City{}, censusLegs("class:sessions", censusStore(rows...)))
	for _, tc := range cases {
		row := c.Rows[rowKey{"class:sessions", tc.name}]
		if row.StartLease != tc.startLease || row.PendingCreate != tc.pendingC {
			t.Errorf("%s: StartLease=%v PendingCreate=%v, want %v %v", tc.name, row.StartLease, row.PendingCreate, tc.startLease, tc.pendingC)
		}
	}

	// The lease follows the configured startup timeout, not the default.
	slow := &config.City{Session: config.SessionConfig{StartupTimeout: "5m"}}
	threeMin := censusSession("gc-slow", map[string]string{"state": "creating", "last_woke_at": censusNow.Add(-3 * time.Minute).Format(time.RFC3339)})
	c = readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, slow, censusLegs("class:sessions", censusStore(threeMin)))
	if !c.Rows[rowKey{"class:sessions", "gc-slow"}].StartLease {
		t.Error("startup_timeout 5m, woke 3m ago: no start lease, want one")
	}
}

// Kills: a create planned for an identity an open unconfirmed create still
// holds (a doomed create, retried every pass, F8), and a lease the census
// index answers differently from legacy's snapshot check.
func TestCensusIdentityLeaseHolderMatchesLegacyCheck(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig"}, {Name: "other", Dir: "rig"}}}
	pool := func(id, state, agent, template string) beads.Bead {
		return censusSession(id, map[string]string{"state": state, "agent_name": agent, "template": template, poolManagedMetadataKey: "true"})
	}
	rows := []beads.Bead{
		pool("gc-1", "creating", "rig/worker-2", "rig/worker"),
		pool("gc-2", "failed-create", "rig/worker-3", "rig/worker"),
		pool("gc-3", "asleep", "rig/worker-4", "rig/worker"),
		pool("gc-4", "start-pending", "rig/other-1", "rig/other"),
		pool("gc-5", "start-pending", "rig/worker-5", ""),
		// Not pool-managed: no lease.
		censusSession("gc-6", map[string]string{"state": "creating", "agent_name": "rig/worker-6", "template": "rig/worker", "session_origin": "manual"}),
	}
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, cfg, censusLegs("class:sessions", censusStore(rows...)))
	snapshot := newSessionBeadSnapshot(rows)

	cases := []struct {
		template, identity string
		holder             string
	}{
		{"rig/worker", "rig/worker-2", "gc-1"},
		{"rig/worker", "rig/worker-3", "gc-2"},
		{"rig/worker", "rig/worker-4", ""},
		{"rig/worker", "rig/other-1", ""},
		{"rig/other", "rig/other-1", "gc-4"},
		{"rig/worker", "rig/worker-5", "gc-5"},
		{"rig/worker", "rig/worker-9", ""},
		{"rig/worker", "rig/worker-6", ""},
		{"rig/worker", "", ""},
	}
	for _, tc := range cases {
		k, held := c.IdentityLeaseHolder(cfg, tc.template, tc.identity)
		if held != (tc.holder != "") || k.ID != tc.holder {
			t.Errorf("%s %q: holder=%q held=%v, want %q", tc.template, tc.identity, k.ID, held, tc.holder)
		}
		legacy := ensurePoolIdentityNotHeldByOpenRow(nil, cfg, snapshot, tc.template, tc.identity)
		if (legacy != nil) != held {
			t.Errorf("%s %q: census held=%v, legacy err=%v", tc.template, tc.identity, held, legacy)
		}
	}
}

// Kills: a lease checked against canonical rows only. The create effect
// checks every copy on every leg under its locks (freshPoolAvailabilityInfos
// keeps copies whose identity fields differ), so a duplicate copy whose
// agent_name differs from the canonical row's holds its own identity, and the
// plan-time check must agree.
func TestCensusIdentityLeaseHolderCountsEveryCopy(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig"}}}
	canonical := censusSession("gc-1", map[string]string{"state": "asleep", "agent_name": "rig/worker-2", "template": "rig/worker", poolManagedMetadataKey: "true"})
	relic := censusSession("gc-1", map[string]string{"state": "creating", "agent_name": "rig/worker-7", "template": "rig/worker", poolManagedMetadataKey: "true"})
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, cfg,
		censusLegs("class:sessions", censusStore(canonical), "city:mc", censusStore(relic)))

	k, held := c.IdentityLeaseHolder(cfg, "rig/worker", "rig/worker-7")
	if !held || k != (rowKey{"city:mc", "gc-1"}) {
		t.Fatalf("holder = %v held=%v, want the duplicate copy on city:mc", k, held)
	}
	if _, held := c.IdentityLeaseHolder(cfg, "rig/worker", "rig/worker-2"); held {
		t.Fatal("settled canonical copy holds a lease")
	}
	union := newSessionBeadSnapshotFromInfos(append(censusInfos(t, canonical), censusInfos(t, relic)...))
	if err := ensurePoolIdentityNotHeldByOpenRow(nil, cfg, union, "rig/worker", "rig/worker-7"); err == nil {
		t.Fatal("fixture: legacy's locked check over the copy union does not hold rig/worker-7")
	}
}

// Kills: a lease dedupe key missing the alias or the session_name. Two copies
// of one bead that agree on agent_name but differ in alias, or in
// session_name, are two holders, as freshPoolAvailabilityInfos keeps them
// (its key is the same four fields): only the later copy's template matches,
// so dropping it loses the lease. Copies equal in all four fields are one
// holder, the first, as there.
func TestCensusIdentityLeaseDedupesCopiesByIDNameAliasAndAgent(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig"}, {Name: "other", Dir: "rig"}}}
	lease := func(template string, extra map[string]string) beads.Bead {
		meta := map[string]string{"state": "creating", "agent_name": "rig/worker-7", "template": template, poolManagedMetadataKey: "true"}
		for k, v := range extra {
			meta[k] = v
		}
		return censusSession("gc-1", meta)
	}
	for _, tc := range []struct {
		name         string
		first, later map[string]string
		held         bool
	}{
		{"alias differs", map[string]string{"alias": "a1"}, map[string]string{"alias": "a2"}, true},
		{"session_name differs", map[string]string{"session_name": "s1"}, map[string]string{"session_name": "s2"}, true},
		{"all four equal", map[string]string{"alias": "a1"}, map[string]string{"alias": "a1"}, false},
	} {
		first, later := lease("rig/other", tc.first), lease("rig/worker", tc.later)
		feed := &fakeCensusFeed{}
		c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, cfg,
			censusLegs("class:sessions", censusStore(first), "city:mc", censusStore(later)))
		k, held := c.IdentityLeaseHolder(cfg, "rig/worker", "rig/worker-7")
		if held != tc.held || (held && k != (rowKey{"city:mc", "gc-1"})) {
			t.Errorf("%s: holder = %v held=%v, want held=%v by the city:mc copy", tc.name, k, held, tc.held)
		}
	}
}

// Kills: a ledger view without endpoints, with every leg listed as complete,
// or without a leg's duplicate copy. The adapter carries every row on every
// leg with its config-only endpoint and only the complete legs (P3-4
// obligations).
func TestCensusLedgerViewCarriesEndpointsAndCompleteLegs(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig", Provider: "p1"}, {Name: "relay", Upstream: "broker"}}}
	woke := censusNow.Add(-10 * time.Second).Format(time.RFC3339)
	sessions := censusStore(
		censusSession("gc-1", map[string]string{"state": "creating", "template": "rig/worker", "pending_create_claim": "true", "last_woke_at": woke, "generation": "4", "instance_token": "tok-1"}),
		censusSession("gc-2", map[string]string{"state": "active", "template": "relay"}),
		censusSession("gc-3", map[string]string{"state": "active", "template": "gone", "provider": "rowp"}),
		// No stored template: the endpoint comes from the template the
		// agent_name resolves to, not the raw field.
		censusSession("gc-4", map[string]string{"state": "active", "agent_name": "rig/worker"}),
	)
	work := censusStore(censusSession("gc-1", map[string]string{"state": "creating", "template": "rig/worker"}))
	feed := &fakeCensusFeed{}
	legs := censusLegs("class:sessions", sessions, "city:mc", work, "rig:a", censusErrStore{beads.NewMemStore(), errors.New("down")})
	view := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, cfg, legs).Ledger(cfg)

	want := map[rowKey]ledgerRow{
		{"class:sessions", "gc-1"}: {Incarnation: 4, InstanceToken: "tok-1", Endpoint: "provider:p1", StartLease: true},
		{"class:sessions", "gc-2"}: {Endpoint: "upstream:broker"},
		{"class:sessions", "gc-3"}: {Endpoint: "provider:rowp"},
		{"class:sessions", "gc-4"}: {Endpoint: "provider:p1"},
		{"city:mc", "gc-1"}:        {Endpoint: "provider:p1"},
	}
	if len(view.Rows) != len(want) {
		t.Fatalf("ledger rows = %+v, want %d", view.Rows, len(want))
	}
	for k, row := range want {
		if got := view.Rows[k]; got != row {
			t.Errorf("%v: %+v, want %+v", k, got, row)
		}
	}
	if !view.Legs["class:sessions"] || !view.Legs["city:mc"] || view.Legs["rig:a"] {
		t.Fatalf("ledger legs = %v, want the two complete legs only", view.Legs)
	}
}

// Kills: an enterprise-only state read as known (a start of a row main does
// not understand), the drain-ack stop-pending state read as unknown, and the
// per-template report missing such rows (F9, OQ-4).
func TestCensusUnknownStatesCountedPerTemplate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig"}}}
	rows := []beads.Bead{
		censusSession("gc-1", map[string]string{"state": "draining", "template": "rig/worker"}),
		censusSession("gc-2", map[string]string{"state": "gc_swept", "template": "rig/worker"}),
		censusSession("gc-3", map[string]string{"state": "draining", "state_reason": session.DrainAckStopPendingReason, "template": "rig/worker"}),
		censusSession("gc-4", map[string]string{"state": "asleep", "template": "rig/worker"}),
	}
	feed := &fakeCensusFeed{}
	c := readCensus(t, newCensusReader(feed.feed(), time.Minute), censusNow, cfg, censusLegs("class:sessions", censusStore(rows...)))
	var unknown []string
	for _, row := range c.Canonical() {
		if row.UnknownState {
			unknown = append(unknown, row.Key.ID)
		}
	}
	if strings.Join(unknown, ",") != "gc-1,gc-2" {
		t.Fatalf("unknown-state rows = %v, want gc-1,gc-2", unknown)
	}
	if got := c.UnknownStates(cfg); len(got) != 1 || got["rig/worker"] != 2 {
		t.Fatalf("UnknownStates = %v, want rig/worker: 2", got)
	}
}
