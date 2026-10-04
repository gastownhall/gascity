package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// allocNow is the decide tests' one clock.
var allocNow = censusNow

const allocSessionsLeg = "class:sessions"

// allocFixture builds one pass's inputs: census legs read through the real
// census reader (every leg exact), an observation cache with one complete
// inventory pass, and demand set by the test.
type allocFixture struct {
	t      *testing.T
	legs   []classStoreCandidate
	attrs  map[string]InventoryAttrs
	listed []string
	// noInventory leaves the observation cache empty: liveness unknown.
	noInventory bool
	// activity sets a known last-activity fact on listed names; facts sets
	// further fresh facts, as a session key's probe write-back would.
	activity map[string]time.Time
	facts    map[string]map[FactKind]ObsFact
	in       allocInputs
}

func newAllocFixture(t *testing.T, cfg *config.City) *allocFixture {
	t.Helper()
	return &allocFixture{
		t:     t,
		attrs: make(map[string]InventoryAttrs),
		in: allocInputs{
			Now:              allocNow,
			Epoch:            "e1",
			SelGen:           1,
			Cfg:              cfg,
			ConfigRev:        "rev-1",
			CityPath:         "/city",
			CityName:         "city",
			ObsMaxAge:        observeMaxAge,
			ScaleCheckMaxAge: time.Minute,
		},
	}
}

// sessions sets the sessions leg's rows.
func (f *allocFixture) sessions(rows ...beads.Bead) *allocFixture {
	f.legs = append([]classStoreCandidate{{ref: allocSessionsLeg, store: censusStore(rows...)}}, f.legs...)
	return f
}

// rigLeg adds a further census leg, rig:a.
func (f *allocFixture) rigLeg(rows ...beads.Bead) *allocFixture {
	f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: censusStore(rows...)})
	return f
}

// alive lists runtime name as a running pane with attrs.
func (f *allocFixture) alive(name string, attrs InventoryAttrs) *allocFixture {
	attrs.DeadKnown = true
	f.attrs[name] = attrs
	f.listed = append(f.listed, name)
	return f
}

// corpse lists runtime name as an exited pane.
func (f *allocFixture) corpse(name string) *allocFixture {
	f.attrs[name] = InventoryAttrs{DeadKnown: true, AllPanesDead: true, AttachedKnown: true}
	f.listed = append(f.listed, name)
	return f
}

// fact sets a fresh fact on a listed runtime name.
func (f *allocFixture) fact(name string, kind FactKind, v ObsFact) *allocFixture {
	if f.facts == nil {
		f.facts = make(map[string]map[FactKind]ObsFact)
	}
	if f.facts[name] == nil {
		f.facts[name] = make(map[FactKind]ObsFact)
	}
	f.facts[name][kind] = v
	return f
}

func (f *allocFixture) census() *sessionCensus {
	f.t.Helper()
	feed := &fakeCensusFeed{}
	return readCensus(f.t, newCensusReader(feed.feed()), f.in.Now, f.in.Cfg, f.legs)
}

func (f *allocFixture) observation() *ObservationSnapshot {
	cache := newObserveCache()
	if f.noInventory {
		return cache.Snapshot()
	}
	snap := cache.publish(f.in.Now, f.attrs, completeBackend("tmux", f.listed...))
	if len(f.activity) == 0 && len(f.facts) == 0 {
		return snap
	}
	// No lane publishes activity yet (F1); a session key's write-back
	// would land like this.
	clone := *snap
	clone.ByName = make(map[string]RuntimeObservation, len(snap.ByName))
	for name, obs := range snap.ByName {
		if at, ok := f.activity[name]; ok {
			obs.LastActivity, obs.LastActivityKnown = at, true
		}
		for kind, v := range f.facts[name] {
			*obs.fact(kind) = RuntimeFact{Value: v, ObservedAt: f.in.Now, Source: SourceProbe}
		}
		clone.ByName[name] = obs
	}
	return &clone
}

// inputs is the pass's inputs with the census read and the observation
// published.
func (f *allocFixture) inputs() allocInputs {
	f.t.Helper()
	if len(f.legs) == 0 {
		f.sessions()
	}
	in := f.in
	in.Census = f.census()
	in.Obs = f.observation()
	return in
}

// decide runs one pass.
func (f *allocFixture) decide() allocDecision {
	f.t.Helper()
	return mustDecide(f.t, f.inputs())
}

// decideSelecting runs the desire steps with ids selected as the plan steps
// would select a pool instance, so they are tested apart from demand and
// realization.
func (f *allocFixture) decideSelecting(ids ...string) allocDecision {
	f.t.Helper()
	in := f.inputs()
	p := newDecidePass(in)
	p.prepare()
	for _, id := range ids {
		k, ok := p.byID[id]
		if !ok {
			f.t.Fatalf("%s is not a managed row", id)
		}
		p.selected[k] = &selection{ref: desiredConfigRef{ConfigRev: in.ConfigRev, AgentTemplate: p.snap.Entries[k].Template, ResolveKind: resolveInstance}}
	}
	return p.finish()
}

// mustDecide runs one pass and fails the test on a refused one.
func mustDecide(t *testing.T, in allocInputs) allocDecision {
	t.Helper()
	d, err := decideAllocation(in)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return d
}

// sessionRow is an open session row.
func sessionRow(id string, meta ...string) beads.Bead {
	m := make(map[string]string)
	for i := 0; i+1 < len(meta); i += 2 {
		m[meta[i]] = meta[i+1]
	}
	return censusSession(id, m)
}

