package main

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// assign gives each row open claimed work assigned to its session name, so
// the full pass selects and wakes it (assigned-work).
func (f *allocFixture) assign(ids ...string) *allocFixture {
	for _, id := range ids {
		f.in.Demand.AssignedWork = append(f.in.Demand.AssignedWork, beads.Bead{ID: "w-" + id, Status: "in_progress", Assignee: "s-" + id})
		f.in.Demand.AssignedStoreRefs = append(f.in.Demand.AssignedStoreRefs, "")
	}
	return f
}

// tokens sets the bucket to n tokens, refilled at Now.
func (f *allocFixture) tokens(n int) *allocFixture {
	f.in.Bucket = bucketState{Tokens: n, LastRefill: f.in.Now}
	return f
}

func grantCity(maxWakes int, agents ...config.Agent) *config.City {
	if len(agents) == 0 {
		agents = []config.Agent{allocPoolAgent("worker", 10)}
	}
	return &config.City{Agents: agents, Daemon: config.DaemonConfig{MaxWakesPerTick: intPtr(maxWakes), PatrolInterval: "1h"}}
}

func sessKey(id string) rowKey { return rowKey{Leg: allocSessionsLeg, ID: id} }

// granted is the rows holding a grant in d, sorted.
func granted(d allocDecision) []string {
	var out []string
	for k, e := range d.Snapshot.Entries {
		if e.Start != nil && e.Start.Grant != "" {
			out = append(out, k.ID)
		}
	}
	slices.Sort(out)
	return out
}

func opsOf(d allocDecision, kind ledgerOpKind) []ledgerOp {
	var out []ledgerOp
	for _, op := range d.LedgerOps {
		if op.Kind == kind {
			out = append(out, op)
		}
	}
	return out
}

func grantEntry(id, row string, st ledgerState) ledgerEntry {
	return ledgerEntry{ID: id, Kind: kindGrant, Key: sessKey(row), State: st, ReservedAt: allocNow, ConfigRev: "rev-1"}
}

// asleepRows are worker rows at slots 1.., asleep and not listed (start
// candidates), each with assigned work.
func asleepRows(ids ...string) []beads.Bead {
	var out []beads.Bead
	for i, id := range ids {
		out = append(out, poolRow(id, "worker", i+1, "asleep"))
	}
	return out
}

// Kills (START-009, C2.7 erratum 2026-10-05): an order that is not legacy's
// wakeFairnessTime (a never-woken row ranks by its creation, not first), an
// order that is not LRU, and ties left to map order.
func TestGrants_LRUByWakeFairnessThenTies(t *testing.T) {
	f := newAllocFixture(t, grantCity(5)).sessions(
		poolRow("gc-3", "worker", 3, "asleep", "last_woke_at", ago(30*time.Minute)),
		poolRow("gc-1", "worker", 1, "asleep", "last_woke_at", ago(3*time.Hour)),
		poolRow("gc-4", "worker", 4, "asleep"),
		poolRow("gc-2", "worker", 2, "asleep"),
	).assign("gc-1", "gc-2", "gc-3", "gc-4").tokens(2)
	d := f.decide()
	var order []string
	for _, id := range []string{"gc-1", "gc-2", "gc-3", "gc-4"} {
		e := entryOf(t, d, id)
		if e.Start == nil {
			t.Fatalf("%s is not ranked: %s/%s", id, e.Desired, e.Reason)
		}
		order = append(order, fmt.Sprintf("%s:%d", id, e.Start.Rank))
	}
	// Never-woken rows wait from their creation (an hour ago), as legacy's
	// wakeFairnessTime reads them: behind gc-1, woken 3h ago, ahead of gc-3.
	if got := fmt.Sprint(order); got != "[gc-1:0 gc-2:1 gc-3:3 gc-4:2]" {
		t.Fatalf("ranks %s, want gc-1, then gc-2 and gc-4 by bead ID, then gc-3", got)
	}
	if got := fmt.Sprint(granted(d)); got != "[gc-1 gc-2]" {
		t.Fatalf("granted %s, want the two least recently woken", got)
	}
	if e := entryOf(t, d, "gc-4"); e.Reason != reasonAwaitingBudget {
		t.Fatalf("gc-4 reason %q, want %s", e.Reason, reasonAwaitingBudget)
	}
}

// Kills (R20): a re-debit or a grant ID churn per pass, and a newly
// higher-ranked candidate pre-empting a reserved grant.
func TestGrants_StickyAcrossPassesSingleDebit(t *testing.T) {
	f := newAllocFixture(t, grantCity(1)).sessions(
		poolRow("gc-1", "worker", 1, "asleep", "last_woke_at", ago(time.Hour)),
	).assign("gc-1").tokens(3)
	first := f.decide()
	reserves := opsOf(first, opReserve)
	if len(reserves) != 1 || first.Bucket.Tokens != 0 {
		t.Fatalf("pass 1: reserves %+v tokens %d, want one grant debited", reserves, first.Bucket.Tokens)
	}
	id := reserves[0].ID
	f.in.Ledger = []ledgerEntry{func() ledgerEntry { e := reserves[0].Entry; e.State = ledgerReserved; return e }()}
	f.in.Bucket, f.in.EntrySeq, f.in.Prev = first.Bucket, first.EntrySeq, first.Snapshot
	f.in.Now = allocNow.Add(time.Second)
	f.legs = nil
	f.sessions(
		poolRow("gc-1", "worker", 1, "asleep", "last_woke_at", ago(time.Hour)),
		poolRow("gc-0", "worker", 2, "asleep"),
	).assign("gc-0")
	second := f.decide()
	if len(second.LedgerOps) != 0 || second.Bucket.Tokens != first.Bucket.Tokens {
		t.Fatalf("pass 2: ops %+v tokens %d, want no op and no debit", second.LedgerOps, second.Bucket.Tokens)
	}
	if e := entryOf(t, second, "gc-1"); e.Start == nil || e.Start.Grant != id {
		t.Fatalf("gc-1 start %+v, want grant %s republished", e.Start, id)
	}
	if e := entryOf(t, second, "gc-0"); e.Start == nil || e.Start.Rank != 0 || e.Start.Grant != "" {
		t.Fatalf("gc-0 start %+v, want rank 0 and no grant: the cap is held", e.Start)
	}
}

