package main

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// demand sets default-probe demand: count new requests for template, each
// driven by one routed work bead.
func (f *allocFixture) demand(template string, workIDs ...string) *allocFixture {
	c := &f.in.Demand.Collected
	c.DefaultProbed = true
	if c.DefaultCounts == nil {
		c.DefaultCounts = make(map[string]int)
		c.DefaultDemand = make(map[string]scaleCheckDemand)
	}
	c.DefaultCounts[template] = len(workIDs)
	d := scaleCheckDemand{StoreRefs: map[string]string{}}
	for _, id := range workIDs {
		d.WorkBeadIDs = append(d.WorkBeadIDs, id)
		d.StoreRefs[id] = "city"
	}
	c.DefaultDemand[template] = d
	return f
}

func planSlots(d allocDecision, template string) []int {
	var out []int
	for _, p := range d.Plans {
		if p.Template == template && p.Named == nil {
			out = append(out, p.Plan.poolSlot)
		}
	}
	return out
}

func planWork(d allocDecision) []string {
	var out []string
	for _, p := range d.Plans {
		if p.Named == nil {
			out = append(out, p.Request.WorkBeadID)
		}
	}
	slices.Sort(out)
	return out
}

func traceHas(d allocDecision, reason string) bool {
	for _, r := range d.Trace {
		if strings.Contains(r.Reason, reason) {
			return true
		}
	}
	return false
}

func hasNamedPlan(d allocDecision) bool {
	for _, p := range d.Plans {
		if p.Kind == createNamed {
			return true
		}
	}
	return false
}

// reserveAll turns d's plans into uncleared create entries, as P3-5b's
// admission would: each reserved at now with its plan's trigger work, and
// no row in the census yet.
func reserveAll(f *allocFixture, d allocDecision, at time.Time) {
	for i, p := range d.Plans {
		id := fmt.Sprintf("c-%d-%s", len(f.in.Ledger)+i, p.identity())
		f.in.Ledger = append(f.in.Ledger, ledgerEntry{
			ID: id, Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, Template: p.Template,
			State: ledgerReserved, ReservedAt: at, Marker: ledgerMarker{InstanceToken: "tok-" + id},
		})
		f.in.Reservations = append(f.in.Reservations, planReservation{
			EntryID: id, Template: p.Template, QualifiedInstance: p.Plan.qualifiedInstance, Slot: p.Plan.poolSlot,
			WorkBeadID: p.Request.WorkBeadID, ReservedAt: at,
		})
	}
}

func TestDecideSmokePlansFreshPoolSessionsForDemand(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	d := newAllocFixture(t, cfg).demand("worker", "w-1", "w-2").decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("plans = %v, trace %v", got, d.Trace)
	}
	if d.Snapshot.PoolDesired["worker"] != 2 {
		t.Fatalf("PoolDesired = %v", d.Snapshot.PoolDesired)
	}
}

// Kills: plans from a suspended city (POOL-001, C2.1).
func TestAllocator_CitySuspended_PlansNothing(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{allocPoolAgent("worker", 3), {Name: "chat"}},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: "always"}},
	}
	f := newAllocFixture(t, cfg).demand("worker", "w-1", "w-2")
	f.in.CitySuspended = true
	if d := f.decide(); len(d.Plans) != 0 || len(d.Snapshot.PoolDesired) != 0 {
		t.Fatalf("a suspended city planned: plans %+v desired %v", d.Plans, d.Snapshot.PoolDesired)
	}
}

// Kills: double counting across store groups (POOL-004). A rig store that
// aliases the city store reports the same routed bead from both groups;
// the collector dedups it by ID and the decide counts it once.
func TestAllocator_DefaultDemand_UnionsOwnRigAndCityLegsDedupByID(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	shared := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "w-1", Title: "w-1", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}, nil)
	targets := []defaultScaleCheckTarget{
		{template: "worker", storeKey: "city", store: shared},
		{template: "worker", storeKey: "rig-a", store: shared},
	}
	counts, demand, partials, errs := defaultScaleCheckCountsAndDemand(cfg, targets, newReadyDemandCache())
	if len(errs) > 0 || len(partials) > 0 {
		t.Fatalf("collector errs=%v partials=%v", errs, partials)
	}
	f := newAllocFixture(t, cfg)
	f.in.Demand.Collected = collectedDemand{DefaultProbed: true, DefaultCounts: counts, DefaultDemand: demand}
	d := f.decide()
	if got := d.Snapshot.PoolDesired["worker"]; got != 1 {
		t.Fatalf("PoolDesired = %d, want 1 (one bead seen from two store groups)", got)
	}
	if got := planSlots(d, "worker"); len(got) != 1 {
		t.Fatalf("plans = %v, want one", got)
	}
}

// Kills: shrinking during a partial read (POOL-035). A pool whose custom
// scale_check is partial keeps every row it would have released: the live
// row is retained by count and the asleep one by the overlay, and neither
// sleeps or drains.
func TestAllocator_PartialTemplate_KeepSetNeverShrinks(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	rows := []beads.Bead{poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "worker", 2, "asleep")}
	run := func(partial bool) allocDecision {
		f := newAllocFixture(t, cfg).sessions(rows...).alive("s-gc-1", InventoryAttrs{})
		f.in.Demand.CustomCheckTemplates = []string{"worker"}
		f.in.ScaleCheck = &scaleCheckResult{Counts: map[string]int{"worker": 0}}
		if partial {
			f.in.ScaleCheck.Partial = map[string]bool{"worker": true}
		}
		return f.decide()
	}
	healthy := run(false)
	if e := entryOf(t, healthy, "gc-2"); e.Desired != desireDrain {
		t.Fatalf("control: idle asleep row without a partial read = %s, want drain", e.Desired)
	}
	d := run(true)
	if tp := d.Snapshot.Partial.Templates["worker"]; !tp.Retain || !tp.BlockCreate {
		t.Fatalf("template partial = %+v, want retain and block-create", tp)
	}
	for _, id := range []string{"gc-1", "gc-2"} {
		if e := entryOf(t, d, id); e.Desired == desireSleep || e.Desired == desireDrain {
			t.Errorf("%s = %s under a partial read, want no shrink", id, e.Desired)
		}
	}
}

// Kills: a create during a partial read (POOL-036); reuse refused with it.
// The partial count still drives reuse of the live row, but no fresh plan.
func TestAllocator_PartialTemplate_BlocksFreshCreateNotReuse(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).alive("s-gc-1", InventoryAttrs{})
	f.in.Demand.CustomCheckTemplates = []string{"worker"}
	f.in.ScaleCheck = &scaleCheckResult{Counts: map[string]int{"worker": 3}, Partial: map[string]bool{"worker": true}}
	d := f.decide()
	if !entryOf(t, d, "gc-1").InDesired {
		t.Fatal("the live row must still be reused under a partial read")
	}
	if len(d.Plans) != 0 || !traceHas(d, gatePartial) {
		t.Fatalf("plans %+v, trace %v: a partial template creates nothing", d.Plans, d.Trace)
	}
}

// Kills: a create from a narrower view (POOL-047). A leg with nothing to
// serve leaves the census incomplete: the snapshot is partial, nothing
// shrinks, every fresh create is refused with the census cause, and reuse
// still works.
func TestAllocator_IncompleteCensus_BlocksFreshCreate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).alive("s-gc-1", InventoryAttrs{}).
		demand("worker", "w-1", "w-2", "w-3")
	f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: censusErrStore{Store: censusStore(), err: errors.New("rig down")}})
	d := f.decide()
	if d.Snapshot.Mode != modePartial || !slices.Contains(d.Snapshot.Partial.Global, causeCensusIncomplete) {
		t.Fatalf("mode %s partial %+v, want partial census-incomplete", d.Snapshot.Mode, d.Snapshot.Partial)
	}
	if len(d.Plans) != 0 || !traceHas(d, gateCensusIncomplete) {
		t.Fatalf("plans %+v trace %v: an incomplete census creates nothing", d.Plans, d.Trace)
	}
	if !entryOf(t, d, "gc-1").InDesired {
		t.Fatal("reuse must survive an incomplete census")
	}
}

// Kills: slot double-allocation across legs (POOL-050). A row on another leg
// is census-only (None) but still occupies its slot, so a fresh plan takes
// the lowest slot free on every leg.
func TestAllocator_FreshSlot_LowestFreeAcrossAllLegs(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	d := newAllocFixture(t, cfg).
		sessions(poolRow("gc-1", "worker", 1, "asleep")).
		rigLeg(poolRow("rg-2", "worker", 2, "active")).
		demand("worker", "w-1").decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[3]" {
		t.Fatalf("plan slots = %v, want [3] (1 and 2 are held on two legs); trace %v", got, d.Trace)
	}
	if e := entryOf(t, d, "rg-2"); e.Desired != desireNone || e.Reason != reasonCensusOnly {
		t.Fatalf("other-leg row = %s/%s, want census-only none", e.Desired, e.Reason)
	}
	for _, p := range d.Plans {
		if p.Plan.slot != p.Plan.poolSlot {
			t.Fatalf("plan slot %d != pool slot %d (P3-6 refuses it as stale)", p.Plan.slot, p.Plan.poolSlot)
		}
	}
}

// Kills: a failed-create row freeing its name (POOL-050). Its slot is free
// but its identity lease holds: slot 1's identity is refused (traced), and
// the request moves on to slot 2 rather than stalling the template.
func TestAllocator_FreshSlot_FailedCreateKeepsNameNotSlot(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	d := newAllocFixture(t, cfg).
		sessions(poolRow("gc-1", "worker", 1, "failed-create")).
		demand("worker", "w-1").decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonFailedCreate {
		t.Fatalf("failed-create row = %s/%s", e.Desired, e.Reason)
	}
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[2]" {
		t.Fatalf("plan slots = %v, want [2]: slot 1 is free but its name is leased", got)
	}
	if !traceHas(d, gateIdentityLease) {
		t.Fatalf("the leased identity must be traced: %v", d.Trace)
	}
}

// Kills: one refused identity starving its template (owner decision at P3-5a
// review): a live create backoff on worker-1 with slots 2-5 free and demand for
// three plans three, on slots 2-4.
func TestAllocator_RefusedIdentityDoesNotStarveTemplate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	f := newAllocFixture(t, cfg).demand("worker", "w-1", "w-2", "w-3")
	f.in.Backoff = createRefusal("worker/worker-1", createStageFence, allocNow.Add(5*time.Minute))
	d := f.decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[2 3 4]" {
		t.Fatalf("plans = %v, want [2 3 4]; trace %v", got, d.Trace)
	}
	if !traceHas(d, gateCreateRefused+"fence") {
		t.Fatalf("the refused identity must be traced: %v", d.Trace)
	}
}