// poolRow is a pool-managed row of template at slot, in state.
func poolRow(id, template string, slot int, state string, meta ...string) beads.Bead {
	base := []string{
		"template", template, "state", state, "pool_managed", "true",
		"session_name", "s-" + id, "agent_name", fmt.Sprintf("%s-%d", template, slot),
		"pool_slot", fmt.Sprint(slot), "generation", "1",
	}
	return sessionRow(id, append(base, meta...)...)
}

// chatRow is a row of the configured named session "chat".
func chatRow(id, generation string, meta ...string) beads.Bead {
	base := []string{
		"template", "chat", "state", "asleep", "session_name", "s-" + id,
		"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always",
		"generation", generation,
	}
	return sessionRow(id, append(base, meta...)...)
}

func chatCity(mode string) *config.City {
	return &config.City{
		Agents:        []config.Agent{{Name: "chat"}},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: mode}},
	}
}

func allocPoolAgent(name string, maxActive int) config.Agent {
	return config.Agent{Name: name, MaxActiveSessions: intPtr(maxActive)}
}

func entryOf(t *testing.T, d allocDecision, id string) *selectionEntry {
	t.Helper()
	for k, e := range d.Snapshot.Entries {
		if k.ID == id {
			return e
		}
	}
	t.Fatalf("no entry for %s; entries: %v", id, entryIDs(d))
	return nil
}

func entryIDs(d allocDecision) []string {
	var out []string
	for k, e := range d.Snapshot.Entries {
		out = append(out, fmt.Sprintf("%s/%s:%s(%s)", k.Leg, k.ID, e.Desired, e.Reason))
	}
	return out
}

func ago(d time.Duration) string { return allocNow.Add(-d).Format(time.RFC3339) }

// Kills: an empty snapshot read as drain-all (#41), and Wake from a
// suspended city (POOL-001, C2.1). Every row has an entry; live rows drain
// as suspended; the canonical named row stays InDesired asleep; a loser
// stays None.
func TestAllocator_CitySuspended_PublishesSuspendedNotEmptySelection(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{allocPoolAgent("worker", 3), {Name: "chat"}},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: "always"}},
	}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		chatRow("gc-2", "3", "state", "active", "session_name", "chat"),
		chatRow("gc-4", "1"),
		sessionRow("gc-3", "template", "worker", "state", "enterprise-only", "session_name", "s-gc-3"),
	).alive("s-gc-1", InventoryAttrs{}).alive("chat", InventoryAttrs{})
	f.in.CitySuspended = true
	d := f.decide()
	if d.Snapshot.Mode != modeSuspended {
		t.Fatalf("mode = %s, want suspended", d.Snapshot.Mode)
	}
	if len(d.Snapshot.Entries) != 4 {
		t.Fatalf("entries = %v, want one per row", entryIDs(d))
	}
	for _, e := range d.Snapshot.Entries {
		if e.Desired == desireWake {
			t.Fatalf("Wake in a suspended city: %v", entryIDs(d))
		}
	}
	if e := entryOf(t, d, "gc-1"); e.Desired != desireDrain || e.DrainReason != drainSuspended || e.Reason != reasonSuspendedCity {
		t.Errorf("pool row: %s/%s/%s, want drain suspended", e.Desired, e.DrainReason, e.Reason)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired != desireSleep || !e.InDesired || e.DrainReason != drainSuspended {
		t.Errorf("canonical named row: %s indesired=%v, want an InDesired sleep (SESS-054/064)", e.Desired, e.InDesired)
	}
	if e := entryOf(t, d, "gc-4"); e.Desired != desireNone || e.Reason != reasonIdentityLoser {
		t.Errorf("identity loser: %s/%s, want none", e.Desired, e.Reason)
	}
	if e := entryOf(t, d, "gc-3"); e.Desired != desireNone || e.Reason != reasonUnknownState {
		t.Errorf("unknown-state row: %s/%s, want none", e.Desired, e.Reason)
	}
}

// Kills: v2 killing on suspend what legacy leaves alone (owner decision at
// P3-5a review; session_reconciler.go:2414-2436): a partial store read, an
// uncertain liveness, a pending create within its lease and open assigned
// work each keep the row, and the row carries its assigned work.
func TestAllocator_CitySuspended_KeepsWhatLegacySuspendSpares(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	live := func(id string, slot int) beads.Bead { return poolRow(id, "worker", slot, "active") }
	cases := []struct {
		label  string
		setup  func(*allocFixture)
		reason string
	}{
		{"control", func(*allocFixture) {}, ""},
		{"store-partial", func(f *allocFixture) { f.in.Demand.StorePartial = true }, reasonPartialRetain},
		{"liveness-unknown", func(f *allocFixture) { f.noInventory = true }, reasonObservationUncertain},
		{"assigned-work", func(f *allocFixture) {
			f.in.Demand.AssignedWork = []beads.Bead{
				{ID: "w-0", Status: "closed", Assignee: "s-gc-1"},
				{ID: "w-1", Status: "open", Assignee: "s-gc-1"},
			}
		}, reasonAssignedWork},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg).sessions(live("gc-1", 1)).alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
		f.in.CitySuspended = true
		tc.setup(f)
		e := entryOf(t, f.decide(), "gc-1")
		switch {
		case tc.reason == "" && e.Desired != desireDrain:
			t.Errorf("%s: %s/%s, want a suspended drain", tc.label, e.Desired, e.Reason)
		case tc.reason != "" && (e.Desired != desireKeep || e.Reason != tc.reason):
			t.Errorf("%s: %s/%s, want keep %s", tc.label, e.Desired, e.Reason, tc.reason)
		}
		if tc.label == "assigned-work" && (e.AssignedWork == nil || e.AssignedWork.BeadID != "w-1" || e.AssignedWork.Claimed) {
			t.Errorf("assigned work = %+v, want the open w-1, unclaimed", e.AssignedWork)
		}
	}

	// In-progress work under the row's alias is claimed; a pending create
	// within its lease keeps; one past its lease is a rollback candidate.
	f := newAllocFixture(t, cfg).sessions(
		live("gc-1", 1),
		poolRow("gc-2", "worker", 2, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(time.Minute)),
		poolRow("gc-3", "worker", 3, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(time.Hour)),
	).alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
	f.in.CitySuspended = true
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-2", Status: "in_progress", Assignee: "gc-1"}}
	d := f.decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireKeep || e.AssignedWork == nil || !e.AssignedWork.Claimed {
		t.Errorf("row with claimed work = %s %+v, want keep, claimed", e.Desired, e.AssignedWork)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired != desireKeep || e.Reason != reasonPendingCreate {
		t.Errorf("pending create in its lease = %s/%s, want keep pending-create", e.Desired, e.Reason)
	}
	if e := entryOf(t, d, "gc-3"); e.Desired != desireNone || e.Reason != reasonRollbackCandidate {
		t.Errorf("expired pending create = %s/%s, want a rollback candidate", e.Desired, e.Reason)
	}
}