// Kills (C5.4(4)): a grant held forever, a start against stale config, a
// grant kept by a row that is no longer a candidate, an issued grant
// released, an expired or stale-config create left holding its slot, an
// issued create released, and a released create dropping its reservation
// (its release may lose to an issue).
func TestGrants_ReleaseOnTTLIneligibleDeselectedStaleConfigRev(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *allocFixture, g *ledgerEntry)
		want  string // release reason, or "" for republished
	}{
		{"fresh", func(*allocFixture, *ledgerEntry) {}, ""},
		{"ttl", func(_ *allocFixture, g *ledgerEntry) { g.ReservedAt = allocNow.Add(-ledgerReserveTTL) }, releaseTTL},
		{"stale-config", func(_ *allocFixture, g *ledgerEntry) { g.ConfigRev = "rev-0" }, releaseStaleConfig},
		{"ineligible", func(f *allocFixture, _ *ledgerEntry) {
			f.in.Backoff = map[string]backoffRecord{rowBackoffKey(sessKey("gc-1")): {Until: allocNow.Add(time.Minute)}}
		}, "ineligible:" + gateBackoff},
		{"deselected", func(f *allocFixture, _ *ledgerEntry) { f.in.Demand = demandView{} }, releaseDeselected},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1")...).assign("gc-1").tokens(5)
		g := grantEntry("g-1", "gc-1", ledgerReserved)
		tc.setup(f, &g)
		f.in.Ledger = []ledgerEntry{g, grantEntry("g-2", "gc-2", ledgerIssued)}
		d := f.decide()
		releases := opsOf(d, opRelease)
		switch {
		case tc.want == "" && (len(releases) != 0 || entryOf(t, d, "gc-1").Start.Grant != "g-1"):
			t.Errorf("%s: releases %+v, want g-1 republished", tc.name, releases)
		case tc.want != "" && (len(releases) != 1 || releases[0].ID != "g-1" || releases[0].Reason != tc.want):
			t.Errorf("%s: releases %+v, want g-1 released for %s (never the issued g-2)", tc.name, releases, tc.want)
		}
		if tc.want != "" && len(opsOf(d, opReserve)) != 0 {
			t.Errorf("%s: the released key was granted again in the same pass: %+v", tc.name, opsOf(d, opReserve))
		}
	}

	for _, tc := range []struct {
		name  string
		setup func(c *ledgerEntry)
		want  string
	}{
		{"fresh create", func(*ledgerEntry) {}, ""},
		{"expired create", func(c *ledgerEntry) { c.ReservedAt = allocNow.Add(-ledgerReserveTTL) }, releaseTTL},
		{"stale-config create", func(c *ledgerEntry) { c.ConfigRev = "rev-0" }, releaseStaleConfig},
		{"issued stale-config create", func(c *ledgerEntry) { c.ConfigRev, c.State = "rev-0", ledgerIssued }, ""},
	} {
		f := newAllocFixture(t, grantCity(5)).tokens(5)
		c := ledgerEntry{
			ID: "c-1", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, State: ledgerReserved,
			ReservedAt: allocNow, ConfigRev: "rev-1", Marker: ledgerMarker{InstanceToken: "tok"},
		}
		tc.setup(&c)
		f.in.Ledger = []ledgerEntry{c}
		f.in.Reservations = []planReservation{{EntryID: "c-1", Template: "worker", QualifiedInstance: "worker-1", Slot: 1}}
		d := f.decide()
		r := opsOf(d, opRelease)
		if (tc.want == "" && len(r) != 0) || (tc.want != "" && (len(r) != 1 || r[0].ID != "c-1" || r[0].Reason != tc.want)) {
			t.Errorf("%s: releases %+v, want %q", tc.name, r, tc.want)
		}
		if len(d.Reservations) != 1 {
			t.Errorf("%s: reservations %+v, want c-1's kept until the ledger drops the entry", tc.name, d.Reservations)
		}
	}
}

// Kills (finding 2): a released create dropping its reservation, so that a
// release lost to IssueCreate plans the work again while the create is in
// flight; and a reservation kept after the release won.
func TestCreates_ReleasedCreateKeepsReservationUntilLedgerDropsIt(t *testing.T) {
	for _, issueWon := range []bool{true, false} {
		f := newAllocFixture(t, grantCity(5)).demand("worker", "w-1").tokens(5)
		first := f.decide()
		if len(first.Creates) != 1 {
			t.Fatalf("pass 1 creates %+v, want 1", first.Creates)
		}
		applyReserves(f, first)
		f.in.Now = allocNow.Add(ledgerReserveTTL)
		second := f.decide()
		if r := opsOf(second, opRelease); len(r) != 1 || len(second.Reservations) != 1 || len(second.Plans) != 0 {
			t.Fatalf("pass 2: releases %+v reservations %d plans %d, want the TTL release and the reservation kept", r, len(second.Reservations), len(second.Plans))
		}
		if issueWon {
			f.in.Ledger[0].State = ledgerIssued
		} else {
			f.in.Ledger = nil
		}
		f.in.Reservations, f.in.Bucket, f.in.EntrySeq, f.in.Prev = second.Reservations, second.Bucket, second.EntrySeq, second.Snapshot
		f.in.Now = f.in.Now.Add(time.Second)
		third := f.decide()
		if issueWon && (len(third.Plans) != 0 || len(third.Creates) != 0 || len(third.Reservations) != 1) {
			t.Errorf("issue won: plans %d creates %d reservations %d, want the work held by the create in flight", len(third.Plans), len(third.Creates), len(third.Reservations))
		}
		if issueWon {
			continue
		}
		// The release won: housekeeping drops the reservation, and the next
		// pass, planning without it, creates again.
		if len(third.Reservations) != 0 {
			t.Errorf("release won: reservations %+v, want the old one dropped", third.Reservations)
		}
		f.in.Reservations, f.in.EntrySeq = third.Reservations, third.EntrySeq
		if fourth := f.decide(); len(fourth.Creates) != 1 {
			t.Errorf("release won: pass 4 creates %+v, want the work planned again", fourth.Creates)
		}
	}
}

// Kills (target 5): a grant through an open endpoint, and a probe herd: a
// half-open endpoint admits exactly one outstanding grant.
func TestGrants_EndpointOpenNoneHalfOpenExactlyOneProbe(t *testing.T) {
	cfg := grantCity(10)
	cfg.Workspace.Provider = "claude"
	for gate, want := range map[endpointGate]int{gateShut: 0, gateProbe: 1, gateClosed: 3} {
		for _, busy := range []bool{false, true} {
			f := newAllocFixture(t, cfg).sessions(asleepRows("gc-1", "gc-2", "gc-3")...).assign("gc-1", "gc-2", "gc-3").tokens(10)
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gate}}
			n := want
			if busy {
				f.in.Ledger = []ledgerEntry{grantEntry("g-x", "gc-x", ledgerIssued)}
				f.in.Ledger[0].Endpoint = "provider:claude"
				if gate == gateProbe {
					n = 0
				}
			}
			if got := len(granted(allocDecisionOf(t, f))); got != n {
				t.Errorf("gate %d busy=%v: %d grants, want %d", gate, busy, got, n)
			}
		}
	}
}

func allocDecisionOf(t *testing.T, f *allocFixture) allocDecision {
	t.Helper()
	d := f.decide()
	if f.in.Endpoints["provider:claude"].Gate == gateShut {
		for _, id := range []string{"gc-1", "gc-2", "gc-3"} {
			if e := entryOf(t, d, id); e.Reason != "ineligible:"+gateEndpointShut {
				t.Errorf("%s reason %q under an open endpoint", id, e.Reason)
			}
		}
	}
	return d
}

// Kills: a poisoned row holding the probe: within an endpoint's slots the
// fewest refusals go first.
func TestGrants_ProbeRotationFewestRefusalsFirst(t *testing.T) {
	cfg := grantCity(10)
	cfg.Workspace.Provider = "claude"
	f := newAllocFixture(t, cfg).sessions(asleepRows("gc-1", "gc-2", "gc-3")...).assign("gc-1", "gc-2", "gc-3").tokens(10)
	f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateProbe, Refusals: map[string]int{"gc-1": 3, "gc-2": 1}}}
	if got := fmt.Sprint(granted(f.decide())); got != "[gc-3]" {
		t.Fatalf("probe granted to %s, want gc-3 (no refusals)", got)
	}
}

// Kills (C5.13, P3-4 obligation): a create and the row it produced counted
// twice, and a grant this pass releases not counted (its release may lose
// to an issue).
func TestGrants_CityInFlightOncePerEffectOnPreReleaseView(t *testing.T) {
	f := newAllocFixture(t, grantCity(3)).sessions(append(asleepRows("gc-1", "gc-2", "gc-3"),
		poolRow("gc-9", "worker", 9, "start-pending", "pending_create_claim", "true",
			"pending_create_started_at", ago(5*time.Second), "instance_token", "tok-9"))...,
	).assign("gc-2", "gc-3").tokens(5)
	expired := grantEntry("g-1", "gc-1", ledgerReserved)
	expired.ReservedAt = allocNow.Add(-ledgerReserveTTL)
	create := ledgerEntry{
		ID: "c-9", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, State: ledgerIssued,
		ReservedAt: allocNow, Marker: ledgerMarker{InstanceToken: "tok-9"},
	}
	f.in.Ledger = []ledgerEntry{create, expired}
	d := f.decide()
	if r := opsOf(d, opRelease); len(r) != 1 || r[0].ID != "g-1" {
		t.Fatalf("releases %+v, want g-1", r)
	}
	if got := granted(d); len(got) != 1 {
		t.Fatalf("granted %v, want exactly one: g-1 and c-9/gc-9 hold two of three slots", got)
	}
}