// Kills: a refusal that is not specific to the planned name moving the
// request to the next slot (F3): one template-wide create failure, or one
// quarantined start, would spread across every slot pass after pass and
// defeat per-identity backoff. As legacy stalls (build_desired_state.go
// 5180), a template-wide create backoff (prepare, lock, fence-read) and a
// #46 quarantine plan nothing past the refused slot; a fence refusal (the
// name was taken) advances one slot per pass, within the cap. The refusals
// go through the backoff table, so its causes are kept verbatim (C3).
func TestAllocator_RefusedIdentityStallsUnlessNameSpecific(t *testing.T) {
	for _, cause := range []string{"prepare", "lock", createStageFenceRead, "quarantine", createStageFence} {
		for label, agent := range map[string]config.Agent{"max-5": allocPoolAgent("worker", 5), "unlimited": {Name: "worker", MaxActiveSessions: intPtr(-1)}} {
			cfg := &config.City{Agents: []config.Agent{agent}}
			table := newBackoffTable()
			episodes := map[string]session.StartupHealthEpisode{}
			var slots []int
			for pass := 0; pass < 7; pass++ {
				f := newAllocFixture(t, cfg).demand("worker", "w-1")
				f.in.Backoff, f.in.Episodes = table.Snapshot(), episodes
				d := f.decide()
				if len(d.Plans) == 0 {
					slots = append(slots, 0)
					continue
				}
				p := d.Plans[0]
				slots = append(slots, p.Plan.poolSlot)
				if cause == "quarantine" {
					key := boundSessionNameLength(poolIdentitySessionName(p.Plan.qualifiedInstance, "worker") + poolRuntimeNameSuffix)
					episodes[key] = session.StartupHealthEpisode{QuarantinedUntil: allocNow.Add(5 * time.Minute)}
					continue
				}
				table.Refuse(createBackoffKey(p.identity()), allocNow, time.Time{}, cause, f.in.ConfigRev)
			}
			want := "[1 0 0 0 0 0 0]"
			switch {
			case cause == createStageFence && label == "max-5":
				want = "[1 2 3 4 5 0 0]"
			case cause == createStageFence:
				want = "[1 2 3 4 5 6 7]"
			}
			if got := fmt.Sprint(slots); got != want {
				t.Errorf("%s, %s pool: slot planned per pass %s, want %s", cause, label, got, want)
			}
		}
	}
}

// Kills: a template-wide gate misread as name-specific (R17, R18): it would
// spread across every slot, and loop over an unlimited pool's. A shut
// endpoint and an unresolvable tmux_alias refuse every slot alike, so the
// request stalls on its first plan, in a capped pool and in an unlimited
// one whose fence backoff elsewhere allows more than one try. However a
// refusal is classified, the slots tried are bounded.
func TestAllocator_TemplateWideRefusalStallsAnUnlimitedPool(t *testing.T) {
	for cause, setup := range map[string]func(*config.Agent, *allocFixture){
		gateEndpointShut: func(_ *config.Agent, f *allocFixture) {
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateShut}}
		},
		gateNoSlot: func(a *config.Agent, _ *allocFixture) { a.TmuxAlias = "{{.Unclosed" },
	} {
		for _, limit := range []int{5, -1} {
			agent := config.Agent{Name: "worker", MaxActiveSessions: intPtr(limit)}
			f := newAllocFixture(t, nil).demand("worker", "w-1")
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
			f.in.Backoff = createRefusal("worker/worker-9", createStageFence, allocNow.Add(time.Minute))
			setup(&agent, f)
			f.in.Cfg = &config.City{Agents: []config.Agent{agent}, Workspace: config.Workspace{Provider: "claude"}}
			done := make(chan allocDecision, 1)
			go func() { done <- f.decide() }()
			select {
			case d := <-done:
				refusals := 0
				for _, r := range d.Trace {
					if strings.Contains(r.Reason, cause) {
						refusals++
					}
				}
				if len(d.Plans) != 0 || refusals != 1 {
					t.Errorf("%s, max %d: plans %v, %d refusals; want no plan and one refused slot: trace %v", cause, limit, planSlots(d, "worker"), refusals, d.Trace)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s, max %d: the pass did not terminate", cause, limit)
			}
		}
	}
}

// Kills: an identity lease treated as name-specific whatever its holder
// (F3, M13). A lease a dead or absent row holds frees nothing soon but
// blocks only that name, so the request moves to the next slot; one a live
// row holds, or a row whose liveness is unknown, stalls the request, as
// legacy does.
func TestAllocator_IdentityLeaseAdvancesOnlyPastANotLiveHolder(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	started := ago(time.Minute)
	canonical := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started)
	stale := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started,
		"agent_name", "worker-2", "pool_slot", "")
	for holder, want := range map[string]string{"absent": "[3]", "alive": "[]", "unknown": "[]"} {
		f := newAllocFixture(t, cfg).sessions(canonical).rigLeg(stale).demand("worker", "w-1", "w-2")
		switch holder {
		case "alive":
			f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
		case "unknown":
			f.noInventory = true
		}
		d := f.decide()
		if got := fmt.Sprint(planSlots(d, "worker")); got != want || !traceHas(d, gateIdentityLease) {
			t.Errorf("holder %s: plans %s trace %v, want %s", holder, got, d.Trace, want)
		}
	}
}

// singletonRuntimeName is the runtime name a canonical singleton's create
// would claim, derived as the create effect derives it.
func singletonRuntimeName(t *testing.T, cfg *config.City, template string) string {
	t.Helper()
	ids, err := derivePoolSessionIdentifiers(cfg, template, poolSessionCreateIdentity{AgentName: template}, "")
	if err != nil {
		t.Fatal(err)
	}
	return ids.sessionName
}

// Kills: a provider probe in the pass, and unknown read as free (POOL-052,
// C7.3). The planned singleton name is read from I3, including a runtime no
// census row owns: a corpse frees it, a zombie and unknown hold it.
func TestAllocator_SingletonRuntimeOccupiedFromObservation(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("solo", 1)}}
	name := singletonRuntimeName(t, cfg, "solo")
	cases := []struct {
		label string
		setup func(*allocFixture)
		plan  bool
		cause string
	}{
		{"absent", func(*allocFixture) {}, true, ""},
		{"corpse", func(f *allocFixture) { f.corpse(name) }, true, ""},
		{"alive-unowned", func(f *allocFixture) { f.alive(name, InventoryAttrs{OwnerState: OwnerNone}) }, false, gateNameOccupied},
		{"zombie", func(f *allocFixture) { f.alive(name, InventoryAttrs{}).fact(name, FactProcessAlive, ObsNo) }, false, gateNameOccupied},
		{"unknown", func(f *allocFixture) { f.noInventory = true }, false, gateLivenessUnknown},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg).demand("solo", "w-1")
		tc.setup(f)
		d := f.decide()
		if got := len(d.Plans) == 1; got != tc.plan {
			t.Errorf("%s: plan = %v, want %v (trace %v)", tc.label, got, tc.plan, d.Trace)
		}
		if tc.cause != "" && !traceHas(d, tc.cause) {
			t.Errorf("%s: trace %v, want %s", tc.label, d.Trace, tc.cause)
		}
	}
}

// Kills: a singleton's #46 quarantine read under its runtime name instead
// of its episode key (startupHealthEpisodeKey: the identity name with the
// -pool suffix, which a non-transient singleton's runtime name lacks).
func TestAllocator_SingletonQuarantineByEpisodeKey(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("solo", 1)}}
	key := boundSessionNameLength(poolIdentitySessionName("solo", "solo") + poolRuntimeNameSuffix)
	if key == singletonRuntimeName(t, cfg, "solo") {
		t.Fatal("fixture: the episode key equals the runtime name; the test proves nothing")
	}
	f := newAllocFixture(t, cfg).demand("solo", "w-1")
	f.in.Episodes = map[string]session.StartupHealthEpisode{key: {QuarantinedUntil: allocNow.Add(time.Minute)}}
	if d := f.decide(); len(d.Plans) != 0 || !traceHas(d, gateQuarantine) {
		t.Fatalf("quarantined singleton: plans %+v trace %v", d.Plans, d.Trace)
	}
}

// Kills: a create storm on held identities (R-43, F8). A later-leg copy of
// a pending row still holds its old identity spelling; the slot it names is
// free in the canonical census, so only the lease check stops the create.
func TestAllocator_IdentityLeaseHeldPlansNoCreate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 2)}}
	started := ago(time.Minute)
	canonical := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started)
	stale := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started,
		"agent_name", "worker-2", "pool_slot", "")
	d := newAllocFixture(t, cfg).sessions(canonical).rigLeg(stale).demand("worker", "w-1", "w-2").decide()
	for _, p := range d.Plans {
		if p.Plan.qualifiedInstance == "worker-2" {
			t.Fatalf("planned a create for the leased identity worker-2: %+v", d.Plans)
		}
	}
	if !traceHas(d, gateIdentityLease) {
		t.Fatalf("trace %v, want identity-lease-held", d.Trace)
	}
}

