package main

import (
	"context"
	"errors"
	"slices"
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

func censusLegs(stores ...any) []classStoreCandidate {
	var legs []classStoreCandidate
	for i := 0; i < len(stores); i += 2 {
		legs = append(legs, classStoreCandidate{ref: stores[i].(string), store: stores[i+1].(beads.Store)})
	}
	return legs
}

func readCensus(t *testing.T, now time.Time, legs []classStoreCandidate) *sessionCensus {
	t.Helper()
	c, err := readSessionCensus(now, legs)
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
	// The residue copy a migration left behind carries a claim of its own;
	// counting it too would count one bring-up twice.
	relic := map[string]string{"state": "start-pending", "pending_create_claim": "true", "last_woke_at": woke, "generation": "1"}
	binding := censusStore(censusSession("gc-1", lease))
	// The work-only row's ID sorts before the binding's: canonical order is
	// leg first, then bead ID.
	work := censusStore(censusSession("gc-1", relic), censusSession("gc-0", map[string]string{"state": "active"}))
	c := readCensus(t, censusNow,
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
	if dup.DuplicateOf != "class:sessions" || dup.PendingCreate {
		t.Fatalf("duplicate row = %+v, want DuplicateOf the binding and no in-flight facts", dup)
	}
	if !canonical[0].PendingCreate || canonical[0].Incarnation != 3 {
		t.Fatalf("canonical gc-1 = %+v, want a pending create at incarnation 3", canonical[0])
	}
}

// Kills: a read error mistaken for an empty city (P-3). A hard error on the
// sessions leg fails the pass; one on another leg keeps that leg's rows
// (none).
func TestCensusSessionsLegErrorFailsPass(t *testing.T) {
	down := errors.New("store down")
	ok := censusStore(censusSession("gc-1", map[string]string{"state": "active"}))

	if _, err := readSessionCensus(censusNow,
		censusLegs("class:sessions", censusErrStore{beads.NewMemStore(), down}, "rig:a", ok)); !errors.Is(err, down) {
		t.Fatalf("sessions-leg failure: err = %v, want the leg's error", err)
	}
	if c, err := readSessionCensus(censusNow, nil); err == nil {
		t.Fatalf("no legs: census %+v, want an error", c)
	}

	c := readCensus(t, censusNow, censusLegs("class:sessions", ok, "rig:a", censusErrStore{beads.NewMemStore(), down}))
	if leg := censusLegNamed(t, c, "rig:a"); !errors.Is(leg.Err, down) {
		t.Fatalf("rig leg = %+v, want the error", leg)
	}
	if c.Partial() {
		t.Fatal("hard rig error: census partial, want only a partial read to retain")
	}
}

// Kills: a partial read that lets templates shrink. On any leg, the sessions
// leg included, a partial read keeps the rows it returned and makes the
// census partial (causeStoreQueryPartial); it does not fail the pass.
func TestCensusPartialLegRetains(t *testing.T) {
	ok := censusStore(censusSession("gc-1", map[string]string{"state": "active"}))
	partial := censusErrStore{censusStore(censusSession("rg-1", map[string]string{"state": "active"})), &beads.PartialResultError{Op: "list", Err: errors.New("down")}}
	for _, tc := range []struct {
		legs       []classStoreCandidate
		partialLeg string
	}{
		{censusLegs("class:sessions", ok, "rig:a", partial), "rig:a"},
		{censusLegs("class:sessions", partial, "rig:a", ok), "class:sessions"},
	} {
		c := readCensus(t, censusNow, tc.legs)
		if _, kept := c.Rows[rowKey{tc.partialLeg, "rg-1"}]; !kept || !c.Partial() {
			t.Fatalf("partial %s read: row kept=%v partial=%v, want the row kept and the census partial", tc.partialLeg, kept, c.Partial())
		}
	}

	d, err := decideAllocation(allocInputs{Now: censusNow, Census: readCensus(t, censusNow, censusLegs("class:sessions", ok, "rig:a", partial))})
	if err != nil {
		t.Fatal(err)
	}
	if d.Snapshot.Mode != modePartial || !slices.Equal(d.Snapshot.Partial.Global, []string{causeStoreQueryPartial}) {
		t.Fatalf("partial census: mode=%v causes=%v, want partial with %s", d.Snapshot.Mode, d.Snapshot.Partial.Global, causeStoreQueryPartial)
	}
}

// Kills: a census that reads the external-reads lane's recordings again. A
// lane-fed leg's rows come from its CachingStore, as legacy's census reads
// them: a write through the cache is visible at once, and the backing is
// never listed in the pass.
func TestCensusReadsCacheOnEveryLeg(t *testing.T) {
	backing := &cacheBackingCounter{MemStore: censusStore(censusSession("rg-1", map[string]string{"state": "asleep"}))}
	rig := beads.NewCachingStoreForTest(backing, nil)
	if err := rig.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	backing.armed.Store(true)
	if err := rig.SetMetadata("rg-1", "state", "creating"); err != nil {
		t.Fatal(err)
	}
	c := readCensus(t, censusNow, censusLegs("class:sessions", censusStore(), "rig:a", rig))
	if got := c.Rows[rowKey{"rig:a", "rg-1"}].Info.MetadataState; got != "creating" {
		t.Fatalf("rig row state = %q, want the cache's creating", got)
	}
	if leg := censusLegNamed(t, c, "rig:a"); leg.Err != nil {
		t.Fatalf("rig leg = %+v, want read", leg)
	}
	if n := backing.reads.Load(); n != 0 {
		t.Fatalf("backing listed %d times, want 0 (the cache serves the leg)", n)
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

	c := readCensus(t, censusNow, censusLegs("class:sessions", cache))
	if leg := c.Legs[0]; leg.Err != nil {
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
	c := readCensus(t, censusNow, censusLegs("class:sessions", censusStore(rows...)))
	canonical := c.Canonical()
	if len(canonical) != 3 || canonical[0].Key.ID != "gc-1" || canonical[2].Key.ID != "gc-3" {
		t.Fatalf("canonical = %+v, want three rows by bead ID", canonical)
	}
	if got := c.RowsNamed("rig--worker-2-pool"); len(got) != 3 {
		t.Fatalf("RowsNamed = %v, want all three", got)
	}

	// A row with no session_name is indexed under the runtime name it
	// derives from its ID, which is the name the inventory lists.
	c = readCensus(t, censusNow, censusLegs("class:sessions", censusStore(censusSession("gc-7", map[string]string{"state": "asleep"}))))
	k := rowKey{"class:sessions", "gc-7"}
	if name := c.Rows[k].Info.SessionName; name == "" || len(c.RowsNamed(name)) != 1 || c.RowsNamed(name)[0] != k {
		t.Fatalf("derived name %q: RowsNamed = %v, want gc-7", name, c.RowsNamed(name))
	}
}

// Kills a lease creeping back into the census (v5 P4, SC A5): PendingCreate
// is the claim alone, whatever last_woke_at, the state or the row's age say.
func TestCensusPendingCreateIsTheClaim(t *testing.T) {
	woke := censusNow.Add(-10 * time.Second).Format(time.RFC3339)
	old := censusNow.Add(-time.Hour)
	cases := map[string]struct {
		meta    map[string]string
		pending bool
	}{
		"creating-recently-woke": {meta: map[string]string{"state": "creating", "last_woke_at": woke}},
		"active-recently-woke":   {meta: map[string]string{"state": "active", "last_woke_at": woke}},
		"claim-recently-woke":    {meta: map[string]string{"state": "active", "pending_create_claim": "true", "last_woke_at": woke}, pending: true},
		"claim-an-hour-old":      {meta: map[string]string{"state": "start-pending", "pending_create_claim": "true"}, pending: true},
	}
	var rows []beads.Bead
	for name, tc := range cases {
		b := censusSession(name, tc.meta)
		b.CreatedAt = old
		rows = append(rows, b)
	}
	c := readCensus(t, censusNow, censusLegs("class:sessions", censusStore(rows...)))
	for name, tc := range cases {
		if got := c.Rows[rowKey{"class:sessions", name}].PendingCreate; got != tc.pending {
			t.Errorf("%s: PendingCreate = %v, want %v", name, got, tc.pending)
		}
	}
}

// Kills: a bring-up projection without endpoints or tokens, out of key
// order, without a leg's duplicate copy, or counting the duplicate's claim.
func TestCensusBringUpCarriesEndpointsTokensAndClaims(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "rig", Provider: "p1"}, {Name: "relay", Upstream: "broker"}}}
	sessions := censusStore(
		censusSession("gc-1", map[string]string{"state": "creating", "template": "rig/worker", "pending_create_claim": "true", "generation": "4", "instance_token": "tok-1"}),
		censusSession("gc-2", map[string]string{"state": "active", "template": "relay"}),
		censusSession("gc-3", map[string]string{"state": "active", "template": "gone", "provider": "rowp"}),
		// No stored template: the endpoint comes from the template the
		// agent_name resolves to, not the raw field.
		censusSession("gc-4", map[string]string{"state": "active", "agent_name": "rig/worker"}),
	)
	work := censusStore(censusSession("gc-1", map[string]string{"state": "creating", "template": "rig/worker", "pending_create_claim": "true", "instance_token": "tok-w"}))
	got := readCensus(t, censusNow, censusLegs("class:sessions", sessions, "city:mc", work)).BringUp(cfg)
	want := []bringUpRow{
		{Key: rowKey{"city:mc", "gc-1"}, Token: "tok-w", Endpoint: "provider:p1"},
		{Key: rowKey{"class:sessions", "gc-1"}, Token: "tok-1", Endpoint: "provider:p1", PendingCreate: true},
		{Key: rowKey{"class:sessions", "gc-2"}, Endpoint: "upstream:broker"},
		{Key: rowKey{"class:sessions", "gc-3"}, Endpoint: "provider:rowp"},
		{Key: rowKey{"class:sessions", "gc-4"}, Endpoint: "provider:p1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("BringUp = %+v\nwant %+v", got, want)
	}
}

// Kills: an enterprise-only state read as known (a start of a row main does
// not understand), and the drain-ack stop-pending state read as unknown (F9).
func TestCensusUnknownStateRows(t *testing.T) {
	rows := []beads.Bead{
		censusSession("gc-1", map[string]string{"state": "draining", "template": "rig/worker"}),
		censusSession("gc-2", map[string]string{"state": "gc_swept", "template": "rig/worker"}),
		censusSession("gc-3", map[string]string{"state": "draining", "state_reason": session.DrainAckStopPendingReason, "template": "rig/worker"}),
		censusSession("gc-4", map[string]string{"state": "asleep", "template": "rig/worker"}),
	}
	c := readCensus(t, censusNow, censusLegs("class:sessions", censusStore(rows...)))
	var unknown []string
	for _, row := range c.Canonical() {
		if row.UnknownState {
			unknown = append(unknown, row.Key.ID)
		}
	}
	if strings.Join(unknown, ",") != "gc-1,gc-2" {
		t.Fatalf("unknown-state rows = %v, want gc-1,gc-2", unknown)
	}
}