// pendingRows are n never-started pending creates of template, each bound
// to its trigger work w-1..w-n, so demand for that work keeps them desired.
func pendingRows(n int, template string) ([]beads.Bead, []string) {
	var rows []beads.Bead
	var work []string
	for i := 1; i <= n; i++ {
		w := fmt.Sprintf("w-%d", i)
		rows = append(rows, poolRow(fmt.Sprintf("%s-p%d", template, i), template, i, "start-pending", "pending_create_claim", "true",
			"pending_create_started_at", ago(5*time.Second), "gc.trigger_bead_id", w, "gc.trigger_bead_store_ref", "city"))
		work = append(work, w)
	}
	return rows, work
}

// Kills (F5, R-park, C5.13): rows parked behind one endpoint starving
// another, shut or half-open; a half-open backlog taking more than the
// probe; and parked rows blocking their own grants, so a closed endpoint's
// backlog never starts: each grant continues the slot its row holds.
func TestGrants_ParkedRowsDoNotStarveOtherEndpoints_HalfOpenCountsOne(t *testing.T) {
	cfg := grantCity(6, config.Agent{Name: "a", Provider: "pa", MaxActiveSessions: intPtr(10)},
		config.Agent{Name: "b", Provider: "pb", MaxActiveSessions: intPtr(10)})
	rows, work := pendingRows(5, "a")
	rows = append(rows, poolRow("gc-b1", "b", 1, "asleep"), poolRow("gc-b2", "b", 2, "asleep"))
	for gate, want := range map[endpointGate][2]int{gateShut: {0, 2}, gateProbe: {1, 2}, gateClosed: {5, 1}} {
		f := newAllocFixture(t, cfg).sessions(rows...).demand("a", work...).assign("gc-b1", "gc-b2").tokens(10)
		f.in.Endpoints = map[endpointKey]endpointView{
			"provider:pa": {Gate: gate, HoldsPendingCreate: true}, "provider:pb": {Gate: gateClosed},
		}
		d := f.decide()
		var a, b int
		for _, id := range granted(d) {
			if strings.HasPrefix(id, "a-") {
				a++
			} else {
				b++
			}
		}
		if a != want[0] || b != want[1] {
			t.Errorf("gate %d: %d grants on pa, %d on pb, want %d and %d", gate, a, b, want[0], want[1])
		}
	}
}

// Kills (F5, C5.13): the probe slot held by only one of a half-open
// endpoint's parked rows, so at the city cap the probe goes to that row
// rather than the one probe rotation picks. Every sibling continues the
// one slot.
func TestGrants_HalfOpenProbeAtCityCapContinuesTheParkedSlot(t *testing.T) {
	cfg := grantCity(1, config.Agent{Name: "a", Provider: "pa", MaxActiveSessions: intPtr(10)})
	rows, work := pendingRows(5, "a")
	refusals := map[string]int{"a-p1": 2, "a-p2": 2, "a-p3": 2, "a-p5": 2}
	for i := 0; i < 10; i++ {
		f := newAllocFixture(t, cfg).sessions(rows...).demand("a", work...).tokens(5)
		f.in.Endpoints = map[endpointKey]endpointView{"provider:pa": {Gate: gateProbe, HoldsPendingCreate: true, Refusals: refusals}}
		if got := fmt.Sprint(granted(f.decide())); got != "[a-p4]" {
			t.Fatalf("run %d: granted %s, want the probe on a-p4 (no refusals)", i, got)
		}
	}
}

// Kills (C2.2, C5.10): silent starvation: an ineligible or unfunded Wake
// entry publishes its cause and takes no grant.
func TestGrants_IneligibleReasonPublished(t *testing.T) {
	cases := []struct {
		cause string
		setup func(f *allocFixture)
	}{
		{gateEndpointShut, func(f *allocFixture) {
			f.in.Cfg.Workspace.Provider = "claude"
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateShut}}
		}},
		{gateProviderRed, func(f *allocFixture) {
			f.in.Cfg.Workspace.Provider = "claude"
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
			f.in.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{"claude": false}}
		}},
		{gateQuarantine, func(f *allocFixture) {
			info := session.Info{ID: "gc-1", Template: "worker", PoolManaged: true, AgentName: "worker-1", SessionNameMetadata: "s-gc-1"}
			q := session.StartupHealthEpisode{QuarantinedUntil: allocNow.Add(time.Minute)}
			f.in.Episodes = map[string]session.StartupHealthEpisode{"s-gc-1": q, startupHealthEpisodeKey(info, "s-gc-1"): q}
		}},
		{gateBackoff, func(f *allocFixture) {
			f.in.Backoff = map[string]backoffRecord{rowBackoffKey(sessKey("gc-1")): {Until: allocNow.Add(time.Minute)}}
		}},
		{gateInFlight, func(f *allocFixture) {
			g := grantEntry("g-1", "gc-1", ledgerCommitted)
			g.WroteRow, g.SettledAt, g.Marker.Incarnation = true, allocNow, 9
			f.in.Ledger = []ledgerEntry{g}
		}},
		{gateLivenessUnknown, func(f *allocFixture) { f.noInventory = true }},
		{reasonAwaitingBudget, func(f *allocFixture) { f.tokens(0) }},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1")...).assign("gc-1").tokens(5)
		tc.setup(f)
		e := entryOf(t, f.decide(), "gc-1")
		want := "ineligible:" + tc.cause
		if tc.cause == reasonAwaitingBudget {
			want = tc.cause
		}
		if e.Desired != desireWake || e.Reason != want || (e.Start != nil && e.Start.Grant != "") {
			t.Errorf("%s: entry %s/%s start %+v, want wake %s with no grant", tc.cause, e.Desired, e.Reason, e.Start, want)
		}
	}
}

// Kills (R21): one refuser absorbing the budget: a row with a live backoff
// is skipped, and the next candidate takes the one slot.
func TestGrants_PersistentBackoffStarvesNoOne(t *testing.T) {
	f := newAllocFixture(t, grantCity(1)).sessions(asleepRows("gc-1", "gc-2")...).assign("gc-1", "gc-2").tokens(5)
	f.in.Backoff = map[string]backoffRecord{rowBackoffKey(sessKey("gc-1")): {Until: allocNow.Add(5 * time.Minute)}}
	if got := fmt.Sprint(granted(f.decide())); got != "[gc-2]" {
		t.Fatalf("granted %s, want gc-2", got)
	}
}

// Kills (C2.12, C8, START-006): a start before its dependency is alive, a
// dependency counted alive while its start is in flight (any uncleared
// entry naming it, its own issued grant included, or a census start
// lease), a suspended dependency skipped (R7), a member outside the desired
// set counted (R31), and dependency waves: a dependent waits ineligible, it
// is not ordered behind its dependency.
func TestGrants_DepsUnsatisfiedIneligibleNoOrdering(t *testing.T) {
	inFlight := func(st ledgerState) func(*allocFixture) {
		return func(f *allocFixture) {
			g := grantEntry("g-db", "db-1", st)
			g.WroteRow, g.SettledAt, g.Marker.Incarnation = st == ledgerCommitted, allocNow, 9
			f.in.Ledger = []ledgerEntry{g}
		}
	}
	cases := []struct {
		name      string
		dbState   string
		alive     bool
		suspended bool
		setup     func(*allocFixture)
		want      string
	}{
		{"db asleep", "asleep", false, false, nil, "[db-1]"},
		{"db alive", "active", true, false, nil, "[app-1]"},
		{"db alive, committed grant", "active", true, false, inFlight(ledgerCommitted), "[]"},
		{"db alive, issued grant", "active", true, false, inFlight(ledgerIssued), "[]"},
		{"db alive, reserved grant", "active", true, false, inFlight(ledgerReserved), "[]"},
		{"db alive, start lease", "creating", true, false, nil, "[]"},
		{"db alive, suspended", "active", true, true, nil, "[]"},
	}
	for _, tc := range cases {
		db := allocPoolAgent("db", 3)
		db.Suspended = tc.suspended
		agents := []config.Agent{{Name: "app", MaxActiveSessions: intPtr(3), DependsOn: []string{"db"}}, db}
		f := newAllocFixture(t, grantCity(5, agents...)).sessions(
			poolRow("app-1", "app", 1, "asleep"), poolRow("db-1", "db", 1, tc.dbState, "last_woke_at", ago(5*time.Second)),
		).assign("app-1", "db-1").tokens(5)
		if tc.alive {
			f.alive("s-db-1", InventoryAttrs{})
		}
		if tc.setup != nil {
			tc.setup(f)
		}
		d := f.decide()
		if got := fmt.Sprint(granted(d)); got != tc.want {
			t.Errorf("%s: granted %s, want %s", tc.name, got, tc.want)
		}
		if e := entryOf(t, d, "app-1"); tc.want != "[app-1]" && e.Reason != "ineligible:"+gateDepsUnsatisfied {
			t.Errorf("%s: app-1 reason %q, want deps-unsatisfied", tc.name, e.Reason)
		}
	}
}

