package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// realizePass runs the decide's desire steps (prepare, plan, finish; the
// realization and everything it feeds) with or without the realization
// index.
func realizePass(t *testing.T, in allocInputs, indexed bool) (*decidePass, allocDecision) {
	t.Helper()
	p := newDecidePass(in)
	if !indexed {
		p.index = nil
	}
	p.prepare()
	p.plan()
	return p, p.finish()
}

// oracleCity is seed's random city for the realization oracle. Its pool
// agents take every identity shape the realize path branches on (numbered,
// canonical singleton, namepool, rig-scoped, one_shot, unlimited); its rows
// every class reuse and the slot claim read (held, quarantined, draining,
// drained, failed-create, pending, manual, dependency-only, out-of-bounds
// or shared slots, legacy template spellings), some shadowed on a second
// leg under the same bead ID; its work every assignee spelling, status and
// a duplicated work ID; plus fence backoffs, planning reservations and a
// named session whose plan reserves.
func oracleCity(t *testing.T, seed int64) allocInputs {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
	cfg := &config.City{Workspace: config.Workspace{Name: "oracle"}, Daemon: config.DaemonConfig{MaxWakesPerTick: intPtr(50)}}
	nTemplates := 1 + rng.Intn(6)
	for i := 0; i < nTemplates; i++ {
		a := config.Agent{Name: fmt.Sprintf("pool-%d", i), MaxActiveSessions: intPtr(1 + rng.Intn(12))}
		switch rng.Intn(7) {
		case 0:
			a.MaxActiveSessions = intPtr(1)
		case 1:
			a.NamepoolNames = []string{"ann", "bob", "cy", "dee"}
		case 2:
			a.Dir = "rig"
		case 3:
			a.Lifecycle = config.AgentLifecycleOneShot
		case 4:
			a.MaxActiveSessions = nil
		}
		cfg.Agents = append(cfg.Agents, a)
	}
	if rng.Intn(3) == 0 {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
	}

	instance := func(a config.Agent, slot int) string {
		if len(a.NamepoolNames) > 0 {
			return a.NamepoolNames[slot%len(a.NamepoolNames)]
		}
		if a.MaxActiveSessions != nil && *a.MaxActiveSessions == 1 && rng.Intn(2) == 0 {
			return a.QualifiedName()
		}
		return fmt.Sprintf("%s-%d", a.QualifiedName(), slot)
	}
	f := newAllocFixture(t, cfg)
	f.in.CityName = "oracle"
	var rows, shadows []beads.Bead
	var identities [][]string
	n := rng.Intn(301)
	for i := 0; i < n; i++ {
		a := cfg.Agents[rng.Intn(nTemplates)]
		id, slot := fmt.Sprintf("or-%04d", i), 1+rng.Intn(14)
		name := instance(a, slot)
		state := pick("active", "active", "active", "asleep", "asleep", "creating", "draining", "drained", "failed_create")
		template := a.QualifiedName()
		switch rng.Intn(10) {
		case 0:
			template = "" // legacy: the identity in agent_name only
		case 1:
			template = a.Name // unqualified spelling of a rig agent
		case 2:
			template = "gone-" + a.Name
		}
		meta := []string{
			"template", template, "state", state, "pool_managed", "true",
			"session_name", "s-" + id, "agent_name", name, "generation", "1",
		}
		if rng.Intn(5) > 0 {
			meta = append(meta, "pool_slot", fmt.Sprint(slot))
		}
		alias := ""
		if rng.Intn(3) == 0 {
			alias = pick(name, fmt.Sprintf("%s-%d", a.QualifiedName(), 1+rng.Intn(14)), "al-"+id)
			meta = append(meta, "alias", alias)
		}
		future := allocNow.Add(time.Hour).Format(time.RFC3339)
		if state == "asleep" && rng.Intn(2) == 0 {
			// Freeable: a one_shot pool reuses it unless it holds work
			// under any identity, its alias history included.
			meta = append(meta, "sleep_reason", "idle", "slept_at", ago(time.Minute), "alias_history", "old-"+id)
		}
		switch rng.Intn(12) {
		case 0:
			meta = append(meta, "held_until", future)
		case 1:
			meta = append(meta, "quarantined_until", future)
		case 2:
			meta = append(meta, "wait_hold", "w-1")
		case 3:
			meta = append(meta, "sleep_reason", pick("idle", "user-hold"), "slept_at", ago(time.Minute))
		case 4:
			meta = append(meta, "pending_create_claim", "true", "pending_create_started_at", ago(time.Duration(rng.Intn(600))*time.Second))
		case 5:
			meta = append(meta, "manual_session", "true")
		case 6:
			meta = append(meta, "dependency_only", "true")
		case 7:
			meta = append(meta, "alias_history", "old-"+id)
		}
		row := sessionRow(id, meta...)
		row.CreatedAt = allocNow.Add(-time.Duration(rng.Intn(5)) * time.Hour)
		rows = append(rows, row)
		identities = append(identities, []string{id, "s-" + id, name, alias, "old-" + id})
		if rng.Intn(15) == 0 {
			shadow := sessionRow(id, "template", template, "state", "active", "pool_managed", "true",
				"session_name", "s-x"+id, "agent_name", instance(a, 1+rng.Intn(14)), "pool_slot", fmt.Sprint(1+rng.Intn(14)))
			shadows = append(shadows, shadow)
		}
		switch state {
		case "active":
			if rng.Intn(6) == 0 {
				f.corpse("s-" + id)
			} else if rng.Intn(8) > 0 {
				f.alive("s-"+id, InventoryAttrs{AttachedKnown: true})
			}
		case "creating":
			if rng.Intn(2) == 0 {
				f.alive("s-"+id, InventoryAttrs{AttachedKnown: true})
			}
		}
	}
	f.sessions(rows...)
	if len(shadows) > 0 {
		f.rigLeg(shadows...)
	}

	var work []beads.Bead
	for i := 0; len(identities) > 0 && i < n/2+rng.Intn(n+1); i++ {
		ids := identities[rng.Intn(len(identities))]
		assignee := ids[rng.Intn(len(ids))]
		if rng.Intn(10) == 0 {
			assignee = " " + assignee + " "
		}
		wid := fmt.Sprintf("ow-%04d", i)
		if i > 0 && rng.Intn(12) == 0 {
			wid = work[rng.Intn(len(work))].ID
		}
		work = append(work, beads.Bead{
			ID: wid, Title: wid, Type: "task", Status: pick("open", "in_progress", "in_progress", "closed"), Assignee: assignee,
			CreatedAt: allocNow, Metadata: map[string]string{"gc.routed_to": cfg.Agents[rng.Intn(nTemplates)].QualifiedName()},
		})
	}
	f.in.Demand = demandView{AssignedWork: work, AssignedStoreRefs: make([]string, len(work))}
	col := collectedDemand{
		DefaultProbed: true, DefaultCounts: map[string]int{}, DefaultDemand: map[string]scaleCheckDemand{}, DefaultPartials: map[string]bool{},
	}
	for _, a := range cfg.Agents[:nTemplates] {
		template := a.QualifiedName()
		d := scaleCheckDemand{Count: rng.Intn(8)}
		for j := 0; j < d.Count; j++ {
			id := fmt.Sprintf("od-%s-%d", strings.ReplaceAll(template, "/", "-"), j)
			col.UnassignedRouted = append(col.UnassignedRouted, beads.Bead{
				ID: id, Title: id, Type: "task", Status: "open", CreatedAt: allocNow, Metadata: map[string]string{"gc.routed_to": template},
			})
			col.UnassignedRoutedRefs = append(col.UnassignedRoutedRefs, "")
			d.WorkBeadIDs = append(d.WorkBeadIDs, id)
		}
		col.DefaultCounts[template], col.DefaultDemand[template] = d.Count, d
		if rng.Intn(4) == 0 {
			slot := 1 + rng.Intn(6)
			f.in.Reservations = append(f.in.Reservations, planReservation{
				EntryID: fmt.Sprintf("e-%s-%d", template, slot), Template: template,
				QualifiedInstance: fmt.Sprintf("%s-%d", template, slot), Slot: slot, ReservedAt: allocNow,
			})
		}
		if rng.Intn(4) == 0 {
			if f.in.Backoff == nil {
				f.in.Backoff = map[string]backoffRecord{}
			}
			key := createBackoffKey(fmt.Sprintf("%s/%s-%d", template, template, 1+rng.Intn(6)))
			f.in.Backoff[key] = backoffRecord{Consecutive: 1, Until: allocNow.Add(time.Minute), Cause: createStageFence}
		}
	}
	f.in.Demand.Collected = col
	return f.inputs()
}