// R-43: nothing plans the identity 43 stale creates and a live owner hold;
// the owner keeps its wake.
func TestAllocator_FortyThreeRowsShareANamePlanNothingForIt(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	rows := []beads.Bead{poolRow("gc-owner", "worker", 2, "active", "session_name", "worker--2-pool")}
	for i := 0; i < 43; i++ {
		rows = append(rows, poolRow(fmt.Sprintf("gc-stale-%02d", i), "worker", 2, "creating", "session_name", "worker--2-pool",
			"pending_create_claim", "true", "pending_create_started_at", ago(time.Hour)))
	}
	d := newAllocFixture(t, cfg).sessions(rows...).
		alive("worker--2-pool", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-owner"}).
		demand("worker", "w-1").decide()
	if e := entryOf(t, d, "gc-owner"); e.Desired != desireWake || e.Liveness != livenessAlive {
		t.Fatalf("live owner = %s (%s), want wake", e.Desired, e.Liveness)
	}
	for _, p := range d.Plans {
		if p.Plan.poolSlot == 2 {
			t.Fatalf("planned slot 2 while 44 rows hold it: %+v", d.Plans)
		}
	}
}

// Kills: reusing doomed rows, and recreating held demand (F8). A pending
// create past its never-started lease is a rollback candidate the pass
// plans around; one whose endpoint still holds pending creates is queued
// demand that keeps its row and wakes as pending-create; one within its
// lease is in-flight demand that keeps its row (POOL-028).
func TestAllocator_StalePendingCreateIsRollbackCandidateUnlessBreakerHolds(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}, Workspace: config.Workspace{Provider: "claude"}}
	run := func(started time.Duration, holds bool) allocDecision {
		row := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(started))
		f := newAllocFixture(t, cfg).sessions(row).demand("worker", "w-1")
		f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed, HoldsPendingCreate: holds}}
		return f.decide()
	}
	d := run(30*time.Minute, false)
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonRollbackCandidate {
		t.Fatalf("expired pending create = %s/%s, want rollback candidate", e.Desired, e.Reason)
	}
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[2]" {
		t.Fatalf("plans = %v, want [2]: plan around the candidate, whose slot stays held", got)
	}
	for label, held := range map[string]allocDecision{"breaker-held": run(30*time.Minute, true), "in-lease": run(time.Minute, false)} {
		// Stage 1d's scaled:creating overwrites pending-create, as legacy's
		// merge does; either is the queued create waking.
		if e := entryOf(t, held, "gc-1"); e.Desired != desireWake || !e.InDesired ||
			(e.Reason != "pending-create" && e.Reason != "scaled:creating") {
			t.Fatalf("%s pending create = %s/%s indesired=%v, want an InDesired create wake", label, e.Desired, e.Reason, e.InDesired)
		}
		if len(held.Plans) != 0 {
			t.Fatalf("%s: recreated in-flight demand: %+v", label, held.Plans)
		}
	}
}

// Kills: a slot freed by an unknown-state row (F9).
func TestAllocator_UnknownStateRowKeepsItsSlot(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	d := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "gc_swept")).demand("worker", "w-1").decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[2]" {
		t.Fatalf("plans = %v, want [2]", got)
	}
}

// Kills: duplicate creates while creates are in flight (POOL-028/029, C5.13
// row 1: in-flight demand counts census ∪ ledger). Pass 1 plans two creates;
// P3-5b reserves them; pass 2 runs before either row reaches the census and
// plans nothing more, with or without a pool max. Once one row shows (by its
// token) and demand grows by one, the next pass counts the row once and
// plans only the new work.
func TestAllocator_InFlightCreate_CountsCacheUnionLedger(t *testing.T) {
	unlimited := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	for label, agent := range map[string]config.Agent{"max-10": allocPoolAgent("worker", 10), "no-max": unlimited} {
		cfg := &config.City{Agents: []config.Agent{agent}}
		f := newAllocFixture(t, cfg).demand("worker", "w-1", "w-2")
		first := f.decide()
		if got := planWork(first); fmt.Sprint(got) != "[w-1 w-2]" {
			t.Fatalf("%s: pass 1 plans %v", label, got)
		}
		reserveAll(f, first, allocNow)
		second := f.decide()
		if len(second.Plans) != 0 {
			t.Fatalf("%s: pass 2 replanned in-flight creates: %v", label, planWork(second))
		}
		if got := second.Snapshot.PoolDesired["worker"]; got != 2 {
			t.Fatalf("%s: pass 2 PoolDesired = %d, want the 2 in flight", label, got)
		}

		// One create lands: its row carries its token; demand grows.
		landed := f.in.Reservations[0]
		row := poolRow("gc-new", "worker", landed.Slot, "start-pending", "pending_create_claim", "true",
			"pending_create_started_at", ago(5*time.Second), "instance_token", "tok-"+landed.EntryID,
			"gc.trigger_bead_id", landed.WorkBeadID)
		f.legs = nil
		f.sessions(row).demand("worker", "w-1", "w-2", "w-3")
		third := f.decide()
		if got := planWork(third); fmt.Sprint(got) != "[w-3]" {
			t.Fatalf("%s: pass 3 plans %v, want only w-3 (trace %v)", label, got, third.Trace)
		}
		if got := third.Snapshot.PoolDesired["worker"]; got != 3 {
			t.Fatalf("%s: pass 3 PoolDesired = %d, want 3 (the landed row counted once)", label, got)
		}
	}
}

// Kills: an in-flight create's trigger work planned again (C5.13 row 1): the
// stand-in carries its work, so POOL-030 matches it there, and with a demand
// that lists another item first the plan takes that item, not the in-flight
// one.
func TestAllocator_InFlightCreateKeepsItsWork(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	f := newAllocFixture(t, cfg).demand("worker", "w-2")
	reserveAll(f, f.decide(), allocNow)
	f.demand("worker", "w-1", "w-2")
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-1]" {
		t.Fatalf("plans carry %v, want [w-1]: w-2's create is in flight", got)
	}
}

// Kills: an in-flight create dropped from the demand floor (POOL-028): two
// creates are in flight and the demand has shrunk to one of their items (the
// other was claimed elsewhere); both creates keep their place, whatever stamp
// their entries carry.
func TestAllocator_InFlightCreateHoldsTheDemandFloor(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	for label, at := range map[string]time.Time{"now": allocNow, "unstamped": {}, "future": allocNow.Add(time.Hour)} {
		f := newAllocFixture(t, cfg).demand("worker", "w-1")
		f.in.Reservations = []planReservation{
			{EntryID: "c-1", Template: "worker", QualifiedInstance: "worker-1", Slot: 1, WorkBeadID: "w-1", ReservedAt: at},
			{EntryID: "c-2", Template: "worker", QualifiedInstance: "worker-2", Slot: 2, WorkBeadID: "w-2", ReservedAt: at},
		}
		d := f.decide()
		if got := d.Snapshot.PoolDesired["worker"]; got != 2 || len(d.Plans) != 0 {
			t.Errorf("%s: PoolDesired = %d plans %+v, want the 2 in flight and no plan", label, got, d.Plans)
		}
	}
}

// Kills: a named create's reservation counted as pool demand: it reserves its
// identity, but its backing pool's demand still plans.
func TestAllocator_NamedReservationIsNoPoolStandIn(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{allocPoolAgent("worker", 5)},
		NamedSessions: []config.NamedSession{{Name: "boss", Template: "worker", Mode: "on_demand"}},
	}
	f := newAllocFixture(t, cfg).demand("worker", "w-1")
	f.in.Reservations = []planReservation{{EntryID: "c-1", Template: "worker", NamedIdentity: "boss", SessionName: "boss", ReservedAt: allocNow}}
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-1]" {
		t.Fatalf("plans carry %v, want [w-1]", got)
	}
}

// Kills: a stale sibling counted as pool capacity (POOL-082): work assigned
// by session name matches the sibling too, and counted as its resume
// request it would take a cap.
func TestAllocator_StaleSiblingTakesNoPoolCapacity(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-a", "worker", 1, "active", "session_name", "shared"),
		poolRow("gc-b", "worker", 2, "active", "session_name", "shared"),
	).alive("shared", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-a"})
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "shared", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	d := f.decide()
	if got := d.Snapshot.PoolDesired["worker"]; got != 1 {
		t.Fatalf("PoolDesired = %d, want 1: only the owner resumes the work", got)
	}
	// New demand is not served by the sibling: it cannot start under a name
	// another runtime holds.
	f.demand("worker", "w-2")
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-2]" {
		t.Fatalf("plans carry %v, want [w-2]: the stale sibling is no capacity", got)
	}
}

// Kills: a named scale-check partial retaining its template's other rows
// (POOL-037): the template is marked, not retained, so a plain row of it is
// decided, not kept.
func TestAllocator_NamedScaleCheckPartialMarksWithoutRetaining(t *testing.T) {
	cfg := chatCity("on_demand")
	f := newAllocFixture(t, cfg).sessions(sessionRow("gc-2", "template", "chat", "state", "active", "session_name", "s-gc-2")).
		alive("s-gc-2", InventoryAttrs{AttachedKnown: true})
	f.in.Demand.Collected.NamedPartials = map[string]bool{"chat": true}
	d := f.decide()
	if tp := d.Snapshot.Partial.Templates["chat"]; tp.Retain || tp.BlockCreate || !slices.Contains(tp.Causes, "named-scale-check-partial") {
		t.Fatalf("chat partial = %+v, want marked only", tp)
	}
	if e := entryOf(t, d, "gc-2"); e.Desired == desireKeep {
		t.Fatalf("plain row of the template = %s/%s, want it decided, not retained", e.Desired, e.Reason)
	}
}

// Kills: a canonical singleton recreated while its create is in flight.
func TestAllocator_InFlightSingletonCreateNotReplanned(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("olivia", 1)}}
	f := newAllocFixture(t, cfg).demand("olivia", "w-1")
	first := f.decide()
	if len(first.Plans) != 1 {
		t.Fatalf("pass 1 plans %+v", first.Plans)
	}
	reserveAll(f, first, allocNow.Add(-time.Second))
	if second := f.decide(); len(second.Plans) != 0 {
		t.Fatalf("pass 2 replanned the singleton: %+v (trace %v)", second.Plans, second.Trace)
	}
}