// Kills: the suspended city's keeps leaking into a normal pass: there an
// undesired row with assigned work, or a pending create within its lease,
// drains; the session key's drain gates hold it (CONTRACT §2.2).
func TestAllocator_NormalPassDrainsUndesiredRowsWithWork(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "worker", 2, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(time.Minute)),
	).alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "gc-1"}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	d := f.decideSelecting()
	for _, id := range []string{"gc-1", "gc-2"} {
		if e := entryOf(t, d, id); e.Desired != desireDrain {
			t.Errorf("%s = %s/%s, want drain", id, e.Desired, e.Reason)
		}
	}
	if e := entryOf(t, d, "gc-1"); e.AssignedWork == nil || e.AssignedWork.BeadID != "w-1" {
		t.Errorf("undesired row's assigned work = %+v, want w-1 carried for the session key's gate", e.AssignedWork)
	}
}

// Kills: a row whose runtime name another bead's runtime holds drained,
// closed, woken or started (C11, POOL-082, #8). Work assigned by session
// name would match it too; it stays out of the awake input and is
// None(name-occupied), in a normal and in a suspended city, while the owner
// wakes for the work.
func TestAllocator_OccupiedNameIsNoneNeverGrantedOrDrained(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	for _, suspended := range []bool{false, true} {
		for _, ids := range [][2]string{{"gc-a", "gc-b"}, {"gc-b", "gc-a"}} {
			owner, sibling := ids[0], ids[1]
			f := newAllocFixture(t, cfg).sessions(
				poolRow(owner, "worker", 1, "active", "session_name", "shared"),
				poolRow(sibling, "worker", 2, "asleep", "session_name", "shared"),
			).alive("shared", InventoryAttrs{OwnerState: OwnerSession, OwnerID: owner})
			f.in.CitySuspended = suspended
			f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "shared"}}
			f.in.Demand.AssignedStoreRefs = []string{""}
			d := f.decideSelecting(owner)
			if e := entryOf(t, d, owner); !suspended && (e.Desired != desireWake || e.Reason != "assigned-work" || e.AssignedWork == nil) {
				t.Errorf("owner %s = %s/%s, want an assigned-work wake", owner, e.Desired, e.Reason)
			}
			if e := entryOf(t, d, sibling); e.Desired != desireNone || e.Reason != reasonNameOccupied || e.InDesired || e.AssignedWork != nil || e.Liveness != livenessOccupied {
				t.Errorf("suspended=%v occupied row %s = %s/%s in-desired=%v %+v, want none name-occupied, no assigned work", suspended, sibling, e.Desired, e.Reason, e.InDesired, e.AssignedWork)
			}
		}
	}
}

// Kills: C11 skipped for a configured named row (M14). The named session's
// only canonical row has its runtime name held by a closed bead's runtime:
// it is None(name-occupied), and no named plan replaces it.
func TestAllocator_OccupiedNamedRowIsNone(t *testing.T) {
	d := newAllocFixture(t, chatCity("always")).sessions(chatRow("gc-1", "3", "session_name", "chat", "state", "asleep")).
		alive("chat", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-0"}).decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonNameOccupied || e.InDesired {
		t.Fatalf("occupied named row = %s/%s in-desired=%v, want none name-occupied", e.Desired, e.Reason, e.InDesired)
	}
	if hasNamedPlan(d) {
		t.Fatalf("plans %+v, want no named plan for an occupied identity", d.Plans)
	}
}

// Kills: a slot or a start for an unknown-state row (F9): it is None, and
// keeps its place in occupancy.
func TestAllocator_UnknownStateRowNoneButOccupiesSlot(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	d := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "gc_swept")).decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonUnknownState || e.InDesired {
		t.Fatalf("unknown-state row = %s/%s indesired=%v", e.Desired, e.Reason, e.InDesired)
	}
}