// Kills (C11, P3-5a obligation): a grant to an entry that is not a start
// candidate: a name another bead's runtime holds, unknown liveness, and
// every entry of a suspended city, whose reserved grants are released.
func TestGrants_NameOccupiedNeverGranted(t *testing.T) {
	f := newAllocFixture(t, grantCity(5)).sessions(
		poolRow("gc-a", "worker", 1, "active", "session_name", "shared"),
		poolRow("gc-b", "worker", 2, "asleep", "session_name", "shared"),
	).alive("shared", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-a"}).tokens(5)
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "shared"}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	if d := f.decide(); len(granted(d)) != 0 || entryOf(t, d, "gc-b").Reason != reasonNameOccupied {
		t.Fatalf("granted %v, gc-b %s: an occupied name must never be granted", granted(d), entryOf(t, d, "gc-b").Reason)
	}

	u := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1")...).assign("gc-1").tokens(5)
	u.noInventory = true
	if d := u.decide(); len(granted(d)) != 0 {
		t.Fatalf("unknown liveness granted %v", granted(d))
	}

	s := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1", "gc-2")...).assign("gc-1", "gc-2").tokens(5)
	s.in.CitySuspended = true
	s.in.Ledger = []ledgerEntry{grantEntry("g-1", "gc-1", ledgerReserved)}
	d := s.decide()
	if len(granted(d)) != 0 || len(opsOf(d, opReserve)) != 0 || len(opsOf(d, opRelease)) != 1 {
		t.Fatalf("suspended: granted %v ops %+v, want g-1 released and nothing reserved", granted(d), d.LedgerOps)
	}
}

// Kills (C5.8, C5.9, R23): a create burst with one token, creates behind a
// busy probe or a full city, and an empty budget read as unlimited
// (poolplan.NewCreateBudget(0) is unbudgeted).
func TestCreates_AdmittedOnlyWhenAGrantIsAdmissibleNowWithinPassTokens(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *allocFixture)
		want   int
		grants int
	}{
		{"one token", func(f *allocFixture) { f.tokens(1) }, 1, 0},
		{"no token", func(f *allocFixture) { f.tokens(0) }, 0, 0},
		{"grants first", func(f *allocFixture) {
			f.sessions(asleepRows("gc-1")...).assign("gc-1").tokens(2)
		}, 1, 1},
		{"city full", func(f *allocFixture) {
			f.tokens(5)
			f.in.Cfg.Daemon.MaxWakesPerTick = intPtr(1)
			f.in.Ledger = []ledgerEntry{grantEntry("g-x", "gc-x", ledgerIssued)}
		}, 0, 0},
		{"probe busy", func(f *allocFixture) {
			f.tokens(5)
			f.in.Cfg.Workspace.Provider = "claude"
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateProbe}}
			f.in.Ledger = []ledgerEntry{grantEntry("g-x", "gc-x", ledgerIssued)}
			f.in.Ledger[0].Endpoint = "provider:claude"
		}, 0, 0},
		{"probe free", func(f *allocFixture) {
			f.tokens(5)
			f.in.Cfg.Workspace.Provider = "claude"
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateProbe}}
		}, 1, 0},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, grantCity(5)).demand("worker", "n-1", "n-2", "n-3", "n-4")
		tc.setup(f)
		d := f.decide()
		if len(d.Creates) != tc.want || len(granted(d)) != tc.grants {
			t.Errorf("%s: %d creates %d grants (plans %d), want %d and %d", tc.name, len(d.Creates), len(granted(d)), len(d.Plans), tc.want, tc.grants)
		}
		if tc.want == 0 && !traceHas(d, reasonAwaitingBudget) {
			t.Errorf("%s: refused creates are not traced", tc.name)
		}
		capacity := f.in.Cfg.Daemon.MaxWakesPerTickOrDefault()
		if want := f.in.Bucket.refill(allocNow, capacity, time.Hour).Tokens - tc.grants; d.Bucket.Tokens != want {
			t.Errorf("%s: bucket %d, want %d: creates debit nothing", tc.name, d.Bucket.Tokens, want)
		}
	}
}

// Kills (AM-N9, C5.8): named sessions starved by elastic demand: the named
// tier is admitted before pool plans.
func TestCreates_NamedTierThenPoolAndFloorFairShare(t *testing.T) {
	cfg := grantCity(5, allocPoolAgent("worker", 10), config.Agent{Name: "chat"})
	cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
	for tokens, want := range map[int]string{1: "[named]", 3: "[named pool pool]"} {
		f := newAllocFixture(t, cfg).demand("worker", "n-1", "n-2").tokens(tokens)
		var kinds []string
		for _, c := range f.decide().Creates {
			if c.Named != nil {
				kinds = append(kinds, "named")
			} else {
				kinds = append(kinds, "pool")
			}
		}
		if got := fmt.Sprint(kinds); got != want {
			t.Errorf("%d tokens: creates %s, want %s", tokens, got, want)
		}
	}
}

// Kills (POOL-049, C4.8): a process-global seed or a fixed winner: the seed
// comes in and goes out as allocator state, and it rotates the scarce token.
func TestCreates_FairShareSeedIsAllocatorStateAndRotates(t *testing.T) {
	cfg := grantCity(5, allocPoolAgent("alpha", 5), allocPoolAgent("beta", 5))
	winner := func(seed uint64) (string, uint64) {
		f := newAllocFixture(t, cfg).demand("alpha", "a-1", "a-2").demand("beta", "b-1", "b-2").tokens(1)
		f.in.FairSeed = seed
		d := f.decide()
		if len(d.Creates) != 1 {
			t.Fatalf("seed %d: %d creates, want 1", seed, len(d.Creates))
		}
		return d.Creates[0].Template, d.FairSeed
	}
	w0, next := winner(0)
	w1, _ := winner(1)
	again, _ := winner(0)
	if w0 == w1 || w0 != again || next != 1 {
		t.Fatalf("seed 0 -> %s (next seed %d), seed 1 -> %s, seed 0 again -> %s: want rotation by the input seed", w0, next, w1, again)
	}
}

// Kills (finding 6, POOL-049): a seed advanced by a pass that admitted no
// fair-share create, so token passes alternating with empty event passes
// always see the same seed and one template wins every token.
func TestCreates_FairShareSeedAdvancesOnlyOnAdmittedCreate(t *testing.T) {
	cfg := grantCity(5, allocPoolAgent("alpha", 5), allocPoolAgent("beta", 5))
	f := newAllocFixture(t, cfg).demand("alpha", "a-1", "a-2").demand("beta", "b-1", "b-2")
	wins := map[string]int{}
	for i := 0; i < 40; i++ {
		f.tokens(1 - i%2)
		d := f.decide()
		for _, c := range d.Creates {
			wins[c.Template]++
		}
		if want := f.in.FairSeed + uint64(len(d.Creates)); d.FairSeed != want {
			t.Fatalf("pass %d: seed %d -> %d with %d creates, want %d", i, f.in.FairSeed, d.FairSeed, len(d.Creates), want)
		}
		f.in.FairSeed = d.FairSeed
	}
	if wins["alpha"] != 10 || wins["beta"] != 10 {
		t.Fatalf("wins over 20 token passes %v, want 10 each", wins)
	}
}