// Kills: a sticky binding not fed into planning (C6.3, C6.6, POOL-030). Pass
// 1 binds the not-alive row X to w-1. In pass 2 demand is [w-0, w-1]: X
// keeps w-1 under the same binding, and the plan carries w-0, never w-1.
// Both an in-flight row (a concrete request) and a plain reusable one.
func TestAllocator_StickyBindingFeedsPlanning(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	for label, x := range map[string]beads.Bead{
		"in-flight": poolRow("gc-x", "worker", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", ago(10*time.Second)),
		"reusable":  poolRow("gc-x", "worker", 1, "active"),
	} {
		d1 := newAllocFixture(t, cfg).sessions(x).demand("worker", "w-1").decide()
		e1 := entryOf(t, d1, "gc-x")
		if e1.Binding == nil || e1.Binding.WorkBeadID != "w-1" || len(d1.Plans) != 0 {
			t.Fatalf("%s pass 1: binding %+v plans %+v", label, e1.Binding, d1.Plans)
		}
		f := newAllocFixture(t, cfg).sessions(x).demand("worker", "w-0", "w-1")
		f.in.Prev, f.in.SelGen = d1.Snapshot, 2
		d2 := f.decide()
		if e2 := entryOf(t, d2, "gc-x"); e2.Binding == nil || *e2.Binding != *e1.Binding {
			t.Fatalf("%s pass 2: binding %+v, want the sticky %+v", label, e2.Binding, e1.Binding)
		}
		if got := planWork(d2); fmt.Sprint(got) != "[w-0]" {
			t.Fatalf("%s pass 2: plans carry %v, want [w-0]", label, got)
		}
	}
}

// Kills: trigger flip churn and double assignment (AM2, C6.3, C6.6). A
// selected start candidate is bound to its request's work, with an ID that
// carries the epoch; a live row never is. A binding published for a row
// still selected keeps its work and ID on the next pass even when demand
// arrives in another order, is dropped once the work is no longer demand,
// and no work item is bound twice.
func TestAllocator_BindingStickyWhileSelectedAndNeverForLiveRows(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	rows := []beads.Bead{
		poolRow("gc-1", "worker", 1, "active", "gc.trigger_bead_id", "w-old"),
		poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-old2"),
	}
	first := newAllocFixture(t, cfg).sessions(rows...).alive("s-gc-2", InventoryAttrs{}).demand("worker", "w-1", "w-2").decide()
	dead, live := entryOf(t, first, "gc-1"), entryOf(t, first, "gc-2")
	if live.Binding != nil {
		t.Fatalf("a live row was bound: %+v", live.Binding)
	}
	if dead.Binding == nil || (dead.Binding.WorkBeadID != "w-1" && dead.Binding.WorkBeadID != "w-2") ||
		dead.Binding.ID != fmt.Sprintf("bind:e1:%s/gc-1:%s@1", allocSessionsLeg, dead.Binding.WorkBeadID) {
		t.Fatalf("dead row binding = %+v, want one of the demand items under an epoch-scoped ID", dead.Binding)
	}

	f := newAllocFixture(t, cfg).sessions(rows...).alive("s-gc-2", InventoryAttrs{}).demand("worker", "w-2", "w-1")
	f.in.Prev, f.in.SelGen = first.Snapshot, 2
	second := f.decide()
	if got := entryOf(t, second, "gc-1").Binding; got == nil || *got != *dead.Binding {
		t.Fatalf("binding churned: %+v -> %+v", dead.Binding, got)
	}

	f = newAllocFixture(t, cfg).sessions(rows...).alive("s-gc-2", InventoryAttrs{}).demand("worker", "w-9")
	f.in.Prev, f.in.SelGen = first.Snapshot, 3
	third := f.decide()
	if got := entryOf(t, third, "gc-1").Binding; got != nil && got.WorkBeadID == dead.Binding.WorkBeadID {
		t.Fatalf("binding to finished work kept: %+v", got)
	}

	// Two dead rows, one work item. The previous snapshot bound w-1 to
	// gc-2, which is still selected: gc-2 keeps it under its old ID and
	// gc-1 gets none.
	f = newAllocFixture(t, cfg).sessions(rows...).demand("worker", "w-1")
	f.in.SelGen = 4
	f.in.Prev = &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
		{allocSessionsLeg, "gc-2"}: {Binding: &bindingTarget{ID: "bind:prev", WorkBeadID: "w-1", WorkStoreRef: "city"}},
	}}
	both := f.decide()
	bound := 0
	for _, id := range []string{"gc-1", "gc-2"} {
		if b := entryOf(t, both, id).Binding; b != nil && b.WorkBeadID == "w-1" {
			bound++
		}
	}
	if bound != 1 || entryOf(t, both, "gc-2").Binding == nil || entryOf(t, both, "gc-2").Binding.ID != "bind:prev" {
		t.Fatalf("w-1 bound to %d rows (gc-2 %+v), want once, to gc-2 under its old ID", bound, entryOf(t, both, "gc-2").Binding)
	}
}

// Kills: the C6.6 consumption rules dropped (C6.3, C6.6, R4): work a selected
// live row carries as its trigger is bound to no other row, whatever the
// previous snapshot bound; a pairing already applied (the row's trigger is
// the work) publishes no binding but still holds the work; a row that now
// resumes its own claimed work drops its old binding.
func TestAllocator_BindingConsumptionRules(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	prevBound := func(id, work string) *selectionSnapshot {
		return &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
			{allocSessionsLeg, id}: {Binding: &bindingTarget{ID: "bind:prev", WorkBeadID: work, WorkStoreRef: "city"}},
		}}
	}

	// Live gc-2 carries w-1; the previous snapshot bound w-1 to dead gc-1.
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-1"),
	).alive("s-gc-2", InventoryAttrs{AttachedKnown: true}).demand("worker", "w-1", "w-2")
	f.in.Prev = prevBound("gc-1", "w-1")
	d := f.decide()
	if b := entryOf(t, d, "gc-1").Binding; b != nil && b.WorkBeadID == "w-1" {
		t.Errorf("w-1 bound to gc-1 while live gc-2 carries it: %+v", b)
	}
	if b := entryOf(t, d, "gc-2").Binding; b != nil {
		t.Errorf("live row bound: %+v", b)
	}

	// No previous snapshot: realization pairs w-1 with live gc-2, which
	// carries it, so dead gc-1 serves w-2 rather than taking w-1 and losing
	// its binding to the live row's claim.
	f = newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-1"),
	).alive("s-gc-2", InventoryAttrs{AttachedKnown: true}).demand("worker", "w-1", "w-2")
	d = f.decide()
	if b := entryOf(t, d, "gc-1"); b.Binding == nil || b.Binding.WorkBeadID != "w-2" {
		t.Errorf("gc-1 binding = %+v, want w-2 (w-1 is the live row's)", b.Binding)
	}

	// Applied: gc-1's trigger is the work its previous binding named.
	f = newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active", "gc.trigger_bead_id", "w-1"),
		poolRow("gc-2", "worker", 2, "active"),
	).demand("worker", "w-1", "w-2")
	f.in.Prev = prevBound("gc-1", "w-1")
	d = f.decide()
	if b := entryOf(t, d, "gc-1").Binding; b != nil {
		t.Errorf("applied pairing republished: %+v", b)
	}
	if b := entryOf(t, d, "gc-2").Binding; b == nil || b.WorkBeadID != "w-2" {
		t.Errorf("gc-2 binding = %+v, want w-2 (w-1 is held by gc-1)", b)
	}

	// Resume: gc-1 now holds claimed work w-7; its old binding to w-1 goes.
	f = newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).demand("worker", "w-1")
	f.in.Prev = prevBound("gc-1", "w-1")
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-7", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	d = f.decide()
	if b := entryOf(t, d, "gc-1").Binding; b != nil && b.WorkBeadID == "w-1" {
		t.Errorf("resuming row kept its old binding: %+v", b)
	}
}

// Kills: a binding for a row that is not a start candidate (AM2), fresh or
// carried over from the previous snapshot, and a live row's selection
// dropped for a refused worktree (owner decision at P3-5a review: keep the
// selection, drop only the binding).
func TestAllocator_BindingsOnlyForStartCandidates(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 1)}}
	prev := &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
		{allocSessionsLeg, "gc-1"}: {Binding: &bindingTarget{ID: "bind:prev", WorkBeadID: "w-1", WorkStoreRef: "city"}},
	}}
	for label, setup := range map[string]func(*allocFixture){
		"unknown":      func(f *allocFixture) { f.noInventory = true },
		"unknown-prev": func(f *allocFixture) { f.noInventory, f.in.Prev = true, prev },
		"alive-prev":   func(f *allocFixture) { f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).in.Prev = prev },
	} {
		f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).demand("worker", "w-1")
		setup(f)
		if e := entryOf(t, f.decide(), "gc-1"); !e.InDesired || e.Binding != nil {
			t.Errorf("%s: indesired=%v binding %+v, want selected, unbound", label, e.InDesired, e.Binding)
		}
	}

	spec := worktree.Spec{BeadID: "w-1", StoreRef: "city", Path: "/wt/w-1"}
	withSpec := func(f *allocFixture) *allocFixture {
		d0 := f.in.Demand.Collected.DefaultDemand["worker"]
		d0.WorktreeSpecs = map[string]*worktree.Spec{"w-1": &spec}
		f.in.Demand.Collected.DefaultDemand["worker"] = d0
		f.in.Backoff = workRefusal(spec)
		return f
	}
	live := withSpec(newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).demand("worker", "w-1"))
	if e := entryOf(t, live.decide(), "gc-1"); !e.InDesired || e.Desired != desireWake || e.Binding != nil {
		t.Errorf("live row with a refused worktree = indesired=%v %s binding %+v, want a selected wake, unbound", e.InDesired, e.Desired, e.Binding)
	}
	dead := withSpec(newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).demand("worker", "w-1"))
	d := dead.decide()
	if e := entryOf(t, d, "gc-1"); e.InDesired || e.Binding != nil || !traceHas(d, gateWorktreeRefused) {
		t.Errorf("start candidate with a refused worktree = indesired=%v binding %+v trace %v, want unselected, traced", e.InDesired, e.Binding, d.Trace)
	}
}

// Kills: a named session resolving to whichever claimant sorts first
// (P3-1 obligation). The loser sorts first; the winner is the row selected.
func TestAllocator_NamedResolvesToVerdictWinnerNotFirstClaimant(t *testing.T) {
	d := newAllocFixture(t, chatCity("always")).sessions(chatRow("gc-1", "1"), chatRow("gc-9", "4")).decide()
	// Selected as the spec's canonical row (alias and instance are the
	// identity), not merely kept by the overlay.
	if e := entryOf(t, d, "gc-9"); !e.InDesired || e.Desired != desireWake || e.Config == nil ||
		e.Config.NamedIdentity != "chat" || e.Config.Alias != "chat" || e.Config.InstanceName != "chat" {
		t.Fatalf("winner = %s indesired=%v cfg=%+v, want the selected canonical wake", e.Desired, e.InDesired, e.Config)
	}
	if len(d.Plans) != 0 {
		t.Fatalf("plans %+v: the identity has a canonical row", d.Plans)
	}
}

// namedRuntimeName is spec.SessionName for the named session "chat".
func namedRuntimeName(t *testing.T, cfg *config.City) string {
	t.Helper()
	spec, ok := findNamedSessionSpec(cfg, "city", "chat")
	if !ok {
		t.Fatalf("no named spec %q", "chat")
	}
	return spec.SessionName
}