// Kills: rolling back a row legacy would not (F8, SESS-501, MAINT-034): a
// pending create past its lease is a rollback candidate only when its own
// runtime is not running (absent, or the name another row holds) and its
// endpoint does not hold it; one within its lease, alive, dead or of
// unknown liveness stays managed. A creating row with no claim past the
// stale window is one too, unless its start is in flight.
func TestAllocator_RollbackCandidatesRequireNotRunning(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 9)}, Workspace: config.Workspace{Provider: "claude"}}
	expired := func(id string, slot int) beads.Bead {
		return poolRow(id, "worker", slot, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(30*time.Minute))
	}
	f := newAllocFixture(t, cfg).sessions(
		expired("gc-absent", 1),
		expired("gc-alive", 2),
		expired("gc-dead", 3),
		poolRow("gc-lease", "worker", 4, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(time.Minute)),
		poolRow("gc-stale", "worker", 5, "creating"),
		poolRow("gc-starting", "worker", 6, "creating", "last_woke_at", ago(10*time.Second)),
		expired("gc-shared", 7),
		poolRow("gc-owner", "worker", 8, "active", "session_name", "s-gc-shared"),
	).alive("s-gc-alive", InventoryAttrs{}).corpse("s-gc-dead").
		alive("s-gc-shared", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-owner"})
	f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
	d := f.decide()
	for id, want := range map[string]bool{
		"gc-absent": true, "gc-alive": false, "gc-dead": false, "gc-lease": false,
		"gc-stale": true, "gc-starting": false, "gc-shared": true,
	} {
		e := entryOf(t, d, id)
		if got := e.Desired == desireNone && e.Reason == reasonRollbackCandidate; got != want {
			t.Errorf("%s (%s) = %s/%s, rollback candidate %v, want %v", id, e.Liveness, e.Desired, e.Reason, got, want)
		}
	}
	unknown := newAllocFixture(t, cfg).sessions(expired("gc-1", 1))
	unknown.noInventory = true
	if e := entryOf(t, unknown.decide(), "gc-1"); e.Reason == reasonRollbackCandidate {
		t.Errorf("a pending create of unknown liveness rolled back: %s/%s", e.Desired, e.Reason)
	}
	held := newAllocFixture(t, cfg).sessions(expired("gc-1", 1))
	held.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed, HoldsPendingCreate: true}}
	if e := entryOf(t, held.decide(), "gc-1"); e.Reason == reasonRollbackCandidate {
		t.Errorf("a pending create the breaker holds rolled back: %s/%s", e.Desired, e.Reason)
	}
}

// Kills: a rollback that fails open when the pass has no view of the row's
// guarded endpoint (F6): an absent, non-empty endpoint key holds the
// pending create; only a view that does not hold it frees it, and a row on
// no endpoint is unguarded.
func TestAllocator_MissingEndpointViewHoldsPendingCreate(t *testing.T) {
	expired := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(30*time.Minute))
	guarded := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}, Workspace: config.Workspace{Provider: "claude"}}
	unguarded := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	for _, tc := range []struct {
		label     string
		cfg       *config.City
		endpoints map[endpointKey]endpointView
		rollback  bool
	}{
		{"no view", guarded, nil, false},
		{"other endpoint's view only", guarded, map[endpointKey]endpointView{"provider:codex": {Gate: gateClosed}}, false},
		{"view does not hold", guarded, map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}, true},
		{"no endpoint", unguarded, nil, true},
	} {
		f := newAllocFixture(t, tc.cfg).sessions(expired)
		f.in.Endpoints = tc.endpoints
		e := entryOf(t, f.decide(), "gc-1")
		if got := e.Desired == desireNone && e.Reason == reasonRollbackCandidate; got != tc.rollback {
			t.Errorf("%s: %s/%s, rollback candidate %v, want %v", tc.label, e.Desired, e.Reason, got, tc.rollback)
		}
	}
}

// Kills: a suspended city draining a pending create legacy's suspend drain
// leaves alone (F4; session_reconciler.go:2252 keeps whatever
// pendingCreateSessionStillLeasedInfo leases): an explicit start request,
// a young creating row, a claim whose attempt is recent though its start is
// no longer in flight, and a claim left on an alive active row. A claim
// past its lease drains, as legacy's does.
func TestAllocator_SuspendedPendingCreateMatchesLegacyLease(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 9)}}
	created := func(b beads.Bead, at time.Time) beads.Bead { b.CreatedAt = at; return b }
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-sp", "worker", 1, "start-pending"),
		created(poolRow("gc-cr", "worker", 2, "creating"), allocNow.Add(-20*time.Second)),
		poolRow("gc-att", "worker", 3, "creating", "pending_create_claim", "true",
			"last_woke_at", ago(3*time.Minute), "pending_create_started_at", ago(30*time.Second)),
		poolRow("gc-act", "worker", 4, "active", "pending_create_claim", "true", "pending_create_started_at", ago(2*time.Minute)),
		poolRow("gc-old", "worker", 5, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(30*time.Minute)),
	).alive("s-gc-act", InventoryAttrs{AttachedKnown: true}).alive("s-gc-old", InventoryAttrs{AttachedKnown: true})
	f.in.CitySuspended = true
	in := f.inputs()
	d := mustDecide(t, in)
	clk := &clock.Fake{Time: allocNow}
	for id, keeps := range map[string]bool{"gc-sp": true, "gc-cr": true, "gc-att": true, "gc-act": true, "gc-old": false} {
		k := rowKey{Leg: allocSessionsLeg, ID: id}
		if legacy := pendingCreateSessionStillLeasedInfo(in.Census.Rows[k].Info, cfg, clk); legacy != keeps {
			t.Fatalf("%s: legacy keeps %v, fixture expects %v", id, legacy, keeps)
		}
		e := d.Snapshot.Entries[k]
		want := desireDrain
		if keeps {
			want = desireKeep
		}
		if e.Desired != want || (keeps && e.Reason != reasonPendingCreate) {
			t.Errorf("%s (%s) = %s/%s, want %s as legacy", id, e.Liveness, e.Desired, e.Reason, want)
		}
	}
}