// Kills (R26, C8, POOL-058): a dependency floor create treated as elastic
// demand, so the seed, not the floor, decides the scarce token.
func TestCreates_DependencyFloorTakesTheScarceToken(t *testing.T) {
	cfg := grantCity(5, config.Agent{Name: "app", MaxActiveSessions: intPtr(3), DependsOn: []string{"db"}},
		allocPoolAgent("db", 3), allocPoolAgent("worker", 5))
	for seed := uint64(0); seed < 3; seed++ {
		f := newAllocFixture(t, cfg).sessions(poolRow("app-1", "app", 1, "asleep")).assign("app-1").demand("worker", "w-1", "w-2").tokens(1)
		f.in.FairSeed = seed
		if d := f.decide(); len(d.Creates) != 1 || d.Creates[0].Template != "db" {
			t.Errorf("seed %d: creates %+v, want the db floor", seed, d.Creates)
		}
	}
}

// Kills (C7.4, P3-6b owner decision 4): a create on a gated template, for
// pool and named plans alike: an incomplete census, BlockCreate, provider
// red, the #46 quarantine and a shut or missing endpoint view each refuse it.
func TestCreates_BlockedByPartialRedQuarantineEndpoint(t *testing.T) {
	cfg := grantCity(5, allocPoolAgent("worker", 10), config.Agent{Name: "chat"})
	cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
	cfg.Workspace.Provider = "claude"
	closed := map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
	named := namedRuntimeName(t, cfg)
	pool := boundSessionNameLength(poolIdentitySessionName("worker-1", "worker") + poolRuntimeNameSuffix)
	cases := []struct {
		name  string
		setup func(f *allocFixture)
		want  int
	}{
		{"open", func(*allocFixture) {}, 2},
		{"census incomplete", func(f *allocFixture) {
			f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: censusErrStore{Store: censusStore(), err: fmt.Errorf("down")}})
		}, 0},
		{"block create", func(f *allocFixture) {
			f.in.ScaleCheck = &scaleCheckResult{}
			f.in.Demand.CustomCheckTemplates = []string{"worker"}
		}, 1},
		{"provider red", func(f *allocFixture) {
			f.in.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{"claude": false}}
		}, 0},
		{"quarantine", func(f *allocFixture) {
			q := session.StartupHealthEpisode{QuarantinedUntil: allocNow.Add(time.Minute)}
			f.in.Episodes = map[string]session.StartupHealthEpisode{named: q, pool: q}
		}, 0},
		{"endpoint shut", func(f *allocFixture) {
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateShut}}
		}, 0},
		{"endpoint missing", func(f *allocFixture) { f.in.Endpoints = nil }, 0},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg).sessions().demand("worker", "n-1").tokens(5)
		f.in.Endpoints = closed
		tc.setup(f)
		if d := f.decide(); len(d.Creates) != tc.want {
			t.Errorf("%s: %d creates %+v, want %d (trace %v)", tc.name, len(d.Creates), d.Creates, tc.want, d.Trace)
		}
	}
}

// Kills (F1): a duplicate dependency-floor create across passes: the floor
// create's reservation goes back in and stands in for its row. The app row
// is desired in both passes, so db is a dependency root in both.
func TestCreates_DependencyFloorReservationStandsIn(t *testing.T) {
	cfg := grantCity(5, config.Agent{Name: "app", MaxActiveSessions: intPtr(3), DependsOn: []string{"db"}}, allocPoolAgent("db", 3))
	f := newAllocFixture(t, cfg).sessions(poolRow("app-1", "app", 1, "asleep")).assign("app-1").tokens(5)
	first := f.decide()
	floors := 0
	for _, c := range first.Creates {
		if c.Template == "db" {
			floors++
		}
	}
	if floors != 1 {
		t.Fatalf("pass 1 creates %+v, want one db floor", first.Creates)
	}
	applyReserves(f, first)
	second := f.decide()
	if len(second.Plans) != 0 || len(second.Creates) != 0 {
		t.Fatalf("pass 2 plans %+v creates %+v, want none: the floor create is in flight", second.Plans, second.Creates)
	}
}

// applyReserves feeds d's reserves and allocator state into the next pass,
// as P3-7's apply step would.
func applyReserves(f *allocFixture, d allocDecision) {
	for _, op := range opsOf(d, opReserve) {
		e := op.Entry
		e.State = ledgerReserved
		f.in.Ledger = append(f.in.Ledger, e)
	}
	f.in.Reservations, f.in.Bucket, f.in.FairSeed, f.in.EntrySeq, f.in.Prev = d.Reservations, d.Bucket, d.FairSeed, d.EntrySeq, d.Snapshot
}

// Kills (P3-5a, P3-6, P3-6b and PR J obligations): a create reserved
// without its trigger work, stamp or identity in its reservation; a create
// entry off the sessions leg or without its token (Reserve refuses both); a
// create plan not built by createPlanOf; a slot that is not the pool slot;
// a pass that does not feed its reservations back, so the work is planned
// again.
func TestCreates_ReservationsCarryTriggerWorkAndFeedBack(t *testing.T) {
	cfg := grantCity(5, allocPoolAgent("worker", 10), config.Agent{Name: "chat"})
	cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-2", "worker", 2, "active")).alive("s-gc-2", InventoryAttrs{}).
		demand("worker", "w-1", "w-3", "w-4").tokens(5)
	d := f.decide()
	if len(d.Creates) != 3 || len(d.Reservations) != 3 {
		t.Fatalf("creates %+v reservations %+v, want named + 2 pool", d.Creates, d.Reservations)
	}
	ledger := newIntentLedger(func() time.Time { return allocNow })
	res := make(map[string]planReservation)
	for _, r := range d.Reservations {
		res[r.EntryID] = r
	}
	var slots []int
	for i, op := range opsOf(d, opReserve) {
		c, r := d.Creates[i], res[op.ID]
		if op.Entry.Key.Leg != allocSessionsLeg || !ledger.Reserve(op.Entry) || c.EntryID != op.ID || !r.ReservedAt.Equal(allocNow) {
			t.Fatalf("reserve %+v create %+v reservation %+v: want a sessions-leg entry the ledger accepts", op, c, r)
		}
		if c.Named != nil {
			if r.NamedIdentity != "chat" || r.SessionName != c.Named.SessionName {
				t.Fatalf("named reservation %+v", r)
			}
			continue
		}
		var ap allocPlan
		for _, p := range d.Plans {
			if p.Named == nil && p.Plan.qualifiedInstance == c.QualifiedInstance {
				ap = p
			}
		}
		if want := createPlanOf(op.ID, ap.Template, ap.Plan); fmt.Sprint(c) != fmt.Sprint(want) || c.Slot != ap.Plan.poolSlot {
			t.Fatalf("create %+v, want createPlanOf %+v with slot = pool slot", c, want)
		}
		if r.WorkBeadID != ap.Request.WorkBeadID || r.Slot != c.Slot || r.QualifiedInstance != c.QualifiedInstance {
			t.Fatalf("pool reservation %+v for plan %+v", r, ap)
		}
		slots = append(slots, r.Slot)
	}
	if fmt.Sprint(slots) != "[1 3]" {
		t.Fatalf("reserved slots %v, want [1 3]: slots need not be contiguous", slots)
	}
	applyReserves(f, d)
	f.in.Now = allocNow.Add(time.Second)
	if next := f.decide(); len(next.Plans) != 0 || len(next.Reservations) != 3 {
		t.Fatalf("pass 2 plans %+v reservations %d, want none replanned and all three kept", next.Plans, len(next.Reservations))
	}
}