// Kills: a probe reintroduced, adopting an attributed runtime, and the S3
// storm or stall (AM-N6, P3-6b §2.2): every row of the occupancy table.
func TestAllocator_NamedPlan_OccupancyFromObservation(t *testing.T) {
	cfg := chatCity("always")
	name := namedRuntimeName(t, cfg)
	cases := []struct {
		label string
		setup func(*allocFixture)
		plan  bool
		adopt bool
		cause string
	}{
		{"unknown", func(f *allocFixture) { f.noInventory = true }, false, false, gateLivenessUnknown},
		{"absent", func(*allocFixture) {}, true, false, ""},
		{"corpse", func(f *allocFixture) { f.corpse(name) }, true, false, ""},
		{"zombie", func(f *allocFixture) { f.alive(name, InventoryAttrs{}).fact(name, FactProcessAlive, ObsNo) }, true, false, ""},
		{"alive-ownerless", func(f *allocFixture) { f.alive(name, InventoryAttrs{OwnerState: OwnerNone}) }, true, true, ""},
		{"alive-unattributable", func(f *allocFixture) { f.alive(name, InventoryAttrs{}) }, true, true, ""},
		{"alive-attribution-pending", func(f *allocFixture) { f.alive(name, InventoryAttrs{Incarnation: "i-1"}) }, false, false, gateOwnerPending},
		{"alive-owned", func(f *allocFixture) { f.alive(name, InventoryAttrs{OwnerState: OwnerSession, OwnerID: "gc-closed"}) }, false, false, gateNameHeld + "gc-closed"},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg)
		tc.setup(f)
		d := f.decide()
		if got := len(d.Plans) == 1; got != tc.plan {
			t.Errorf("%s: plan = %v, want %v (trace %v)", tc.label, got, tc.plan, d.Trace)
			continue
		}
		if tc.plan {
			p := d.Plans[0]
			if p.Kind != createNamed || p.Named == nil || p.Named.SessionName != name || p.Named.AdoptLive != tc.adopt {
				t.Errorf("%s: plan = %+v %+v, want adopt=%v", tc.label, p, p.Named, tc.adopt)
			}
		}
		if tc.cause != "" && !traceHas(d, tc.cause) {
			t.Errorf("%s: trace %v, want %s", tc.label, d.Trace, tc.cause)
		}
	}
}

// Kills: a named create under a C7.4 gate (P3-6b obligations): an incomplete
// census, the backing template's BlockCreate, provider red, a #46 quarantine
// of its session name and a shut endpoint each refuse it, traced.
func TestAllocator_NamedPlan_PlanTimeGates(t *testing.T) {
	cfg := chatCity("always")
	cfg.Workspace.Provider = "claude"
	name := namedRuntimeName(t, cfg)
	open := map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
	cases := []struct {
		cause string
		setup func(*allocFixture)
	}{
		{"", func(*allocFixture) {}},
		{gateCensusIncomplete, func(f *allocFixture) {
			f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: censusErrStore{Store: censusStore(), err: errors.New("rig down")}})
		}},
		{gateBlockCreate, func(f *allocFixture) {
			f.in.Demand.CustomCheckTemplates = []string{"chat"} // no lane result: partial
		}},
		{gateProviderRed, func(f *allocFixture) {
			f.in.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{"claude": false}}
		}},
		{gateQuarantine, func(f *allocFixture) {
			f.in.Episodes = map[string]session.StartupHealthEpisode{name: {QuarantinedUntil: allocNow.Add(time.Minute)}}
		}},
		{gateEndpointShut, func(f *allocFixture) {
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateShut}}
		}},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg).sessions()
		f.in.Endpoints = open
		tc.setup(f)
		d := f.decide()
		if tc.cause == "" {
			if !hasNamedPlan(d) {
				t.Errorf("control: no named plan (trace %v)", d.Trace)
			}
			continue
		}
		if hasNamedPlan(d) || !traceHas(d, tc.cause) {
			t.Errorf("%s: plans %+v trace %v", tc.cause, d.Plans, d.Trace)
		}
	}
}

// Kills: duplicate creates across passes (P3-6b N10). An uncleared create
// entry's planning reservation holds the identity.
func TestAllocator_NamedPlan_OnePerIdentityWhileEntryUncleared(t *testing.T) {
	cfg := chatCity("always")
	f := newAllocFixture(t, cfg)
	f.in.Reservations = []planReservation{{EntryID: "create-1", Template: "chat", NamedIdentity: "chat", SessionName: namedRuntimeName(t, cfg)}}
	d := f.decide()
	if len(d.Plans) != 0 || !traceHas(d, gateInFlight) {
		t.Fatalf("plans %+v trace %v: one create per identity while its entry is uncleared", d.Plans, d.Trace)
	}
}

// Kills: legacy predicate drift (POOL-039..042, P3-6b §3.1): an
// identity-first canonical row plans nothing, a conflicting holder plans
// nothing, an on_demand session without work plans nothing, a pool-slot
// shaped identity is never planned, and a live create backoff refuses it.
func TestAllocator_NamedPlan_CanonicalAndConflictGates(t *testing.T) {
	named := func(mode string) *config.City {
		return &config.City{
			Agents:        []config.Agent{{Name: "chat"}, allocPoolAgent("worker", 3)},
			NamedSessions: []config.NamedSession{{Template: "chat", Mode: mode}},
		}
	}
	d := newAllocFixture(t, named("always")).sessions(chatRow("gc-1", "1", "session_name", "renamed")).decide()
	if len(d.Plans) != 0 || !entryOf(t, d, "gc-1").InDesired {
		t.Errorf("identity-first canonical: plans %+v", d.Plans)
	}
	d = newAllocFixture(t, named("always")).sessions(sessionRow("gc-2", "template", "worker", "state", "active",
		"session_name", "s-gc-2", "alias", "chat")).decide()
	if len(d.Plans) != 0 || !traceHas(d, gateNamedConflict) {
		t.Errorf("conflicting holder: plans %+v trace %v", d.Plans, d.Trace)
	}
	if d = newAllocFixture(t, named("on_demand")).decide(); len(d.Plans) != 0 {
		t.Errorf("on_demand without work planned: %+v", d.Plans)
	}
	f := newAllocFixture(t, named("on_demand"))
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-7", Status: "in_progress", Assignee: "chat"}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	if d = f.decide(); len(d.Plans) != 1 || d.Plans[0].Named.BoundStepID != "w-7" {
		t.Errorf("on_demand with work: plans %+v", d.Plans)
	}
	slotShaped := &config.City{
		Agents:        []config.Agent{allocPoolAgent("worker", 3)},
		NamedSessions: []config.NamedSession{{Name: "worker-2", Template: "worker", Mode: "always"}},
	}
	if d = newAllocFixture(t, slotShaped).decide(); hasNamedPlan(d) || !traceHas(d, gatePoolSlotShaped) {
		t.Errorf("pool-slot shaped identity: plans %+v trace %v", d.Plans, d.Trace)
	}
	f = newAllocFixture(t, named("always"))
	f.in.Backoff = createRefusal("named:chat", createStageFence, allocNow.Add(time.Minute))
	if d = f.decide(); len(d.Plans) != 0 || !traceHas(d, gateCreateRefused+"fence") {
		t.Errorf("refused identity: plans %+v trace %v", d.Plans, d.Trace)
	}
}

// A named plan reserves its identity before pool planning, so a canonical
// singleton pool of the same name is refused in the same pass (P3-6b §3.1).
func TestAllocator_NamedPlanRefusesSameIdentityPoolSingleton(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{allocPoolAgent("olivia", 1)},
		NamedSessions: []config.NamedSession{{Template: "olivia", Mode: "always"}},
	}
	d := newAllocFixture(t, cfg).demand("olivia", "w-1").decide()
	if !hasNamedPlan(d) {
		t.Fatalf("no named plan: %+v trace %v", d.Plans, d.Trace)
	}
	for _, p := range d.Plans {
		if p.Kind == createPool {
			t.Fatalf("pool singleton planned beside the named identity: %+v", d.Plans)
		}
	}
}

// controlGapFixture is a city with a city dispatcher only, and an open
// control row owned by rig "fixture", routed to the city dispatcher: a
// scope with no dispatcher (P3-2's gap fixture). The default probe counted
// it for the dispatcher from a Ready read that kept the stale route.
func controlGapFixture(t *testing.T) (*allocFixture, string) {
	t.Helper()
	cfg := cityOnlyDispatcherFixtureConfig(t)
	dispatcher := cfg.Agents[0].QualifiedName()
	row := beads.Bead{ID: "gcg-ctl", Title: "ctl", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
		beadmeta.RoutedToMetadataKey:     dispatcher,
		beadmeta.RootStoreRefMetadataKey: "rig:fixture",
	}}
	f := newAllocFixture(t, cfg)
	f.in.Demand.Collected = collectedDemand{
		DefaultProbed:        true,
		DefaultCounts:        map[string]int{dispatcher: 1},
		DefaultDemand:        map[string]scaleCheckDemand{dispatcher: {Count: 1, WorkBeadIDs: []string{row.ID}, StoreRefs: map[string]string{row.ID: "rig:fixture"}}},
		UnassignedRouted:     []beads.Bead{row},
		UnassignedRoutedRefs: []string{"rig:fixture"},
	}
	return f, dispatcher
}

// Kills: control work counted that legacy's in-tick repair suppresses (P3-2
// obligation), a projection applied to one consumer only (P3-2 re-review),
// and the default probe counting the suppressed route (mc-zndi7.41, fixed
// v2-only: an explained difference for P3-5c). The projection runs once,
// and control demand, the ready routed work and the default probe all read
// its rows.
func TestAllocator_ControlRoutesProjectedForEveryConsumer(t *testing.T) {
	f, dispatcher := controlGapFixture(t)
	if got := openControlDispatcherDemand(f.in.Cfg, f.in.Demand.Collected.UnassignedRouted); !got[dispatcher] {
		t.Fatalf("fixture: the unprojected row counts no control demand (%v); the test would prove nothing", got)
	}
	d := f.decide()
	if got := d.Snapshot.PoolDesired[dispatcher]; got != 0 {
		t.Fatalf("PoolDesired[%s] = %d, want 0: a gap's route is not demand", dispatcher, got)
	}
	if len(d.Plans) != 0 {
		t.Fatalf("plans for suppressed control work: %+v", d.Plans)
	}
	if len(d.ReadyRouted) != 0 {
		t.Fatalf("ready routed work carries the suppressed row: %+v", d.ReadyRouted)
	}
}