// R-43: 43 stale creates share one runtime name with a live owner. Each
// gets its own entry; the stale ones are rollback candidates; the owner
// keeps its decision.
func TestAllocator_FortyThreeRowsShareANameStaleOnesRollBack(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	rows := []beads.Bead{poolRow("gc-owner", "worker", 2, "active", "session_name", "worker--2-pool")}
	for i := 0; i < 43; i++ {
		rows = append(rows, poolRow(fmt.Sprintf("gc-stale-%02d", i), "worker", 2, "creating", "session_name", "worker--2-pool",
			"pending_create_claim", "true", "pending_create_started_at", ago(time.Hour)))
	}
	d := newAllocFixture(t, cfg).sessions(rows...).
		alive("worker--2-pool", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-owner", AttachedKnown: true}).
		decideSelecting("gc-owner")
	if len(d.Snapshot.Entries) != 44 {
		t.Fatalf("entries = %d, want 44", len(d.Snapshot.Entries))
	}
	for i := 0; i < 43; i++ {
		if e := entryOf(t, d, fmt.Sprintf("gc-stale-%02d", i)); e.Desired != desireNone || e.Reason != reasonRollbackCandidate {
			t.Fatalf("stale row %d = %s/%s", i, e.Desired, e.Reason)
		}
	}
	if e := entryOf(t, d, "gc-owner"); !e.InDesired || e.Liveness != livenessAlive || e.Desired == desireNone {
		t.Fatalf("live owner = %s (%s), want its own decision", e.Desired, e.Liveness)
	}
}

// Kills: starting both duplicates, and retiring under a partial read
// (C2.13). The loser is None with a verdict.
func TestAllocator_IdentityVerdictsLosersNoneNoneUnderPartial(t *testing.T) {
	rows := []beads.Bead{chatRow("gc-1", "1"), chatRow("gc-2", "3")}
	d := newAllocFixture(t, chatCity("always")).sessions(rows...).decide()
	v := d.Snapshot.Identities["chat"]
	if v.Canonical == nil || v.Canonical.ID != "gc-2" || len(v.Losers) != 1 || v.Losers[0].ID != "gc-1" || v.VerdictID == "" {
		t.Fatalf("verdict = %+v, want gc-2 over gc-1", v)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonIdentityLoser || !e.Identity.Loser {
		t.Fatalf("loser = %s/%s %+v", e.Desired, e.Reason, e.Identity)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired == desireNone || !e.Identity.Canonical {
		t.Fatalf("winner = %s %+v, want the canonical row managed", e.Desired, e.Identity)
	}
	for label, setup := range map[string]func(*allocFixture){
		"store-partial": func(f *allocFixture) { f.in.Demand.StorePartial = true },
	} {
		f := newAllocFixture(t, chatCity("always")).sessions(rows...)
		setup(f)
		partial := f.decide()
		if v := partial.Snapshot.Identities["chat"]; len(v.Losers) != 0 || v.Canonical == nil || v.Canonical.ID != "gc-2" {
			t.Errorf("%s: verdict %+v, want gc-2 canonical with no losers", label, v)
		}
		if e := entryOf(t, partial, "gc-1"); e.Desired == desireNone {
			t.Errorf("%s: a duplicate retired: %s/%s", label, e.Desired, e.Reason)
		}
	}
}

// Kills: count-based floors (C2.6). The floor is the min_active_sessions
// warm pool members with the lowest bead IDs; asleep, named, manual and
// dependency-only rows, and a row whose name another bead holds, never hold a
// floor rank.
func TestAllocator_FloorsLowestBeadIDWarmOnly(t *testing.T) {
	agent := allocPoolAgent("worker", 9)
	agent.MinActiveSessions = intPtr(2)
	cfg := &config.City{Agents: []config.Agent{agent}}
	d := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "asleep"),
		poolRow("gc-3", "worker", 3, "active"),
		poolRow("gc-2", "worker", 2, "active"),
		poolRow("gc-4", "worker", 4, "active"),
		poolRow("gc-0", "worker", 5, "active", "dependency_only", "true"),
		sessionRow("gc-00", "template", "worker", "state", "active", "session_name", "manual-0", "manual_session", "true"),
		poolRow("gc-000", "worker", 6, "active", "session_name", "shared"),
		poolRow("gc-9", "worker", 7, "active", "session_name", "shared"),
	).alive("shared", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-9"}).decide()
	got := d.Snapshot.Floors["worker"]
	if fmt.Sprint(got) != fmt.Sprint([]rowKey{{allocSessionsLeg, "gc-2"}, {allocSessionsLeg, "gc-3"}}) {
		t.Fatalf("floor = %v, want [gc-2 gc-3]", got)
	}
	if e := entryOf(t, d, "gc-2"); e.Floor == nil || e.Floor.Rank != 0 || e.Floor.MinActive != 2 {
		t.Fatalf("gc-2 floor view = %+v", e.Floor)
	}
	for _, id := range []string{"gc-4", "gc-1", "gc-0", "gc-00", "gc-000"} {
		if entryOf(t, d, id).Floor != nil {
			t.Errorf("%s holds a floor rank", id)
		}
	}
}

// Kills: a start on unknown liveness (C2.9, GUAR-053). With no inventory
// pass, an undesired row keeps (never drains) and is uncertain; a desired
// row carries unknown liveness, which is not a start candidate. Unknown
// liveness reads running, so a running on_demand session stays awake.
func TestAllocator_UncertainLivenessKeepsNoGrant(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3), allocPoolAgent("idle", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "idle", 1, "active"),
	)
	f.noInventory = true
	d := f.decideSelecting("gc-1")
	if e := entryOf(t, d, "gc-2"); e.Desired != desireKeep || !e.ObservationUncertain || e.Reason != reasonObservationUncertain {
		t.Fatalf("undesired row on unknown liveness = %s/%s uncertain=%v, want keep", e.Desired, e.Reason, e.ObservationUncertain)
	}
	if e := entryOf(t, d, "gc-1"); e.Liveness.startCandidate() || e.Liveness != livenessUnknown {
		t.Fatalf("desired row on unknown liveness: liveness %s must not be a start candidate", e.Liveness)
	}

	od := newAllocFixture(t, chatCity("on_demand")).sessions(chatRow("gc-1", "1", "configured_named_mode", "on_demand", "state", "active"))
	od.noInventory = true
	if e := entryOf(t, od.decideSelecting("gc-1"), "gc-1"); e.Desired != desireWake || e.Reason != "on-demand:running" {
		t.Fatalf("on_demand row of unknown liveness = %s/%s, want the running on_demand wake", e.Desired, e.Reason)
	}
}