// Kills (P3-6): a create entry ID or token reused within the process: the
// sequence is allocator state threaded through every pass.
func TestCreates_EntryIDsAndTokensUniqueAcrossPasses(t *testing.T) {
	f := newAllocFixture(t, grantCity(5)).demand("worker", "w-1")
	ids, tokens := make(map[string]bool), make(map[string]bool)
	for pass := 0; pass < 4; pass++ {
		f.in.Ledger, f.in.Reservations = nil, nil // each pass's create cleared
		f.tokens(5)
		d := f.decide()
		for _, op := range opsOf(d, opReserve) {
			if ids[op.ID] || tokens[op.Entry.Marker.InstanceToken] || !strings.Contains(op.ID, f.in.Epoch) {
				t.Fatalf("pass %d: entry %s token %s reused, or no epoch", pass, op.ID, op.Entry.Marker.InstanceToken)
			}
			ids[op.ID], tokens[op.Entry.Marker.InstanceToken] = true, true
		}
		f.in.EntrySeq = d.EntrySeq
	}
	if len(ids) != 4 {
		t.Fatalf("%d entries over 4 passes, want 4", len(ids))
	}
}

// Kills (P3-4, P3-6b obligations): C5.5's in-flight cause matching a create
// only by token (a reopen sets Key.ID and never writes its token) or only by
// key (a create the census shows before it settles has no key).
func TestGrants_InFlightCauseMatchesCreateByTokenAndKey(t *testing.T) {
	for name, create := range map[string]ledgerEntry{
		"key":   {ID: "c-1", Kind: kindCreate, Key: sessKey("gc-1"), State: ledgerIssued, Marker: ledgerMarker{InstanceToken: "other"}},
		"token": {ID: "c-1", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, State: ledgerIssued, Marker: ledgerMarker{InstanceToken: "tok-1"}},
	} {
		f := newAllocFixture(t, grantCity(5)).sessions(poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-1")).
			assign("gc-1").tokens(5)
		create.ReservedAt = allocNow
		f.in.Ledger = []ledgerEntry{create}
		if e := entryOf(t, f.decide(), "gc-1"); e.Reason != "ineligible:"+gateInFlight {
			t.Errorf("by %s: reason %q, want in-flight", name, e.Reason)
		}
	}
}

// Kills (PR J): an ambiguous create cleared as unwritten without its create
// backoff, a backoff that advances the slot (fence), a backoff recorded for
// a failure whose effect already recorded one, and a cleared create keeping
// its reservation.
func TestHousekeep_AmbiguousUnwrittenBackoffOnlyWhenAmbiguous(t *testing.T) {
	f := newAllocFixture(t, grantCity(5)).tokens(5)
	f.legs = []classStoreCandidate{{ref: allocSessionsLeg, store: censusStore()}}
	in := f.inputs()
	in.Census.Legs[0].Exact, in.Census.Legs[0].StartedAt = false, allocNow
	settled := allocNow.Add(-time.Second)
	amb := ledgerEntry{
		ID: "c-amb", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, State: ledgerCommitted, WroteRow: true,
		Ambiguous: true, SettledAt: settled, ConfigRev: "rev-1", Marker: ledgerMarker{InstanceToken: "t1"},
	}
	failed := ledgerEntry{
		ID: "c-fail", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, State: ledgerFailed, SettledAt: settled,
		Marker: ledgerMarker{InstanceToken: "t2"},
	}
	in.Ledger = []ledgerEntry{amb, failed}
	in.Reservations = []planReservation{
		{EntryID: "c-amb", Template: "worker", QualifiedInstance: "worker-1", Slot: 1},
		{EntryID: "c-fail", Template: "worker", QualifiedInstance: "worker-2", Slot: 2},
	}
	d := mustDecide(t, in)
	clears := opsOf(d, opClear)
	if len(clears) != 2 || len(d.Reservations) != 0 {
		t.Fatalf("clears %+v reservations %+v, want both cleared and both reservations dropped", clears, d.Reservations)
	}
	for _, op := range clears {
		switch op.ID {
		case "c-amb":
			want := backoffOp{Key: "create:worker/worker-1", Until: allocNow.Add(3 * time.Hour), Cause: createStageWrite, Fingerprint: "rev-1"}
			if op.Clear != clearUnwritten || op.Backoff == nil || *op.Backoff != want || op.From != ledgerCommitted {
				t.Fatalf("ambiguous clear %+v backoff %+v, want %+v", op, op.Backoff, want)
			}
		case "c-fail":
			if op.Backoff != nil {
				t.Fatalf("a no-write failure recorded a second backoff: %+v", op.Backoff)
			}
		}
	}
}

// Kills (R8, R34, P3-4 obligation): an entry this pass clears still
// counted in flight, so a landed start holds its city slot after its marker
// shows.
func TestHousekeep_ClearedEntryHoldsNoSlot(t *testing.T) {
	f := newAllocFixture(t, grantCity(1)).sessions(
		poolRow("gc-1", "worker", 1, "active", "generation", "2", "last_woke_at", ago(time.Hour)), poolRow("gc-2", "worker", 2, "asleep"),
	).alive("s-gc-1", InventoryAttrs{}).assign("gc-1", "gc-2").tokens(5)
	g := grantEntry("g-1", "gc-1", ledgerCommitted)
	g.WroteRow, g.SettledAt, g.Marker.Incarnation = true, allocNow, 2
	f.in.Ledger = []ledgerEntry{g}
	d := f.decide()
	if c := opsOf(d, opClear); len(c) != 1 || c[0].Clear != clearWritten || fmt.Sprint(granted(d)) != "[gc-2]" {
		t.Fatalf("clears %+v granted %v, want g-1 cleared by its marker and gc-2 granted the slot", c, granted(d))
	}
}

// Kills (PR J, C5.15): a hard-bound clear without its alert naming the
// entry, its key and its leg, or without a trace record.
func TestHousekeep_HardBoundAlertsAndTraces(t *testing.T) {
	f := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1")...).tokens(5)
	g := grantEntry("g-1", "gc-1", ledgerCommitted)
	g.WroteRow, g.SettledAt, g.Marker.Incarnation = true, allocNow.Add(-ledgerHardBound), 9
	f.in.Ledger = []ledgerEntry{g}
	d := f.decide()
	clears := opsOf(d, opClear)
	if len(clears) != 1 || clears[0].Clear != clearHardBound ||
		!strings.Contains(clears[0].Alert, "g-1") || !strings.Contains(clears[0].Alert, "gc-1") || !strings.Contains(clears[0].Alert, allocSessionsLeg) {
		t.Fatalf("clears %+v, want a hard-bound clear alerting g-1, gc-1 and its leg", clears)
	}
	if !traceHas(d, "ledger-hard-bound:g-1") {
		t.Fatalf("no trace record for the hard-bound clear: %v", d.Trace)
	}
}

// Kills (POOL-070, R6, R22): an issued or committed grant left out of awake
// supply, so its row, asleep in the census until PreWake shows, loses its
// scaled slot to a create; and an active row projected as creating.
func TestAllocator_IssuedGrantCountsAsAwakeSupply(t *testing.T) {
	scaled := func(f *allocFixture) {
		f.in.ScaleCheck = &scaleCheckResult{Counts: map[string]int{"worker": 1}, At: allocNow}
		f.in.Demand.CustomCheckTemplates = []string{"worker"}
	}
	for _, st := range []ledgerState{ledgerIssued, ledgerCommitted} {
		f := newAllocFixture(t, grantCity(5)).sessions(poolRow("gc-1", "worker", 1, "asleep")).tokens(5)
		scaled(f)
		g := grantEntry("g-1", "gc-1", st)
		g.WroteRow, g.SettledAt, g.Marker.Incarnation = st == ledgerCommitted, allocNow, 2 // the marker is not in the census yet
		f.in.Ledger = []ledgerEntry{g}
		d := f.decide()
		e := entryOf(t, d, "gc-1")
		if e.Desired != desireWake || len(d.Creates) != 0 || (st == ledgerIssued && (e.Start == nil || e.Start.Grant != "g-1")) {
			t.Errorf("state %d: gc-1 = %s/%s start %+v creates %d, want it woken on its grant and no create", st, e.Desired, e.Reason, e.Start, len(d.Creates))
		}

		a := newAllocFixture(t, grantCity(5)).sessions(poolRow("gc-1", "worker", 1, "active", "last_woke_at", ago(time.Minute))).
			alive("s-gc-1", InventoryAttrs{}).tokens(5)
		scaled(a)
		g.Marker.Incarnation = 3
		a.in.Ledger = []ledgerEntry{g}
		if e := entryOf(t, a.decide(), "gc-1"); e.Reason != "scaled:demand" {
			t.Errorf("state %d: active gc-1 reason %q, want scaled:demand: an active row is not projected as creating", st, e.Reason)
		}
	}
}

// allocSim drives passes end to end, as P3-7 and the session keys will: the
// decision's ops applied in order to a real ledger, the create executor
// issuing each create and writing its pending row, and session keys issuing
// reserved grants and committing the start (generation up, runtime alive).
// With rng nil every effect is issued and lands in the pass; with rng set,
// creates and grants may stay reserved, land or fail without a write, and
// an issue may beat a release.
type allocSim struct {
	t      *testing.T
	f      *allocFixture
	rng    *rand.Rand
	ledger *intentLedger
	rows   map[string]beads.Bead
	order  []string
	alive  map[string]bool
	step   time.Duration
	next   int
	// Counts: grant debits, confirmed refunds, issued grants, creates
	// written and starts committed.
	debits, refunds, issued, creates, starts int
}

func newAllocSim(t *testing.T, f *allocFixture, rng *rand.Rand, rows ...beads.Bead) *allocSim {
	s := &allocSim{t: t, f: f, rng: rng, rows: map[string]beads.Bead{}, alive: map[string]bool{}, step: time.Second}
	s.ledger = newIntentLedger(func() time.Time { return s.f.in.Now })
	for _, r := range rows {
		s.rows[r.ID], s.order = r, append(s.order, r.ID)
	}
	return s
}

func (s *allocSim) chance(n int) bool { return s.rng == nil || s.rng.Intn(n) != 0 }

// pass runs one decide and its effects, checks the bucket, and advances the
// clock by step.
func (s *allocSim) pass() {
	f := s.f
	f.legs, f.listed, f.attrs = nil, nil, map[string]InventoryAttrs{}
	var rows []beads.Bead
	for _, id := range s.order {
		rows = append(rows, s.rows[id])
	}
	f.sessions(rows...)
	for name := range s.alive {
		f.alive(name, InventoryAttrs{})
	}
	f.in.Ledger = s.ledger.View()
	capacity, interval := f.in.Cfg.Daemon.MaxWakesPerTickOrDefault(), f.in.Cfg.Daemon.PatrolIntervalDuration()
	d := f.decide()
	grants := 0
	for _, op := range opsOf(d, opReserve) {
		if op.Entry.Kind == kindGrant {
			grants++
		}
	}
	if want := f.in.Bucket.refill(f.in.Now, capacity, interval).Tokens - grants; d.Bucket.Tokens != want || want < 0 {
		s.t.Fatalf("bucket %d, want %d: each pass debits exactly its new grants", d.Bucket.Tokens, want)
	}
	b := d.Bucket
	for _, op := range d.LedgerOps {
		switch op.Kind {
		case opClear:
			s.ledger.Transition(op.ID, op.From, ledgerCleared, nil)
		case opRelease:
			if e := s.entry(op.ID); s.rng != nil && e.Kind == kindGrant && s.rng.Intn(3) == 0 && s.ledger.Issue(e.ID, e.Key) {
				s.issued++ // the key's issue beat the release: the token is spent
				s.settleGrant(e)
			}
			if n, ok := s.ledger.Release(op.ID); ok {
				s.refunds += n
				b = b.refund(n, capacity)
			}
		case opReserve:
			if !s.ledger.Reserve(op.Entry) {
				s.t.Fatalf("reserve %s refused", op.ID)
			}
			if op.Entry.Kind == kindGrant {
				s.debits++
			}
		}
	}
	if b.Tokens > capacity {
		s.t.Fatalf("bucket %d over capacity %d", b.Tokens, capacity)
	}
	f.in.Bucket, f.in.EntrySeq, f.in.FairSeed, f.in.Prev, f.in.Reservations = b, d.EntrySeq, d.FairSeed, d.Snapshot, d.Reservations
	for _, c := range d.Creates {
		if s.chance(4) {
			s.create(c)
		}
	}
	for _, e := range s.ledger.View() {
		if e.Kind == kindGrant && e.State == ledgerReserved && s.chance(2) && s.ledger.Issue(e.ID, e.Key) {
			s.issued++
			s.settleGrant(e)
		}
	}
	f.in.Now = f.in.Now.Add(s.step)
}

func (s *allocSim) entry(id string) ledgerEntry {
	for _, e := range s.ledger.View() {
		if e.ID == id {
			return e
		}
	}
	return ledgerEntry{}
}

// create issues c and writes its pending row, or fails without a write.
func (s *allocSim) create(c createPlan) {
	tok, _, ok := s.ledger.IssueCreate(c.EntryID)
	if !ok {
		return
	}
	if !s.chance(5) {
		s.ledger.Fail(c.EntryID, false, ledgerMarker{})
		return
	}
	s.next++
	id := fmt.Sprintf("n-%d", s.next)
	meta := []string{"pending_create_claim", "true", "pending_create_started_at", s.f.in.Now.UTC().Format(time.RFC3339), "instance_token", tok}
	for k, v := range c.Metadata {
		meta = append(meta, k, v)
	}
	s.rows[id], s.order = poolRow(id, c.Template, c.Slot, "start-pending", meta...), append(s.order, id)
	s.ledger.CommitCreate(c.EntryID, false, ledgerMarker{RowID: id, InstanceToken: tok})
	s.creates++
}

// settleGrant commits an issued grant's start (PreWake: generation up,
// state active, runtime alive), or fails it without a write.
func (s *allocSim) settleGrant(e ledgerEntry) {
	if !s.chance(3) {
		s.ledger.Fail(e.ID, false, ledgerMarker{})
		return
	}
	row := s.rows[e.Key.ID]
	gen, _ := strconv.Atoi(row.Metadata["generation"])
	gen++
	meta := maps.Clone(row.Metadata)
	meta["generation"], meta["state"], meta["last_woke_at"] = fmt.Sprint(gen), "active", s.f.in.Now.UTC().Format(time.RFC3339)
	delete(meta, "pending_create_claim")
	row.Metadata = meta
	s.rows[e.Key.ID] = row
	s.alive[meta["session_name"]] = true
	s.ledger.Commit(e.ID, ledgerMarker{Incarnation: int64(gen)})
	s.starts++
}

// pending is the rows created and not yet started.
func (s *allocSim) pending() int {
	n := 0
	for _, r := range s.rows {
		if r.Metadata["pending_create_claim"] == "true" {
			n++
		}
	}
	return n
}

// Kills (finding 1, C5.9, C5.13): never-started pending rows blocking their
// own grants once the city reaches its cap, so demand at or above the cap
// creates and never starts. Each grant continues its row's counted slot.
func TestGrants_PendingRowsAtCapAreGranted(t *testing.T) {
	for _, tc := range []struct{ cap, pending int }{{3, 3}, {5, 3}, {50, 50}} {
		rows, work := pendingRows(tc.pending, "worker")
		d := newAllocFixture(t, grantCity(tc.cap, allocPoolAgent("worker", 100))).sessions(rows...).demand("worker", work...).tokens(tc.cap).decide()
		if len(granted(d)) != tc.pending || len(d.Creates) != 0 {
			t.Errorf("cap %d, %d pending: %d grants %d creates, want every pending row granted and no create", tc.cap, tc.pending, len(granted(d)), len(d.Creates))
		}
	}
}

// Kills (finding 1, C5.9, C5.13): demand at or above the cap, run through
// creates to starts. Every bead is created and started, and no more than
// the cap are ever created and not started.
func TestAdmission_DemandAtCapRunsThroughCreatesToStarts(t *testing.T) {
	for _, tc := range []struct{ cap, work, passes int }{{3, 3, 6}, {50, 60, 40}} {
		var work []string
		for i := 1; i <= tc.work; i++ {
			work = append(work, fmt.Sprintf("w-%d", i))
		}
		cfg := grantCity(tc.cap, allocPoolAgent("worker", 100))
		cfg.Daemon.PatrolInterval = fmt.Sprintf("%ds", tc.cap) // one token a second
		f := newAllocFixture(t, cfg).demand("worker", work...).tokens(tc.cap)
		s := newAllocSim(t, f, nil)
		for i := 0; i < tc.passes; i++ {
			s.pass()
			if n := s.pending(); n > tc.cap {
				t.Fatalf("cap %d, %d beads: pass %d leaves %d rows created and not started", tc.cap, tc.work, i, n)
			}
		}
		if s.creates != tc.work || s.starts != tc.work {
			t.Errorf("cap %d, %d beads: %d creates %d starts after %d passes, want every bead created and started", tc.cap, tc.work, s.creates, s.starts, tc.passes)
		}
	}
}

// Kills (finding 3, F5): a half-open endpoint whose parked rows cannot take
// the probe (their dependency is down, or their liveness unknown) trickling
// one create a pass past the cap: behind a probe gate the parked rows are
// outstanding for creates.
func TestCreates_HalfOpenParkedRowsHoldTheProbe(t *testing.T) {
	var work []string
	for i := 1; i <= 20; i++ {
		work = append(work, fmt.Sprintf("w-%d", i))
	}
	for _, depsDown := range []bool{true, false} {
		for _, gate := range []endpointGate{gateClosed, gateProbe} {
			a := config.Agent{Name: "a", Provider: "pa", MaxActiveSessions: intPtr(100)}
			agents := []config.Agent{a}
			if depsDown {
				agents[0].DependsOn = []string{"d"}
				agents = append(agents, config.Agent{Name: "d", Provider: "pd", MaxActiveSessions: intPtr(3)})
			}
			f := newAllocFixture(t, grantCity(5, agents...)).demand("a", work...).tokens(5)
			f.noInventory = !depsDown
			f.in.Endpoints = map[endpointKey]endpointView{"provider:pa": {Gate: gate, HoldsPendingCreate: true}, "provider:pd": {Gate: gateShut, HoldsPendingCreate: true}}
			s := newAllocSim(t, f, nil)
			for i := 0; i < 15; i++ {
				s.pass()
			}
			if s.creates > 5 || s.starts != 0 {
				t.Errorf("deps down %v, gate %d: %d creates %d starts after 15 passes, want at most the cap of 5 and none started", depsDown, gate, s.creates, s.starts)
			}
		}
	}
}

// Kills (I-tokens, C5.7, finding 8): a leaked or double-counted token.
// Seeded passes run through allocSim with a refilling bucket, creates and
// grants that land, fail or stay reserved, and issues that beat releases;
// each pass debits exactly its new grants, refunds come only from confirmed
// releases, and at quiescence the ledger is empty and debits minus refunds
// equal the grants ever issued.
func TestGrants_TokenConservationProperty(t *testing.T) {
	ids := []string{"gc-1", "gc-2", "gc-3", "gc-4", "gc-5"}
	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cfg := grantCity(3)
		cfg.Daemon.PatrolInterval = "30s" // a token every 10s
		f := newAllocFixture(t, cfg).assign(ids...).demand("worker", "w-a", "w-b", "w-c").tokens(3)
		s := newAllocSim(t, f, rng, asleepRows(ids...)...)
		for step := 0; step < 30; step++ {
			s.step = time.Duration(rng.Intn(20)) * time.Second
			if step >= 25 {
				f.in.Demand, s.step = demandView{}, ledgerReserveTTL
			}
			s.pass()
		}
		if v := s.ledger.View(); len(v) != 0 || s.debits-s.refunds != s.issued {
			t.Fatalf("seed %d: ledger %+v, debits %d refunds %d issued %d", seed, v, s.debits, s.refunds, s.issued)
		}
	}
}