// Kills: an edit to anything the plan steps are handed: the routed rows the
// projection rewrites copy-on-write, the default-probe maps, the
// reservations, the ledger view and the previous snapshot. Two identical
// inputs are built; the pass runs on one; the two must stay deeply equal.
func TestAllocator_PlanNeverEditsItsInputs(t *testing.T) {
	build := func() allocInputs {
		in := purityInputs(t)
		gap, dispatcher := controlGapFixture(t)
		in.Cfg.Agents = append(in.Cfg.Agents, gap.in.Cfg.Agents...)
		// The fixture's rig lives in a fresh temp dir per build; pin it so
		// the two builds compare equal.
		in.Cfg.Rigs = slices.Clone(gap.in.Cfg.Rigs)
		in.Cfg.Rigs[0].Path = "/rigs/fixture"
		in.Demand.Collected.UnassignedRouted = gap.in.Demand.Collected.UnassignedRouted
		in.Demand.Collected.UnassignedRoutedRefs = gap.in.Demand.Collected.UnassignedRoutedRefs
		in.Demand.Collected.DefaultCounts[dispatcher] = 1
		in.Demand.Collected.DefaultDemand[dispatcher] = gap.in.Demand.Collected.DefaultDemand[dispatcher]
		in.Reservations = []planReservation{{EntryID: "c-1", Template: "worker", QualifiedInstance: "worker-4", Slot: 4, WorkBeadID: "w-4", ReservedAt: allocNow}}
		in.Ledger = []ledgerEntry{{ID: "c-1", Kind: kindCreate, Key: rowKey{Leg: allocSessionsLeg}, Template: "worker", State: ledgerReserved, ReservedAt: allocNow}}
		in.Prev = &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
			{allocSessionsLeg, "gc-2"}: {Binding: &bindingTarget{ID: "bind:prev", WorkBeadID: "w-2", WorkStoreRef: "city"}},
		}}
		return in
	}
	in, control := build(), build()
	mustDecide(t, in)
	if !reflect.DeepEqual(in, control) {
		t.Fatal("the decide edited its inputs")
	}
}

// Kills: a scale_check partial read for a template with no custom check
// (P3-3 obligation): every other template reads partial on the lane's
// result, so only the custom-check templates are asked.
func TestAllocator_ScaleCheckPartialOnlyForCustomCheckTemplates(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3), allocPoolAgent("custom", 3)}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "asleep"))
	f.in.Demand.CustomCheckTemplates = []string{"custom"}
	d := f.decide() // no lane result: custom is partial
	if _, partial := d.Snapshot.Partial.Templates["worker"]; partial {
		t.Fatalf("worker read partial with no custom check: %+v", d.Snapshot.Partial)
	}
	if !d.Snapshot.Partial.Templates["custom"].BlockCreate {
		t.Fatalf("custom with no lane result must read partial: %+v", d.Snapshot.Partial)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired != desireDrain {
		t.Fatalf("idle worker row = %s, want drain (not retained by another template's partial)", e.Desired)
	}
}

// A dead runtime (a remain-on-exit corpse) is a start candidate: the row
// wakes for its demand and the start path recycles the pane (P3-3
// obligation, §4.11's interim divergence from MAINT-031).
func TestAllocator_DeadRowIsAStartCandidate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 1)}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 0, "active", "agent_name", "worker", "pool_slot", "")).
		corpse("s-gc-1").demand("worker", "w-1")
	d := f.decide()
	e := entryOf(t, d, "gc-1")
	if e.Desired != desireWake || e.Liveness != livenessDead || !e.Liveness.startCandidate() || e.ObservationUncertain {
		t.Fatalf("dead row = %s/%s liveness=%s uncertain=%v, want a certain wake on a start candidate", e.Desired, e.Reason, e.Liveness, e.ObservationUncertain)
	}
	if len(d.Plans) != 0 {
		t.Fatalf("plans beside a reusable dead row: %+v", d.Plans)
	}
}

// Kills: a lease checked with an identity the create effect does not check
// (P3-3 obligations): a canonical singleton's lease is its template
// identity, and an aliased pool (not bead-scoped) holds no lease.
func TestAllocator_IdentityLeaseAskedOnlyForBeadScopedIdentities(t *testing.T) {
	solo := &config.City{Agents: []config.Agent{allocPoolAgent("solo", 1)}}
	d := newAllocFixture(t, solo).sessions(poolRow("gc-1", "solo", 0, "failed-create", "agent_name", "solo", "pool_slot", "")).
		demand("solo", "w-1").decide()
	if len(d.Plans) != 0 || !traceHas(d, gateIdentityLease) {
		t.Fatalf("singleton with a leased identity: plans %+v trace %v", d.Plans, d.Trace)
	}

	aliased := allocPoolAgent("worker", 3)
	aliased.TmuxAlias = "box"
	cfg := &config.City{Agents: []config.Agent{aliased}}
	d = newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "failed-create")).demand("worker", "w-1").decide()
	if traceHas(d, gateIdentityLease) || fmt.Sprint(planSlots(d, "worker")) != "[1]" {
		t.Fatalf("aliased pool: plans %v trace %v; its identity is no lease (the effect checks the alias instead)", planSlots(d, "worker"), d.Trace)
	}
}

// Kills: a create backoff ignored (AM-N8, P3-6 obligation) and a refused
// worktree's work bound or created (#34): the refused identity is traced,
// and a template-wide cause plans no other slot; the throttled work item
// takes no slot, while a record on other evidence for the same bead, or an
// expired one, refuses nothing.
func TestAllocator_CreateAndWorkBackoffRefusePlans(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).demand("worker", "w-1")
	key := createIdentity{Template: "worker", QualifiedInstance: "worker-1", Slot: 1}.key()
	f.in.Backoff = createRefusal(key, createStageLock, allocNow.Add(time.Minute))
	d := f.decide()
	if len(d.Plans) != 0 || !traceHas(d, gateCreateRefused+"lock") {
		t.Fatalf("refused identity: plans %v trace %v, want worker-1 refused and nothing planned", planSlots(d, "worker"), d.Trace)
	}
	f.in.Backoff = createRefusal(key, createStageLock, allocNow)
	if d = f.decide(); fmt.Sprint(planSlots(d, "worker")) != "[1]" {
		t.Fatalf("expired backoff: plans %v trace %v", planSlots(d, "worker"), d.Trace)
	}

	spec := worktree.Spec{BeadID: "w-1", StoreRef: "city", Path: "/wt/w-1"}
	f = newAllocFixture(t, cfg).demand("worker", "w-1")
	d0 := f.in.Demand.Collected.DefaultDemand["worker"]
	d0.WorktreeSpecs = map[string]*worktree.Spec{"w-1": &spec}
	f.in.Demand.Collected.DefaultDemand["worker"] = d0
	planned := f.decide()
	if len(planned.Plans) != 1 || planned.Plans[0].Plan.worktreeSpec == nil || *planned.Plans[0].Plan.worktreeSpec != spec {
		t.Fatalf("plan with worktree evidence = %+v, want it to carry the spec for the effect to verify (POOL-055)", planned.Plans)
	}
	other := spec
	other.Path = "/wt/elsewhere"
	f.in.Backoff = workRefusal(other)
	if d = f.decide(); len(d.Plans) != 1 {
		t.Fatalf("a record on other evidence refused the plan: %+v trace %v", d.Plans, d.Trace)
	}
	expired := workRefusal(spec)
	expired[workBackoffKey("w-1")] = backoffRecord{Until: allocNow, Fingerprint: specFingerprint(spec)}
	f.in.Backoff = expired
	if d = f.decide(); len(d.Plans) != 1 {
		t.Fatalf("an expired work record refused the plan: %+v trace %v", d.Plans, d.Trace)
	}
	f.in.Backoff = workRefusal(spec)
	refused := f.decide()
	if len(refused.Plans) != 0 || !traceHas(refused, gateWorktreeRefused) {
		t.Fatalf("refused worktree: plans %+v trace %v", refused.Plans, refused.Trace)
	}
}

// Kills: the planning census leaking planning reservations (P3-6
// obligation: createPass.planning holds census rows only).
func TestAllocator_PlanningCensusHoldsCensusRowsOnly(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).demand("worker", "w-1", "w-2", "w-3")
	f.in.Reservations = []planReservation{{EntryID: "create-9", Template: "worker", QualifiedInstance: "worker-2", Slot: 2, WorkBeadID: "w-2"}}
	d := f.decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[3]" {
		t.Fatalf("plans = %v, want [3]: slot 2 is reserved by an uncleared create", got)
	}
	if len(d.Planning) != 1 || d.Planning[0].ID != "gc-1" {
		t.Fatalf("planning census = %+v, want the census row only", d.Planning)
	}
}

// Kills: dependency floors that collide with the pass's own pool plans or an
// uncleared create (P3-1 obligation, F1), never plan (POOL-058), or plan for
// a suspended dependency. A root's dependency gets one floor plan; a pool
// plan for it in the same pass, or a create of it in flight, satisfies the
// floor.
func TestAllocator_DependencyFloorNeverCollidesWithAPassPlan(t *testing.T) {
	db := allocPoolAgent("db", 3)
	app := allocPoolAgent("app", 3)
	app.DependsOn = []string{"db"}
	cfg := &config.City{Agents: []config.Agent{db, app}}
	d := newAllocFixture(t, cfg).demand("app", "w-1").decide()
	var floor *allocPlan
	for i := range d.Plans {
		if d.Plans[i].Kind == createDependency {
			floor = &d.Plans[i]
		}
	}
	if floor == nil || floor.Template != "db" || floor.Plan.poolSlot != 1 {
		t.Fatalf("plans %+v, want a db dependency floor at slot 1", d.Plans)
	}
	d = newAllocFixture(t, cfg).demand("app", "w-1").demand("db", "w-2").decide()
	if got := planSlots(d, "db"); fmt.Sprint(got) != "[1]" {
		t.Fatalf("db plans = %v, want [1]: the pool plan satisfies the floor", got)
	}
	f := newAllocFixture(t, cfg).demand("app", "w-1")
	f.in.Reservations = []planReservation{{EntryID: "create-1", Template: "db", QualifiedInstance: "db-1", Slot: 1}}
	if got := planSlots(f.decide(), "db"); len(got) != 0 {
		t.Fatalf("db plans = %v while a create of db is in flight", got)
	}
	suspended := db
	suspended.Suspended = true
	d = newAllocFixture(t, &config.City{Agents: []config.Agent{suspended, app}}).demand("app", "w-1").decide()
	if got := planSlots(d, "db"); len(got) != 0 {
		t.Fatalf("a suspended dependency got a floor plan: %v", got)
	}
}