// Kills: a pending interaction ignored (stage 3, POOL-074): a live row with
// a fresh pending fact wakes for it, and config sleep never suppresses it.
func TestAllocator_PendingInteractionWakes(t *testing.T) {
	f := newAllocFixture(t, chatCity("on_demand")).
		sessions(chatRow("gc-1", "1", "configured_named_mode", "on_demand", "state", "active", "detached_at", ago(time.Hour))).
		alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).fact("s-gc-1", FactPending, ObsYes)
	f.activity = map[string]time.Time{"s-gc-1": allocNow.Add(-time.Hour)}
	f.in.SleepPolicies = map[string]resolvedSessionSleepPolicy{"gc-1": {Effective: "1m", Duration: time.Minute, Fingerprint: "fp"}}
	e := entryOf(t, f.decideSelecting("gc-1"), "gc-1")
	if e.Desired != desireWake || e.Reason != "pending" || e.DemandClass != "human" {
		t.Fatalf("pending row = %s/%s class %s, want a human pending wake", e.Desired, e.Reason, e.DemandClass)
	}
}

// Kills: a start or wake on an attach the pass cannot read (stage 3a as
// amended by AM3, C2.9). An uncertain attach on a live runtime reads
// attached; a desired row whose only wake is that attach keeps instead of
// waking, and an undesired one keeps instead of draining.
func TestAllocator_UncertainAttachOnlyWakeKeeps(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3), allocPoolAgent("idle", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-0", "worker", 1, "active"),
		poolRow("gc-1", "worker", 2, "active"),
		poolRow("gc-2", "idle", 1, "active"),
	)
	// The first pass reports attach; the next lists every name but reports
	// attach for gc-0 only, so gc-1's and gc-2's attach facts age past
	// maxAge while their listings stay fresh: a stale attach on a reporter.
	cache := newObserveCache()
	attrs := map[string]InventoryAttrs{}
	for _, n := range []string{"s-gc-0", "s-gc-1", "s-gc-2"} {
		attrs[n] = InventoryAttrs{DeadKnown: true, AttachedKnown: true}
	}
	cache.publish(allocNow.Add(-50*time.Second), attrs, completeBackend("tmux", "s-gc-0", "s-gc-1", "s-gc-2"))
	in := f.inputs()
	in.Now = allocNow.Add(observeMaxAge - 40*time.Second)
	in.Obs = cache.publish(in.Now, map[string]InventoryAttrs{"s-gc-0": {DeadKnown: true, AttachedKnown: true, Attached: true}},
		completeBackend("tmux", "s-gc-0", "s-gc-1", "s-gc-2"))
	p := newDecidePass(in)
	p.prepare()
	for _, id := range []string{"gc-0", "gc-1"} {
		p.selected[p.byID[id]] = &selection{}
	}
	d := p.finish()
	if e := entryOf(t, d, "gc-0"); e.Desired != desireWake || e.ObservationUncertain {
		t.Fatalf("control row = %s/%s uncertain=%v, want a certain wake", e.Desired, e.Reason, e.ObservationUncertain)
	}
	if e := entryOf(t, d, "gc-1"); !e.InDesired || !e.ObservationUncertain || e.Desired != desireKeep || e.Reason != reasonObservationUncertain ||
		!slices.Equal(e.WakeReasons, []WakeReason{WakeAttached}) || e.DemandClass != "human" {
		t.Fatalf("desired row woken only by an uncertain attach = %s/%s indesired=%v wake=%v class=%s, want a keep read as attached",
			e.Desired, e.Reason, e.InDesired, e.WakeReasons, e.DemandClass)
	}
	if e := entryOf(t, d, "gc-2"); e.InDesired || e.Desired != desireKeep {
		t.Fatalf("undesired row with an uncertain attach = %s/%s, want keep, not drain", e.Desired, e.Reason)
	}
}