// Kills (C2.10; R2, R3b, R4): an enqueue diff that is empty, ignores a
// changed grant, a removed entry or a changed partial state, or enqueues
// unchanged keys.
func TestAllocator_EnqueueDiffAgainstPrev(t *testing.T) {
	fixture := func(grant string) *allocFixture {
		f := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1", "gc-2")...).assign("gc-1").tokens(5)
		f.in.Ledger = []ledgerEntry{grantEntry(grant, "gc-1", ledgerReserved)}
		return f
	}
	f := fixture("g-1")
	first := f.decide()
	if got := fmt.Sprint(first.Enqueue); got != fmt.Sprint([]rowKey{sessKey("gc-1"), sessKey("gc-2")}) {
		t.Fatalf("no Prev: enqueue %s, want every key", got)
	}
	f.in.Prev = first.Snapshot
	if d := f.decide(); len(d.Enqueue) != 0 {
		t.Fatalf("unchanged: enqueue %v, want none", d.Enqueue)
	}

	g := fixture("g-2") // gc-1's grant changed, nothing else
	g.in.Prev = first.Snapshot
	if d := g.decide(); fmt.Sprint(d.Enqueue) != fmt.Sprint([]rowKey{sessKey("gc-1")}) {
		t.Fatalf("grant changed: enqueue %v, want gc-1", d.Enqueue)
	}

	removed := *first.Snapshot
	removed.Entries = maps.Clone(removed.Entries)
	removed.Entries[sessKey("gc-9")] = &selectionEntry{Key: sessKey("gc-9")}
	f.in.Prev = &removed
	if d := f.decide(); fmt.Sprint(d.Enqueue) != fmt.Sprint([]rowKey{sessKey("gc-9")}) {
		t.Fatalf("gc-9 removed: enqueue %v, want gc-9", d.Enqueue)
	}

	for name, changed := range map[string]partialState{
		"leg":             {Legs: map[string][]string{allocSessionsLeg: {causeStoreQueryPartial}}},
		"template retain": {Templates: map[string]templatePartial{"worker": {Retain: true}}},
		"template causes": {Templates: map[string]templatePartial{"worker": {Causes: []string{causeStoreQueryPartial}}}},
	} {
		partial := *first.Snapshot
		partial.Partial = changed
		f.in.Prev = &partial
		if d := f.decide(); fmt.Sprint(d.Enqueue) != fmt.Sprint([]rowKey{sessKey("gc-1"), sessKey("gc-2")}) {
			t.Errorf("%s partial state changed: enqueue %v, want both worker keys", name, d.Enqueue)
		}
	}
}

