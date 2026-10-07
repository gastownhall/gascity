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

// submitAll records d's plans as running creates in the in-flight view, as
// the planner submits them: each under its own token, with its plan's
// identity and trigger work, and no row in the census yet.
func submitAll(f *allocFixture, d allocDecision) {
	for _, p := range d.Plans {
		f.in.InFlight.Entries = append(f.in.InFlight.Entries, inflightEntry{
			Kind: inflightCreate, Token: fmt.Sprintf("tok-%d", len(f.in.InFlight.Entries)), Identity: p.identity(),
			Template: p.Template, QualifiedInstance: p.Plan.qualifiedInstance, Slot: p.Plan.poolSlot, WorkBeadID: p.Request.WorkBeadID,
		})
	}
}

// inFlightCreate is a running create entry for a pool slot, or for a named
// identity when instance is "named:<identity>".
func inFlightCreate(token, template, instance string, slot int, work string) inflightEntry {
	e := inflightEntry{Kind: inflightCreate, Token: token, Identity: template + "/" + instance, Template: template, QualifiedInstance: instance, Slot: slot, WorkBeadID: work}
	if strings.HasPrefix(instance, "named:") {
		e = inflightEntry{Kind: inflightCreate, Token: token, Identity: instance, Template: template}
	}
	return e
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
	if tp := d.Snapshot.Partial.Templates["worker"]; !tp.Retain {
		t.Fatalf("template partial = %+v, want retain", tp)
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

// Kills a regression of S1-2/S1-9: a failed or partial non-sessions leg
// blocking creates city-wide. Its rows are census-only relics; the create
// effect's locked live re-census fails closed on any partial leg. A partial
// read still retains.
func TestCreateNotBlockedByPartialNonSessionLeg(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	for _, tc := range []struct {
		err  error
		mode allocMode
	}{
		{errors.New("rig down"), modeNormal},
		{&beads.PartialResultError{Op: "list", Err: errors.New("rig down")}, modePartial},
	} {
		f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "active")).alive("s-gc-1", InventoryAttrs{}).
			demand("worker", "w-1", "w-2", "w-3")
		f.legs = append(f.legs, classStoreCandidate{ref: "rig:a", store: censusErrStore{Store: censusStore(), err: tc.err}})
		d := f.decide()
		if d.Snapshot.Mode != tc.mode {
			t.Errorf("rig err %v: mode %s, want %s", tc.err, d.Snapshot.Mode, tc.mode)
		}
		if got := planSlots(d, "worker"); len(got) != 2 {
			t.Errorf("rig err %v: plans %v trace %v, want two fresh creates", tc.err, got, d.Trace)
		}
		if !entryOf(t, d, "gc-1").InDesired {
			t.Errorf("rig err %v: the live row is not reused", tc.err)
		}
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

// Kills: a failed-create row holding its slot (POOL-050). Its slot is free;
// its identity lease is the create effect's to refuse under the identifier
// locks (fence), which advances the next pass to slot 2 (F3).
func TestAllocator_FreshSlot_FailedCreateKeepsNameNotSlot(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	d := newAllocFixture(t, cfg).
		sessions(poolRow("gc-1", "worker", 1, "failed-create")).
		demand("worker", "w-1").decide()
	if e := entryOf(t, d, "gc-1"); e.Desired != desireNone || e.Reason != reasonFailedCreate {
		t.Fatalf("failed-create row = %s/%s", e.Desired, e.Reason)
	}
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[1]" {
		t.Fatalf("plan slots = %v, want [1]: the failed-create row's slot is free", got)
	}
}

// Kills: replanning the refused slot (S2-8's replacement for F3's retry
// loop). A live fence backoff occupies its pool name, so the pass plans the
// next free slots, never the backoff's own, and traces the held name as
// create-refused:fence (P-6); that holds in a capped and an unlimited pool
// and for a rig-scoped template. A template that prefixes another's key
// ("a" against rig "a"'s "a/a") holds nothing for it. An expired fence
// backoff occupies nothing, and another cause still refuses (and stalls) the
// request on its slot. A canonical singleton has one name: its fence backoff
// refuses the plan, as before.
func TestFenceBackoffOccupiesSlot(t *testing.T) {
	rig := &config.City{
		Rigs:   []config.Rig{{Name: "r", Path: "/rigs/r"}},
		Agents: []config.Agent{{Name: "worker", Dir: "r", MaxActiveSessions: intPtr(5)}},
	}
	prefixed := &config.City{
		Rigs:   []config.Rig{{Name: "a", Path: "/rigs/a"}},
		Agents: []config.Agent{allocPoolAgent("a", 5), {Name: "a", Dir: "a", MaxActiveSessions: intPtr(5)}},
	}
	for label, tc := range map[string]struct {
		cfg      *config.City
		template string
		refused  string
		cause    string
		until    time.Time
		want     string
		trace    string
		held     string // the instance a live fence backoff holds, traced
	}{
		"capped":            {template: "worker", refused: "worker/worker-1", cause: createStageFence, want: "[2 3 4]", held: "worker-1"},
		"unlimited":         {cfg: &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(-1)}}}, template: "worker", refused: "worker/worker-1", cause: createStageFence, want: "[2 3 4]", held: "worker-1"},
		"rig-scoped":        {cfg: rig, template: "r/worker", refused: "r/worker/r/worker-1", cause: createStageFence, want: "[2 3 4]", held: "r/worker-1"},
		"prefix-of-another": {cfg: prefixed, template: "a", refused: "a/a/a/a-1", cause: createStageFence, want: "[1 2 3]"},
		"expired":           {template: "worker", refused: "worker/worker-1", cause: createStageFence, until: allocNow, want: "[1 2 3]"},
		"other-cause":       {template: "worker", refused: "worker/worker-1", cause: "prepare", want: "[2 3]", trace: gateCreateRefused + "prepare"},
		"singleton":         {cfg: &config.City{Agents: []config.Agent{allocPoolAgent("solo", 1)}}, template: "solo", refused: "solo/solo", cause: createStageFence, want: "[]", trace: gateCreateRefused + createStageFence},
	} {
		cfg := tc.cfg
		if cfg == nil {
			cfg = &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
		}
		until := tc.until
		if until.IsZero() {
			until = allocNow.Add(5 * time.Minute)
		}
		f := newAllocFixture(t, cfg).demand(tc.template, "w-1", "w-2", "w-3")
		f.in.Backoff = createRefusal(tc.refused, tc.cause, until)
		d := f.decide()
		if got := fmt.Sprint(planSlots(d, tc.template)); got != tc.want {
			t.Errorf("%s: plans %s, want %s; trace %v", label, got, tc.want, d.Trace)
		}
		heldTraced := slices.ContainsFunc(d.Trace, func(r allocTraceRecord) bool {
			return r.Template == tc.template && r.Instance == tc.held && r.Reason == "ineligible:"+gateCreateRefused+createStageFence
		})
		if tc.held != "" && !heldTraced {
			t.Errorf("%s: the held name %s is not traced: %v", label, tc.held, d.Trace)
		}
		if tc.held == "" && tc.trace == "" && slices.ContainsFunc(d.Trace, func(r allocTraceRecord) bool { return r.Template == tc.template }) {
			t.Errorf("%s: unexpected refusal for %s: %v", label, tc.template, d.Trace)
		}
		if tc.trace != "" && !traceHas(d, tc.trace) {
			t.Errorf("%s: trace %v, want %s", label, d.Trace, tc.trace)
		}
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
		submitAll(f, first)
		second := f.decide()
		if len(second.Plans) != 0 {
			t.Fatalf("%s: pass 2 replanned in-flight creates: %v", label, planWork(second))
		}
		if got := second.Snapshot.PoolDesired["worker"]; got != 2 {
			t.Fatalf("%s: pass 2 PoolDesired = %d, want the 2 in flight", label, got)
		}

		// One create lands: its row carries its token; demand grows.
		landed := f.in.InFlight.Entries[0]
		row := poolRow("gc-new", "worker", landed.Slot, "start-pending", "pending_create_claim", "true",
			"pending_create_started_at", ago(5*time.Second), "instance_token", landed.Token,
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
	submitAll(f, f.decide())
	f.demand("worker", "w-1", "w-2")
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-1]" {
		t.Fatalf("plans carry %v, want [w-1]: w-2's create is in flight", got)
	}
}