// Kills: a dependency floor planned again on every pass while its create is
// in flight (F1, R21): the root (app) is a live row the overlay selects, so
// pass 1 plans the db floor; once P3-5b reserves it, pass 2 plans nothing
// for db until the row lands. An in-flight pool create of db holds the
// floor the same way.
func TestAllocator_DependencyFloorInFlightNotReplanned(t *testing.T) {
	db := allocPoolAgent("db", 3)
	app := allocPoolAgent("app", 3)
	app.DependsOn = []string{"db"}
	cfg := &config.City{Agents: []config.Agent{db, app}}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-a", "app", 1, "active", "gc.trigger_bead_id", "w-1")).
		alive("s-gc-a", InventoryAttrs{AttachedKnown: true}).demand("app", "w-1")
	d1 := f.decide()
	if len(d1.Plans) != 1 || d1.Plans[0].Kind != createDependency || d1.Plans[0].Template != "db" {
		t.Fatalf("pass 1 plans %+v, want one db dependency floor for the live root", d1.Plans)
	}
	reserveAll(f, d1, allocNow)
	if d2 := f.decide(); len(d2.Plans) != 0 {
		t.Fatalf("pass 2 plans %+v while the db floor create is in flight", d2.Plans)
	}

	f = newAllocFixture(t, cfg).sessions(poolRow("gc-a", "app", 1, "active", "gc.trigger_bead_id", "w-1")).
		alive("s-gc-a", InventoryAttrs{AttachedKnown: true}).demand("app", "w-1").demand("db", "w-9")
	d1 = f.decide()
	if got := planSlots(d1, "db"); fmt.Sprint(got) != "[1]" || d1.Plans[0].Kind != createPool {
		t.Fatalf("pass 1 plans %+v, want one db pool plan", d1.Plans)
	}
	reserveAll(f, d1, allocNow)
	f.demand("db")
	if d2 := f.decide(); len(d2.Plans) != 0 {
		t.Fatalf("pass 2 plans %+v while a db pool create is in flight", d2.Plans)
	}
}

// Kills: a landed dependency floor row planned again (F1). Pass 1 plans the
// db floor; the real create effect writes its row; once the census shows
// the row, the entry clears and its reservation goes. The row is
// dependency-only from its create, so the next pass reuses it as the floor
// rather than planning a second one.
func TestAllocator_DependencyFloorLandedRowNotReplanned(t *testing.T) {
	db := config.Agent{Name: "db", StartCommand: "true", MaxActiveSessions: intPtr(3)}
	app := config.Agent{Name: "app", StartCommand: "true", MaxActiveSessions: intPtr(3), DependsOn: []string{"db"}}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{db, app}}
	appRow := poolRow("gc-a", "app", 1, "active", "gc.trigger_bead_id", "w-1")
	d1 := newAllocFixture(t, cfg).sessions(appRow).alive("s-gc-a", InventoryAttrs{AttachedKnown: true}).demand("app", "w-1").decide()
	if len(d1.Plans) != 1 || d1.Plans[0].Kind != createDependency {
		t.Fatalf("pass 1 plans %+v, want one db floor", d1.Plans)
	}
	p := d1.Plans[0]
	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: store}, createPlanOf("c1", p.Template, p.Plan))
	e := h.entry(t)
	all, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var landed []beads.Bead
	for _, b := range all {
		if b.ID == e.Marker.RowID {
			b.CreatedAt = allocNow.Add(-5 * time.Second)
			landed = append(landed, b)
		}
	}
	if len(landed) != 1 {
		t.Fatalf("landed rows %+v (entry %+v), want the floor row", landed, e)
	}
	d2 := newAllocFixture(t, cfg).sessions(appRow, landed[0]).alive("s-gc-a", InventoryAttrs{AttachedKnown: true}).demand("app", "w-1").decide()
	if got := planSlots(d2, "db"); len(got) != 0 {
		le := entryOf(t, d2, landed[0].ID)
		t.Fatalf("pass 2 plans a second db floor %v while the floor row %s is open (%s/%s)", got, landed[0].ID, le.Desired, le.Reason)
	}
	if le := entryOf(t, d2, landed[0].ID); !le.InDesired || le.Config == nil || !le.Config.DependencyOnly || landed[0].Metadata["dependency_only"] != "true" {
		t.Fatalf("landed floor row = %s/%s config %+v, want it dependency-only and selected as the floor", le.Desired, le.Reason, le.Config)
	}
}

// Kills: a sticky binding kept on work another row claimed or a resume
// request names (F2), and assigned-but-open work read as unclaimed (M19).
// Pass 1 binds the reusable X to W; Y then claims W. Whether Y is alive or a
// start candidate resuming W, X's pairing ends and X takes the new work W2
// rather than a fresh plan carrying it. When the demand read raced the
// claim, so W is still demand, assigned work is claimed whether in progress
// or still open.
func TestAllocator_StickyBindingYieldsToClaimAndResume(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	x, y := poolRow("gc-x", "worker", 1, "active"), poolRow("gc-y", "worker", 2, "active")
	d1 := newAllocFixture(t, cfg).sessions(x).demand("worker", "W").decide()
	if b := entryOf(t, d1, "gc-x").Binding; b == nil || b.WorkBeadID != "W" {
		t.Fatalf("pass 1: X binding %+v, want W", b)
	}
	claimedBy := func(f *allocFixture, status string) *allocFixture {
		f.in.Demand.AssignedWork = []beads.Bead{{ID: "W", Status: status, Assignee: "gc-y", Metadata: map[string]string{"gc.routed_to": "worker"}}}
		f.in.Demand.AssignedStoreRefs = []string{""}
		f.in.Prev, f.in.SelGen = d1.Snapshot, 2
		return f
	}
	for _, yAlive := range []bool{true, false} {
		f := newAllocFixture(t, cfg).sessions(x, y).demand("worker", "W2")
		if yAlive {
			f.alive("s-gc-y", InventoryAttrs{AttachedKnown: true})
		}
		d := claimedBy(f, "in_progress").decide()
		ex, ey := entryOf(t, d, "gc-x"), entryOf(t, d, "gc-y")
		if ex.Binding == nil || ex.Binding.WorkBeadID != "W2" || len(d.Plans) != 0 {
			t.Errorf("y alive=%v: X binding %+v, plans %v; want X on W2 and no plan", yAlive, ex.Binding, planWork(d))
		}
		if !yAlive && (ey.Binding == nil || ey.Binding.WorkBeadID != "W") {
			t.Errorf("Y resuming W: binding %+v, want W", ey.Binding)
		}
	}
	for _, status := range []string{"in_progress", "open"} {
		f := newAllocFixture(t, cfg).sessions(x, y).alive("s-gc-y", InventoryAttrs{AttachedKnown: true}).demand("worker", "W2", "W")
		if b := entryOf(t, claimedBy(f, status).decide(), "gc-x").Binding; b == nil || b.WorkBeadID != "W2" {
			t.Errorf("W %s and still demand: X binding %+v, want W2", status, b)
		}
	}
}