// Kills (R1, R30, R35): a NextWake that is always the patrol backstop, that
// ignores a starved candidate's refill, a new reservation's TTL, an older
// reservation's remaining TTL or a backoff expiring.
func TestAllocator_NextWake(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *allocFixture)
		want  time.Duration
	}{
		{"idle", func(f *allocFixture) { f.in.Demand = demandView{} }, 0},
		{"starved", func(f *allocFixture) { f.tokens(0) }, 12 * time.Minute}, // 1h / 5 tokens
		{"new grant", func(*allocFixture) {}, ledgerReserveTTL},
		{"held grant", func(f *allocFixture) {
			g := grantEntry("g-1", "gc-1", ledgerReserved)
			g.ReservedAt = allocNow.Add(-20 * time.Second)
			f.in.Ledger = []ledgerEntry{g}
		}, 10 * time.Second},
		{"backoff", func(f *allocFixture) {
			f.in.Backoff = map[string]backoffRecord{rowBackoffKey(sessKey("gc-1")): {Until: allocNow.Add(5 * time.Second)}}
		}, 5 * time.Second},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, grantCity(5)).sessions(asleepRows("gc-1")...).assign("gc-1").tokens(5)
		tc.setup(f)
		if d := f.decide(); d.NextWake != tc.want {
			t.Errorf("%s: NextWake %v, want %v", tc.name, d.NextWake, tc.want)
		}
	}
}