// Kills: an in-flight create dropped from the demand floor (POOL-028): two
// creates are in flight and the demand has shrunk to one of their items (the
// other was claimed elsewhere); both creates keep their place, and an
// ambiguous one too until a census row carries its token.
func TestAllocator_InFlightCreateHoldsTheDemandFloor(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 5)}}
	f := newAllocFixture(t, cfg).demand("worker", "w-1")
	ambiguous := inFlightCreate("tok-2", "worker", "worker-2", 2, "w-2")
	ambiguous.Ambiguous = true
	f.in.InFlight.Entries = []inflightEntry{inFlightCreate("tok-1", "worker", "worker-1", 1, "w-1"), ambiguous}
	d := f.decide()
	if got := d.Snapshot.PoolDesired["worker"]; got != 2 || len(d.Plans) != 0 {
		t.Errorf("PoolDesired = %d plans %+v, want the 2 in flight and no plan", got, d.Plans)
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
	f.in.InFlight.Entries = []inflightEntry{inFlightCreate("tok-1", "worker", "named:boss", 0, "")}
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
	if tp := d.Snapshot.Partial.Templates["chat"]; tp.Retain || !slices.Contains(tp.Causes, "named-scale-check-partial") {
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
	submitAll(f, first)
	if second := f.decide(); len(second.Plans) != 0 {
		t.Fatalf("pass 2 replanned the singleton: %+v (trace %v)", second.Plans, second.Trace)
	}
}

// Kills: stickiness creeping back (C6.1, S2-5): the decide has no previous
// snapshot to read. Within the pass a selected start candidate is bound to
// its request's work, a live row never is, and no work item is bound twice.
func TestBindingRecomputedEachPass(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	rows := []beads.Bead{
		poolRow("gc-1", "worker", 1, "active", "gc.trigger_bead_id", "w-old"),
		poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-old2"),
	}
	fixture := func() *allocFixture {
		return newAllocFixture(t, cfg).sessions(rows...).alive("s-gc-2", InventoryAttrs{}).demand("worker", "w-1", "w-2")
	}
	bindings := func(d allocDecision) string {
		return fmt.Sprintf("gc-1:%+v gc-2:%+v plans:%v", entryOf(t, d, "gc-1").Binding, entryOf(t, d, "gc-2").Binding, planWork(d))
	}
	fresh := fixture().decide()
	dead, live := entryOf(t, fresh, "gc-1"), entryOf(t, fresh, "gc-2")
	if live.Binding != nil {
		t.Fatalf("a live row was bound: %+v", live.Binding)
	}
	if dead.Binding == nil || dead.Binding.WorkBeadID != "w-1" || fmt.Sprint(planWork(fresh)) != "[]" {
		t.Fatalf("no previous snapshot: %s, want gc-1 bound to w-1", bindings(fresh))
	}
}

// Kills: binding one work bead to a second row while a row that is alive or
// is starting carries it (C6.3, S2-5). gc-2 carries w-1 as its trigger
// (starting is v5's: a pending-create claim or the creating state, with no
// lease); dead gc-1 is selected with w-1 as its binding candidate and is not
// bound to it. A gc-2 that is neither alive nor starting consumes nothing,
// so gc-1 is.
func TestWorkConsumedByAliveOrStartingRow(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	gc1 := poolRow("gc-1", "worker", 1, "active")
	for label, tc := range map[string]struct {
		gc2      beads.Bead
		alive    bool
		consumed bool
	}{
		"alive": {gc2: poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-1"), alive: true, consumed: true},
		"start-lease": {gc2: poolRow("gc-2", "worker", 2, "creating", "gc.trigger_bead_id", "w-1",
			"pending_create_claim", "true", "pending_create_started_at", ago(10*time.Second)), consumed: true},
		"neither": {gc2: poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-1")},
	} {
		f := newAllocFixture(t, cfg).sessions(gc1, tc.gc2)
		if tc.alive {
			f.alive("s-gc-2", InventoryAttrs{AttachedKnown: true})
		}
		p := newDecidePass(f.inputs())
		p.prepare()
		p.selected[p.byID["gc-1"]] = &selection{binding: &bindingTarget{WorkBeadID: "w-1"}}
		p.selected[p.byID["gc-2"]] = &selection{}
		d := p.finish()
		b1, b2 := entryOf(t, d, "gc-1").Binding, entryOf(t, d, "gc-2").Binding
		if b2 != nil {
			t.Errorf("%s: gc-2 bound to the work it carries: %+v", label, b2)
		}
		if bound := b1 != nil && b1.WorkBeadID == "w-1"; bound == tc.consumed {
			t.Errorf("%s: gc-1 binding %+v, want bound to w-1 = %v", label, b1, !tc.consumed)
		}
	}
}

// Kills: map order deciding which of two holders consumes one trigger. Two
// start-lease rows carry w-1, and each is selected with a binding candidate
// for it (its work dir differs from the row's). The first holder in key
// order consumes w-1, so only gc-1 may be bound to it, on every one of 200
// passes: a 20-run purity check misses a coin flip like this.
func TestBindingConsumptionIsDeterministic(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	holder := func(id string, slot int) beads.Bead {
		return poolRow(id, "worker", slot, "creating", "gc.trigger_bead_id", "w-1", "pending_create_claim", "true",
			"pending_create_started_at", ago(10*time.Second), "last_woke_at", ago(10*time.Second))
	}
	f := newAllocFixture(t, cfg).sessions(holder("gc-1", 1), holder("gc-2", 2)).demand("worker", "w-1")
	in := f.inputs()
	var first string
	for i := 0; i < 200; i++ {
		d := mustDecide(t, in)
		b1, b2 := entryOf(t, d, "gc-1").Binding, entryOf(t, d, "gc-2").Binding
		if b2 != nil && b2.WorkBeadID == "w-1" {
			t.Fatalf("pass %d: gc-2 bound to w-1, which gc-1 (first in key order) consumes: %+v", i, b2)
		}
		got := fmt.Sprintf("gc-1:%+v gc-2:%+v", b1, b2)
		if i == 0 {
			first = got
			if b1 == nil || b1.WorkBeadID != "w-1" {
				t.Fatalf("the fixture must give gc-1 a w-1 binding: %s", got)
			}
		} else if got != first {
			t.Fatalf("pass %d: %s, want %s", i, got, first)
		}
	}
}

// Kills: a reusable row paired with work a live row carries outside a
// concrete request, and that work planned again (S2-5's accepted change,
// through the whole decide). Live gc-2 carries w-1; dead gc-1 is reusable.
// The live row stays selected (the overlay keeps it), gc-1 is not bound to
// w-1, and no create is planned.
func TestLiveHolderKeepsItsWorkEndToEnd(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	d := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "worker", 2, "active", "gc.trigger_bead_id", "w-1"),
	).alive("s-gc-2", InventoryAttrs{AttachedKnown: true}).demand("worker", "w-1").decide()
	if e := entryOf(t, d, "gc-2"); !e.InDesired || e.Binding != nil {
		t.Errorf("live holder = indesired=%v binding %+v, want selected, unbound", e.InDesired, e.Binding)
	}
	if b := entryOf(t, d, "gc-1").Binding; b != nil && b.WorkBeadID == "w-1" {
		t.Errorf("reused gc-1 bound to the live row's w-1: %+v", b)
	}
	if len(d.Plans) != 0 {
		t.Errorf("plans %v (work %v): w-1 is held, nothing to create", planSlots(d, "worker"), planWork(d))
	}
}

// Kills: a binding for a row that is not a start candidate (AM2), and a
// live row's selection
// dropped for a refused worktree (owner decision at P3-5a review: keep the
// selection, drop only the binding).
func TestAllocator_BindingsOnlyForStartCandidates(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 1)}}
	for label, setup := range map[string]func(*allocFixture){
		"unknown": func(f *allocFixture) { f.noInventory = true },
		"alive":   func(f *allocFixture) { f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true}) },
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