// Kills: a sticky binding kept on work that closed (F2): with no demand
// left, neither an in-flight nor a reusable X stays bound to W.
func TestAllocator_StickyBindingEndsWhenWorkCloses(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	for label, x := range map[string]beads.Bead{
		"in-flight": poolRow("gc-x", "worker", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", ago(10*time.Second)),
		"reusable":  poolRow("gc-x", "worker", 1, "active"),
	} {
		d1 := newAllocFixture(t, cfg).sessions(x).demand("worker", "W").decide()
		if b := entryOf(t, d1, "gc-x").Binding; b == nil || b.WorkBeadID != "W" {
			t.Fatalf("%s: pass 1 binding %+v, want W", label, b)
		}
		f := newAllocFixture(t, cfg).sessions(x)
		f.in.Prev, f.in.SelGen = d1.Snapshot, 2
		if b := entryOf(t, f.decide(), "gc-x").Binding; b != nil {
			t.Errorf("%s: pass 2 binding %+v after W closed, want none", label, b)
		}
	}
}

// Kills: a sticky binding that outlives its work's demand (F2, finding 4).
// Pass 1 binds the in-flight X to W. W then closes, and the template's only
// demand W2 is another in-flight row Y's trigger: pass after pass, X is not
// held on W, whose demand is gone.
func TestAllocator_StickyBindingNeedsUnclaimedDemand(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	x := poolRow("gc-x", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(10*time.Second))
	d := newAllocFixture(t, cfg).sessions(x).demand("worker", "W").decide()
	if b := entryOf(t, d, "gc-x").Binding; b == nil || b.WorkBeadID != "W" {
		t.Fatalf("pass 1: X binding %+v, want W", b)
	}
	y := poolRow("gc-y", "worker", 2, "start-pending", "pending_create_claim", "true", "pending_create_started_at", ago(10*time.Second),
		"gc.trigger_bead_id", "W2")
	for pass := 2; pass <= 4; pass++ {
		f := newAllocFixture(t, cfg).sessions(x, y).demand("worker", "W2")
		f.in.Prev, f.in.SelGen = d.Snapshot, uint64(pass)
		d = f.decide()
		if b := entryOf(t, d, "gc-x").Binding; b != nil && b.WorkBeadID == "W" {
			t.Errorf("pass %d: X stays bound to the closed work W (%s)", pass, b.ID)
		}
	}
}

// Kills: a binding ID minted afresh each pass for the same (row, work) pair
// (C6.1, finding 5): a start whose binding ID changed is abandoned. A
// wake_mode=fresh pool's asleep row Y holds assigned work W; its
// wake-known-identity request goes to the reusable X with a binding, which
// keeps its ID across passes.
func TestAllocator_BindingIDStableForTheSamePair(t *testing.T) {
	agent := allocPoolAgent("worker", 5)
	agent.WakeMode = "fresh"
	cfg := &config.City{Agents: []config.Agent{agent}}
	x, y := poolRow("gc-x", "worker", 1, "active"), poolRow("gc-y", "worker", 2, "asleep")
	work := []beads.Bead{{ID: "W", Status: "in_progress", Assignee: "s-gc-y", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	var prev *selectionSnapshot
	var ids []string
	for pass := 1; pass <= 3; pass++ {
		f := newAllocFixture(t, cfg).sessions(x, y)
		f.in.Demand.AssignedWork, f.in.Demand.AssignedStoreRefs = work, []string{""}
		f.in.Prev, f.in.SelGen = prev, uint64(pass)
		d := f.decide()
		b := entryOf(t, d, "gc-x").Binding
		if b == nil || b.WorkBeadID != "W" {
			t.Fatalf("pass %d: X binding %+v, want W", pass, b)
		}
		ids = append(ids, b.ID)
		prev = d.Snapshot
	}
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("binding IDs per pass %q, want one ID for the same (row, work) pair", ids)
	}
}

// Kills: a refused phase-2 pairing dropping its request (F7b). X is bound
// to W from the last pass, but W's worktree evidence is now refused, so X
// cannot take it; the request falls through to fresh realization, which
// selects the live row Z (a live row needs no worktree check).
func TestAllocator_RefusedPairingFallsThroughToFreshRealization(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	spec := worktree.Spec{BeadID: "W", StoreRef: "city", Path: "/wt/W"}
	withSpec := func(f *allocFixture) *allocFixture {
		d := f.in.Demand.Collected.DefaultDemand["worker"]
		d.WorktreeSpecs = map[string]*worktree.Spec{"W": &spec}
		f.in.Demand.Collected.DefaultDemand["worker"] = d
		return f
	}
	x := poolRow("gc-x", "worker", 1, "active")
	d1 := withSpec(newAllocFixture(t, cfg).sessions(x).demand("worker", "W")).decide()
	if b := entryOf(t, d1, "gc-x").Binding; b == nil || b.WorkBeadID != "W" {
		t.Fatalf("pass 1: X binding %+v, want W", b)
	}
	f := withSpec(newAllocFixture(t, cfg).sessions(x, poolRow("gc-z", "worker", 2, "active")).
		alive("s-gc-z", InventoryAttrs{AttachedKnown: true}).demand("worker", "W"))
	f.in.Backoff = workRefusal(spec)
	f.in.Prev, f.in.SelGen = d1.Snapshot, 2
	d := f.decide()
	// Z is realized for the request (pool identity), not only kept by the
	// overlay, which also includes live rows.
	if ez := entryOf(t, d, "gc-z"); !ez.InDesired || ez.Config == nil || ez.Config.PoolSlot != 2 {
		t.Fatalf("Z = %s/%s config %+v; want the refused pairing's request realized on Z; trace %v", ez.Desired, ez.Reason, ez.Config, d.Trace)
	}
}

// POOL-018: a store-query partial blocks no create.
func TestAllocator_StoreQueryPartialCreates(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).demand("worker", "w-1")
	f.in.Demand.StorePartial = true
	if got := planSlots(f.decide(), "worker"); len(got) != 1 {
		t.Fatalf("plans = %v: a store-query partial blocks no create", got)
	}
}

// POOL-012: a failed unassigned-routed read retains the control dispatcher
// without blocking its creates.
func TestAllocator_ControlDispatcherRetentionPartialDoesNotBlockCreate(t *testing.T) {
	cfg := cityOnlyDispatcherFixtureConfig(t)
	dispatcher := cfg.Agents[0].QualifiedName()
	f := newAllocFixture(t, cfg)
	f.in.Demand.Collected.UnassignedRoutedPartial = true
	d := f.decide()
	tp := d.Snapshot.Partial.Templates[dispatcher]
	if !tp.Retain || tp.BlockCreate {
		t.Fatalf("dispatcher partial = %+v, want retain without block-create", tp)
	}
}

// POOL-048, C5.10: provider red, an open endpoint, a refused transport and
// a #46 quarantine of the planned identity each refuse the request; the
// quarantine does not move it to the next slot (F3). Each is traced and
// consumes nothing.
func TestAllocator_PlanGatesRedQuarantineEndpointTransport(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}, Workspace: config.Workspace{Provider: "claude"}}
	closed := map[endpointKey]endpointView{"provider:claude": {Gate: gateClosed}}
	cases := []struct {
		cause string
		slots string
		setup func(*allocFixture)
	}{
		{"", "[1]", func(*allocFixture) {}},
		{gateProviderRed, "[]", func(f *allocFixture) {
			f.in.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{"claude": false}}
		}},
		{gateQuarantine, "[]", func(f *allocFixture) {
			key := boundSessionNameLength(poolIdentitySessionName("worker-1", "worker") + poolRuntimeNameSuffix)
			f.in.Episodes = map[string]session.StartupHealthEpisode{key: {QuarantinedUntil: allocNow.Add(time.Minute)}}
		}},
		{gateEndpointShut, "[]", func(f *allocFixture) {
			f.in.Endpoints = map[endpointKey]endpointView{"provider:claude": {Gate: gateShut}}
		}},
		{gateTransport, "[]", func(f *allocFixture) { f.in.TransportRefused = map[string]string{"worker": "no tmux"} }},
	}
	for _, tc := range cases {
		f := newAllocFixture(t, cfg).demand("worker", "w-1")
		f.in.Endpoints = closed
		tc.setup(f)
		d := f.decide()
		if got := fmt.Sprint(planSlots(d, "worker")); got != tc.slots || (tc.cause != "" && !traceHas(d, tc.cause)) {
			t.Errorf("%s: plans %s trace %v, want %s", tc.cause, got, d.Trace, tc.slots)
		}
	}
}

// POOL-002, V-D1: a template in a suspended rig gets no requests, so it
// consumes no caps, and its rows drain as suspended.
func TestAllocator_SuspendedRigTemplateExcludedBeforeCaps(t *testing.T) {
	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "r", Path: "/rigs/r"}},
		Agents: []config.Agent{{Name: "worker", Dir: "r", MaxActiveSessions: intPtr(3), MinActiveSessions: intPtr(1)}},
	}
	f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "r/worker", 1, "active")).alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).
		demand("r/worker", "w-1")
	f.in.SuspendedRigPaths = map[string]bool{"/rigs/r": true}
	d := f.decide()
	if d.Snapshot.PoolDesired["r/worker"] != 0 || len(d.Plans) != 0 {
		t.Fatalf("suspended rig template: PoolDesired %v plans %+v", d.Snapshot.PoolDesired, d.Plans)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired != desireDrain || e.DrainReason != drainSuspended {
		t.Fatalf("suspended rig row = %s/%s, want drain suspended", e.Desired, e.DrainReason)
	}
}

// POOL-046: a reused canonical singleton row whose stored identity is a
// phantom slot spelling is selected unchanged and marked for the session key
// to normalize before start; one already canonical is not.
func TestAllocator_SingletonReuseMarksNormalizeOnlyForAWrongIdentity(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("solo", 1)}}
	for label, row := range map[string]beads.Bead{
		"phantom":   poolRow("gc-1", "solo", 3, "active", "agent_name", "solo-3"),
		"canonical": poolRow("gc-1", "solo", 0, "active", "agent_name", "solo", "pool_slot", ""),
	} {
		d := newAllocFixture(t, cfg).sessions(row).alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).demand("solo", "w-1").decide()
		e := entryOf(t, d, "gc-1")
		if !e.InDesired || e.Config == nil || e.Config.ResolveKind != resolveBase || e.Normalize != (label == "phantom") {
			t.Errorf("%s singleton reuse = indesired=%v normalize=%v cfg=%+v", label, e.InDesired, e.Normalize, e.Config)
		}
	}
}

// Kills: a reused dependency-floor row started under a phantom identity
// (F7c, POOL-046): a canonical singleton dependency's floor row whose stored
// identity is a slot spelling is marked for the session key to normalize.
func TestAllocator_DependencyFloorReuseMarksNormalize(t *testing.T) {
	app := allocPoolAgent("app", 3)
	app.DependsOn = []string{"db"}
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("db", 1), app}}
	for label, row := range map[string]beads.Bead{
		"phantom":   poolRow("gc-d", "db", 3, "asleep", "agent_name", "db-3", "dependency_only", "true"),
		"canonical": poolRow("gc-d", "db", 0, "asleep", "agent_name", "db", "pool_slot", "", "dependency_only", "true"),
	} {
		d := newAllocFixture(t, cfg).sessions(poolRow("gc-a", "app", 1, "active", "gc.trigger_bead_id", "w-1"), row).
			alive("s-gc-a", InventoryAttrs{AttachedKnown: true}).demand("app", "w-1").decide()
		e := entryOf(t, d, "gc-d")
		if !e.InDesired || e.Config == nil || !e.Config.DependencyOnly || e.Normalize != (label == "phantom") || len(d.Plans) != 0 {
			t.Errorf("%s floor reuse = indesired=%v normalize=%v cfg=%+v plans %d", label, e.InDesired, e.Normalize, e.Config, len(d.Plans))
		}
	}
}

// POOL-059: the overlay includes a manual session without resolving its
// template, with a manual config ref.
func TestAllocator_OverlayIncludesManualSession(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "chat"}}}
	d := newAllocFixture(t, cfg).sessions(sessionRow("gc-1", "template", "chat", "state", "asleep",
		"session_name", "mine", "manual_session", "true", "alias", "me")).decide()
	e := entryOf(t, d, "gc-1")
	if !e.InDesired || e.Config == nil || e.Config.ResolveKind != resolveManual || e.Config.Alias != "me" || e.Normalize {
		t.Fatalf("manual row = indesired=%v normalize=%v cfg=%+v", e.InDesired, e.Normalize, e.Config)
	}
}

// Kills: a named row realized as a pool instance (legacy's defense in depth
// in the selection phase): a request naming it selects nothing.
func TestAllocator_RealizeNeverSelectsANamedRowForAPoolRequest(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{allocPoolAgent("worker", 3)},
		NamedSessions: []config.NamedSession{{Name: "boss", Template: "worker", Mode: "on_demand"}},
	}
	f := newAllocFixture(t, cfg).sessions(sessionRow("gc-1", "template", "worker", "state", "active", "session_name", "s-gc-1",
		"configured_named_session", "true", "configured_named_identity", "boss", "configured_named_mode", "on_demand"))
	p := newDecidePass(f.inputs())
	p.prepare()
	p.newPlanParams()
	p.poolStates = []PoolDesiredState{{Template: "worker", Requests: []SessionRequest{{Template: "worker", Tier: "resume", SessionBeadID: "gc-1"}}}}
	p.realizePools()
	if len(p.selected) != 0 || len(p.plans) != 0 {
		t.Fatalf("a named row realized for a pool request: selected %v plans %+v", maps.Keys(p.selected), p.plans)
	}
}