// Kills: an index that drops or duplicates a reuse candidate, normalizes a
// row's template differently from legacy's resolvedSessionTemplateInfo,
// narrows away work a reuse or resume check reads, or miscounts a fresh
// slot's occupancy (a shadowed bead ID, a canonical holder, an out-of-bounds
// slot). Over seeded random cities the indexed realization equals the
// unindexed one, legacy's own path: the same selected rows with their
// config refs and binding candidates, the same plans with their slots and
// identifiers, the same refusals, desired membership and published entries.
func TestRealizationIndexOracle(t *testing.T) {
	seeds := 500
	if testing.Short() {
		seeds = 50
	}
	var selected, plans, refusals int
	for seed := int64(0); seed < int64(seeds); seed++ {
		in := oracleCity(t, seed)
		legacy, want := realizePass(t, in, false)
		indexed, got := realizePass(t, in, true)
		if indexed.index == nil || indexed.index.rowsByTemplate == nil {
			t.Fatalf("seed %d: the indexed pass built no realization index", seed)
		}
		if !reflect.DeepEqual(indexed.selected, legacy.selected) {
			t.Fatalf("seed %d: selected rows differ:\nindexed %s\nlegacy  %s", seed, selectionDump(indexed), selectionDump(legacy))
		}
		if !reflect.DeepEqual(got.Plans, want.Plans) {
			t.Fatalf("seed %d: plans differ:\nindexed %+v\nlegacy  %+v", seed, got.Plans, want.Plans)
		}
		if !reflect.DeepEqual(got.Trace, want.Trace) {
			t.Fatalf("seed %d: refusals differ:\nindexed %+v\nlegacy  %+v", seed, got.Trace, want.Trace)
		}
		if !reflect.DeepEqual(indexed.desired, legacy.desired) || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: desired membership or published decision differ", seed)
		}
		// Each agent's view holds exactly legacy's reuse universe: the rows
		// resolving to its template, once each, in snapshot order.
		for i := range in.Cfg.Agents {
			a := &in.Cfg.Agents[i]
			view := indexed.realizeParams(a, nil)
			rows := []session.Info{}
			for _, info := range indexed.bp.sessionBeads.OpenInfos() {
				if resolvedSessionTemplateInfo(info, in.Cfg) == a.QualifiedName() {
					rows = append(rows, info)
				}
			}
			if !reflect.DeepEqual(view.sessionBeads.OpenInfos(), rows) || !reflect.DeepEqual(view.realizeMemo.rows, rows) {
				t.Fatalf("seed %d: %s's view holds %d rows (memo %d), want its template's %d", seed, a.QualifiedName(),
					len(view.sessionBeads.OpenInfos()), len(view.realizeMemo.rows), len(rows))
			}
		}
		selected += len(legacy.selected)
		plans += len(want.Plans)
		refusals += len(want.Trace)
	}
	// The cities must reach every outcome, or equality proves little.
	if selected < seeds*5 || plans < seeds || refusals < seeds {
		t.Fatalf("oracle cities too tame: %d selected, %d plans, %d refusals over %d seeds", selected, plans, refusals, seeds)
	}
}