// Kills: live sessions slept on stale data, dead sessions woken against
// policy, and the override and exemption rules dropped (stage 6b, AM3,
// SESS-585/586/652). A row that is not alive measures idleness from
// detached_at; a live row is suppressed only with a known activity fact;
// assigned work outranks the policy; a pinned row is never suppressed.
func TestAllocator_ConfigSleepSuppressionDeadUsesDetachedAtLiveNeedsActivity(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "chat"}}}
	manual := func(id string, meta ...string) beads.Bead {
		return sessionRow(id, append([]string{
			"template", "chat", "state", "active", "session_name", "s-" + id,
			"manual_session", "true", "detached_at", ago(10 * time.Minute),
		}, meta...)...)
	}
	f := newAllocFixture(t, cfg).sessions(manual("gc-dead"), manual("gc-live"), manual("gc-busy"), manual("gc-idle"),
		manual("gc-fresh", "detached_at", ago(30*time.Second)), manual("gc-work"), manual("gc-pin", "pin_awake", "true")).
		alive("s-gc-live", InventoryAttrs{AttachedKnown: true}).
		alive("s-gc-busy", InventoryAttrs{AttachedKnown: true}).
		alive("s-gc-idle", InventoryAttrs{AttachedKnown: true})
	f.activity = map[string]time.Time{"s-gc-busy": allocNow.Add(-30 * time.Second), "s-gc-idle": allocNow.Add(-5 * time.Minute)}
	policy := resolvedSessionSleepPolicy{Effective: "1m", Duration: time.Minute, Fingerprint: "fp"}
	f.in.SleepPolicies = map[string]resolvedSessionSleepPolicy{}
	for _, id := range []string{"gc-dead", "gc-live", "gc-busy", "gc-idle", "gc-fresh", "gc-work", "gc-pin"} {
		f.in.SleepPolicies[id] = policy
	}
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "s-gc-work"}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	d := f.decideSelecting("gc-dead", "gc-live", "gc-busy", "gc-idle", "gc-fresh", "gc-work", "gc-pin")
	for id, suppressed := range map[string]bool{
		"gc-dead": true, "gc-live": false, "gc-busy": false, "gc-idle": true,
		"gc-fresh": false, "gc-work": false, "gc-pin": false,
	} {
		e := entryOf(t, d, id)
		if got := e.Desired == desireSleep && e.Reason == reasonConfigSleep; got != suppressed {
			t.Errorf("%s = %s/%s, config-sleep-suppressed %v, want %v", id, e.Desired, e.Reason, got, suppressed)
		}
		if !suppressed && e.Desired != desireWake {
			t.Errorf("%s = %s/%s, want a wake", id, e.Desired, e.Reason)
		}
	}
}

// Kills: a stale leg's rows shrunk (§4.2 rule 4): served past their bound,
// they keep, and the census is incomplete.
func TestAllocator_StaleLegRowsKeep(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	feed := &fakeCensusFeed{nonExact: map[beads.Store]bool{}, recordings: map[beads.Store]censusRecording{}}
	rig := censusStore()
	feed.nonExact[rig] = true
	feed.recordings[rig] = censusRecording{Rows: censusInfos(t, poolRow("rg-1", "worker", 2, "active")), At: allocNow.Add(-10 * time.Minute), Expires: allocNow.Add(-9 * time.Minute)}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "asleep"))
	f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: rig})
	in := f.in
	c, err := newCensusReader(feed.feed()).read(allocNow, cfg, f.legs)
	if err != nil {
		t.Fatal(err)
	}
	in.Census, in.Obs = c, f.observation()
	d := mustDecide(t, in)
	if !slices.Contains(d.Snapshot.Partial.Legs["rig:a"], causeLegStale) || d.Snapshot.Mode != modePartial ||
		!slices.Contains(d.Snapshot.Partial.Global, causeCensusIncomplete) {
		t.Fatalf("stale leg: mode %s partial %+v", d.Snapshot.Mode, d.Snapshot.Partial)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired != desireKeep || e.Reason != reasonPartialRetain {
		t.Fatalf("row under an incomplete census = %s/%s, want partial-retain keep", e.Desired, e.Reason)
	}
}

// Kills: retiring an identity loser under an incomplete census (C2.13,
// M22). A stale leg may hold the winner, so the verdict retires nothing.
func TestAllocator_StaleLegRetiresNoIdentityLoser(t *testing.T) {
	cfg := chatCity("always")
	feed := &fakeCensusFeed{nonExact: map[beads.Store]bool{}, recordings: map[beads.Store]censusRecording{}}
	rig := censusStore()
	feed.nonExact[rig] = true
	feed.recordings[rig] = censusRecording{At: allocNow.Add(-10 * time.Minute), Expires: allocNow.Add(-9 * time.Minute)}
	f := newAllocFixture(t, cfg).sessions(chatRow("gc-1", "1"), chatRow("gc-2", "3"))
	f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: rig})
	in := f.in
	c, err := newCensusReader(feed.feed()).read(allocNow, cfg, f.legs)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Incomplete() {
		t.Fatal("stale leg: census complete, want incomplete")
	}
	in.Census, in.Obs = c, f.observation()
	d := mustDecide(t, in)
	if v := d.Snapshot.Identities["chat"]; len(v.Losers) != 0 {
		t.Fatalf("stale leg: losers %v retired under an incomplete census", v.Losers)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired == desireNone {
		t.Fatalf("stale leg: a duplicate retired: %s/%s", e.Desired, e.Reason)
	}
}

// Kills: a missing census read as an empty city (§4.2 census errors): with
// no census the pass is partial and keeps.
func TestAllocator_NoCensusIsPartialNotEmpty(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	in := newAllocFixture(t, cfg).in
	d := mustDecide(t, in)
	if d.Snapshot.Mode != modePartial || !slices.Contains(d.Snapshot.Partial.Global, causeCensusIncomplete) {
		t.Fatalf("no census: mode %s partial %+v, want partial census-incomplete", d.Snapshot.Mode, d.Snapshot.Partial)
	}
}