// Kills: a named create under a C7.4 gate (P3-6b obligations): provider red,
// a #46 quarantine of its session name and a shut endpoint each refuse it,
// traced.
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
	e := inFlightCreate("tok-1", "chat", "named:chat", 0, "")
	e.SessionName = namedRuntimeName(t, cfg)
	f.in.InFlight.Entries = []inflightEntry{e}
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
// and the default probe counting the suppressed route (mc-zndi7.41; legacy's
// probe applies the same rule through controlRowServableByTemplate). The
// projection runs once, and control demand, the ready routed work and the
// default probe all read its rows.
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
		in.InFlight.Entries = []inflightEntry{inFlightCreate("tok-1", "worker", "worker-4", 4, "w-4")}
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
	if !d.Snapshot.Partial.Templates["custom"].Retain {
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
	f.in.InFlight.Entries = []inflightEntry{inFlightCreate("tok-9", "worker", "worker-2", 2, "w-2")}
	d := f.decide()
	if got := planSlots(d, "worker"); fmt.Sprint(got) != "[3]" {
		t.Fatalf("plans = %v, want [3]: slot 2 is reserved by an uncleared create", got)
	}
	if len(d.Planning) != 1 || d.Planning[0].ID != "gc-1" {
		t.Fatalf("planning census = %+v, want the census row only", d.Planning)
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
	if !tp.Retain {
		t.Fatalf("dispatcher partial = %+v, want retain", tp)
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