func selectionDump(p *decidePass) string {
	var b strings.Builder
	for _, k := range sortedSelectionKeys(p.selected) {
		s := p.selected[k]
		fmt.Fprintf(&b, "%s:%+v/%+v ", k.ID, s.ref, s.binding)
	}
	return b.String()
}

func sortedSelectionKeys(m map[rowKey]*selection) []rowKey {
	keys := make([]rowKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortRowKeys(keys)
	return keys
}

// Kills: a nil-guard regression in the legacy helpers the index hooks into
// (freshPoolOccupancyInfos, reusablePoolSessionInfos): legacy's own params,
// built by its constructor, carry no memo, and over them both helpers and
// the fresh-slot claim return legacy's golden output, a shadowed bead ID
// folded first-wins and a held slot skipped.
func TestRealizationNilIndexIsLegacy(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 4)}}
	if bp := newAgentBuildParams("city", "/city", cfg, nil, allocNow, nil, nil); bp.realizeMemo != nil {
		t.Fatal("legacy build params carry a realization memo")
	}
	info := func(id, agentName, slot, state string) session.Info {
		return session.Info{
			ID: id, Template: "worker", AgentName: agentName, PoolSlot: slot, PoolManaged: true,
			MetadataState: state, SessionNameMetadata: "s-" + id, CreatedAt: allocNow,
		}
	}
	primary := []session.Info{
		info("gc-1", "worker-1", "1", "asleep"),
		info("gc-2", "worker-2", "2", "active"),
		info("gc-3", "worker-3", "3", "active"),
	}
	bp := baseAgentBuildParams("city", "/city", cfg, nil, allocNow, nil, nil)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(primary)
	bp.sessionOccupancyInfos = []session.Info{info("gc-2", "worker-4", "4", "active"), info("rig-9", "worker-1", "1", "failed_create")}
	bp.assignedWorkBeads = []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "gc-3"}}

	var ids []string
	for _, i := range freshPoolOccupancyInfos(bp) {
		ids = append(ids, i.ID+"@"+i.PoolSlot)
	}
	if got, want := strings.Join(ids, ","), "gc-2@4,rig-9@1,gc-1@1,gc-3@3"; got != want {
		t.Fatalf("legacy fresh occupancy = %s, want %s", got, want)
	}
	ids = nil
	for _, i := range reusablePoolSessionInfos(bp, &cfg.Agents[0], "worker", nil) {
		ids = append(ids, i.ID)
	}
	if got, want := strings.Join(ids, ","), "gc-2"; got != want {
		t.Fatalf("legacy reusable rows = %s, want %s (asleep and working rows skipped)", got, want)
	}
	slot, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], map[int]bool{})
	if err != nil || slot != 2 {
		t.Fatalf("legacy fresh slot = %d, %v; want 2 (1 held by gc-1, 3 by gc-3, 4 by gc-2's census row)", slot, err)
	}
}