// POOL-018: a store-query partial retains everything (Keep, not Drain or
// Sleep), and only that.
func TestAllocator_StoreQueryPartialRetains(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("idle", 3)}}
	run := func(partial bool) allocDecision {
		f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "idle", 1, "active"), poolRow("gc-2", "idle", 2, "active")).
			alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).alive("s-gc-2", InventoryAttrs{AttachedKnown: true})
		f.in.Demand.StorePartial = partial
		return f.decideSelecting("gc-2")
	}
	if e := entryOf(t, run(false), "gc-1"); e.Desired != desireDrain {
		t.Fatalf("control: undesired live row = %s, want drain", e.Desired)
	}
	d := run(true)
	if d.Snapshot.Mode != modePartial || !slices.Contains(d.Snapshot.Partial.Global, causeStoreQueryPartial) {
		t.Fatalf("mode %s partial %+v", d.Snapshot.Mode, d.Snapshot.Partial)
	}
	for _, id := range []string{"gc-1", "gc-2"} {
		if e := entryOf(t, d, id); e.Desired != desireKeep || e.Reason != reasonPartialRetain {
			t.Errorf("%s = %s/%s, want partial-retain keep", id, e.Desired, e.Reason)
		}
	}
}

// Kills: rows on a migrated duplicate leg counted or managed (C2.11).
func TestAllocator_DuplicatesNone(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("other", 3)}}
	row := poolRow("gc-1", "other", 1, "asleep")
	d := newAllocFixture(t, cfg).sessions(row).rigLeg(row).decide()
	dup := d.Snapshot.Entries[rowKey{"rig:a", "gc-1"}]
	if dup == nil || dup.Desired != desireNone || dup.Reason != reasonDuplicate || dup.Identity == nil || dup.Identity.DuplicateOf != allocSessionsLeg {
		t.Fatalf("duplicate copy = %+v", dup)
	}
	if e := d.Snapshot.Entries[rowKey{allocSessionsLeg, "gc-1"}]; e == nil || e.Desired == desireNone {
		t.Fatalf("canonical copy = %+v, want managed", e)
	}
}

// Kills: a row whose stored template is legacy's (empty template, the
// identity in agent_name) escaping its template's partial retention:
// Entry.Template resolves as legacy does (resolvedSessionTemplateInfo).
func TestAllocator_EntryTemplateResolvesLegacyRows(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		sessionRow("gc-2", "state", "active", "pool_managed", "true", "session_name", "s-gc-2",
			"agent_name", "worker-2", "pool_slot", "2", "generation", "1"),
	).alive("s-gc-2", InventoryAttrs{AttachedKnown: true})
	in := f.inputs()
	p := newDecidePass(in)
	p.prepare()
	p.markTemplate("worker", true, true, "pool-scale-check-partial")
	d := p.finish()
	if e := entryOf(t, d, "gc-2"); e.Template != "worker" || e.Desired != desireKeep {
		t.Fatalf("legacy-template row = template %q %s/%s, want worker, retained", e.Template, e.Desired, e.Reason)
	}
}

// Kills: a named scale-check partial retaining the template's other rows
// (POOL-037): it keeps only its named rows.
func TestAllocator_NamedScaleCheckPartialKeepsOnlyNamedRows(t *testing.T) {
	cfg := chatCity("always")
	f := newAllocFixture(t, cfg).sessions(
		chatRow("gc-1", "1", "state", "active"),
		sessionRow("gc-2", "template", "chat", "state", "active", "session_name", "s-gc-2"),
	).alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).alive("s-gc-2", InventoryAttrs{AttachedKnown: true})
	p := newDecidePass(f.inputs())
	p.prepare()
	p.markTemplate("chat", false, false, "named-scale-check-partial")
	d := p.finish()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireKeep || e.Reason != reasonPartialRetain {
		t.Errorf("named row = %s/%s, want partial-retain keep", e.Desired, e.Reason)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired != desireDrain {
		t.Errorf("plain row of the template = %s/%s, want drain", e.Desired, e.Reason)
	}
}

// Kills: an undesired row of a suspended agent drained as orphaned (SESS-074).
func TestAllocator_UndesiredSuspendedAgentDrainsAsSuspended(t *testing.T) {
	agent := allocPoolAgent("worker", 3)
	agent.Suspended = true
	cfg := &config.City{Agents: []config.Agent{agent, allocPoolAgent("other", 3)}}
	d := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "other", 1, "active")).
		alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).alive("s-gc-2", InventoryAttrs{AttachedKnown: true}).decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireDrain || e.DrainReason != drainSuspended {
		t.Errorf("suspended agent's row = %s/%s, want drain suspended", e.Desired, e.DrainReason)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired != desireDrain || e.DrainReason != drainOrphaned {
		t.Errorf("unselected row = %s/%s, want drain orphaned", e.Desired, e.DrainReason)
	}
}

// Kills: an edit to anything the pass is handed (P3-2 obligation: copy a
// row before editing it). Two identical inputs are built; the pass runs on
// one, and the two must still be deeply equal, census internals, rows,
// metadata and the observation included.
func TestAllocator_DecideNeverEditsItsInputs(t *testing.T) {
	build := func() allocInputs {
		in := purityInputs(t)
		in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "s-gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
		in.Demand.AssignedStoreRefs = []string{""}
		in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{{ID: "w-1"}: true}
		return in
	}
	in, control := build(), build()
	mustDecide(t, in)
	if !reflect.DeepEqual(in, control) {
		t.Fatal("the decide edited its inputs")
	}
}

// TestAllocator_DecideRefusesAZeroClock pins the one-clock rule (#35): a
// pass with no Now would read every lease as expired.
func TestAllocator_DecideRefusesAZeroClock(t *testing.T) {
	in := newAllocFixture(t, &config.City{}).inputs()
	in.Now = time.Time{}
	if _, err := decideAllocation(in); !errors.Is(err, errDecideNoClock) {
		t.Fatalf("err = %v, want errDecideNoClock", err)
	}
}
