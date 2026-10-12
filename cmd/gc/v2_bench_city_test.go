package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The v2 benchmarks' synthetic city (architecture §1.8 and its Appendix):
// pool templates with max = 2 × rows/template, rows round-robin across them,
// and of every 20 rows 13 active and alive, 5 asleep and 2 creating. Every
// other active row holds one in_progress assigned bead; each template has
// rows/(2·templates)+1 routed default demand; bucket 50; patrol 15s. It is
// pure and deterministic: every input derives from the row index.

const benchSessionsLeg = "city:bench"

// v2BenchCity is one synthetic city's pass inputs. In carries the census, the
// observation and the demand view; Sessions and Work are the beads they were
// built from.
type v2BenchCity struct {
	In       allocInputs
	Sessions []beads.Bead
	Work     []beads.Bead
}

// benchRowState is row i's state in the per-20 mix.
func benchRowState(i int) string {
	switch k := i % 20; {
	case k < 13:
		return "active"
	case k < 18:
		return "asleep"
	default:
		return "creating"
	}
}

// benchDemand is each template's routed default demand.
func benchDemand(sessions, templates int) int { return sessions/(2*templates) + 1 }

// newV2BenchCity builds a city of sessions rows over templates pool templates.
func newV2BenchCity(tb testing.TB, sessions, templates int) v2BenchCity {
	tb.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bench"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: intPtr(50), PatrolInterval: "15s"},
	}
	for t := 0; t < templates; t++ {
		cfg.Agents = append(cfg.Agents, allocPoolAgent(benchTemplate(t), max(2*(sessions/templates), 1)))
	}

	var city v2BenchCity
	attrs := make(map[string]InventoryAttrs)
	var listed, assignedRefs []string
	active := 0
	for i := 0; i < sessions; i++ {
		id, template, state := fmt.Sprintf("bc-%05d", i), benchTemplate(i%templates), benchRowState(i)
		city.Sessions = append(city.Sessions, poolRow(id, template, i/templates+1, state))
		if state != "active" {
			continue
		}
		name := "s-" + id
		attrs[name] = InventoryAttrs{DeadKnown: true, AttachedKnown: true}
		listed = append(listed, name)
		if active++; active%2 == 1 {
			city.Work = append(city.Work, beads.Bead{
				ID: "bw-" + id, Title: "bw-" + id, Type: "task", Status: "in_progress", Assignee: id,
				CreatedAt: censusNow, Metadata: map[string]string{"gc.routed_to": template},
			})
			assignedRefs = append(assignedRefs, "")
		}
	}

	demand := benchDemand(sessions, templates)
	collected := collectedDemand{
		DefaultProbed:   true,
		DefaultCounts:   make(map[string]int, templates),
		DefaultDemand:   make(map[string]scaleCheckDemand, templates),
		DefaultPartials: map[string]bool{},
	}
	for t := 0; t < templates; t++ {
		template := benchTemplate(t)
		d := scaleCheckDemand{Count: demand}
		for j := 0; j < demand; j++ {
			id := fmt.Sprintf("br-%s-%03d", template, j)
			collected.UnassignedRouted = append(collected.UnassignedRouted, beads.Bead{
				ID: id, Title: id, Type: "task", Status: "open", CreatedAt: censusNow,
				Metadata: map[string]string{"gc.routed_to": template},
			})
			collected.UnassignedRoutedRefs = append(collected.UnassignedRoutedRefs, "")
			d.WorkBeadIDs = append(d.WorkBeadIDs, id)
		}
		collected.DefaultCounts[template] = demand
		collected.DefaultDemand[template] = d
	}

	census, err := readSessionCensus(censusNow, cfg, []classStoreCandidate{{ref: benchSessionsLeg, store: censusStore(city.Sessions...)}})
	if err != nil {
		tb.Fatalf("bench census: %v", err)
	}
	city.In = allocInputs{
		Now:       censusNow,
		Epoch:     "e1",
		SelGen:    1,
		Cfg:       cfg,
		ConfigRev: "rev-1",
		CityPath:  "/city",
		CityName:  "bench",
		Census:    census,
		Demand:    demandView{AssignedWork: city.Work, AssignedStoreRefs: assignedRefs, Collected: collected},
		Obs:       newObserveCache().publish(censusNow, attrs, completeBackend("tmux", listed...)),
		ObsMaxAge: observeMaxAge,
	}
	return city
}

func benchTemplate(t int) string { return fmt.Sprintf("pool-%03d", t) }

// Kills: generator drift that would make the benchmark gate (G1) measure a
// different city: the per-20 mix, alive rows, the work holders, each
// template's demand and max, and determinism.
func TestV2BenchCityShape(t *testing.T) {
	const sessions, templates = 200, 30
	city := newV2BenchCity(t, sessions, templates)

	states := map[string]int{}
	for _, row := range city.In.Census.Canonical() {
		states[row.Info.MetadataState]++
	}
	if want := map[string]int{"active": 130, "asleep": 50, "creating": 20}; fmt.Sprint(states) != fmt.Sprint(want) {
		t.Fatalf("census states = %v, want %v", states, want)
	}
	alive := 0
	for _, o := range observeCensus(city.In.Obs, city.In.Census, city.In.Now, city.In.ObsMaxAge) {
		if o.Liveness.alive() {
			alive++
		}
	}
	if alive != 130 {
		t.Fatalf("alive rows = %d, want every active row (130)", alive)
	}

	holders := map[string]bool{}
	for _, w := range city.Work {
		row, ok := city.In.Census.Rows[rowKey{benchSessionsLeg, w.Assignee}]
		if w.Status != "in_progress" || !ok || row.Info.MetadataState != "active" || holders[w.Assignee] {
			t.Fatalf("work %s (%s) must be in_progress on its own active row", w.ID, w.Assignee)
		}
		holders[w.Assignee] = true
	}
	if len(holders) != 65 || len(city.In.Demand.AssignedStoreRefs) != len(city.Work) {
		t.Fatalf("work holders = %d (refs %d), want every other active row (65)", len(holders), len(city.In.Demand.AssignedStoreRefs))
	}

	col := city.In.Demand.Collected
	want := benchDemand(sessions, templates)
	if want != 4 || len(col.UnassignedRouted) != templates*want || len(col.DefaultCounts) != templates {
		t.Fatalf("demand %d, routed %d, templates %d: want 4 per template over %d", want, len(col.UnassignedRouted), len(col.DefaultCounts), templates)
	}
	for _, a := range city.In.Cfg.Agents {
		if col.DefaultCounts[a.Name] != want || len(col.DefaultDemand[a.Name].WorkBeadIDs) != want || *a.MaxActiveSessions != 12 {
			t.Fatalf("%s: count %d, work %d, max %d; want %d, %d, 12", a.Name, col.DefaultCounts[a.Name], len(col.DefaultDemand[a.Name].WorkBeadIDs), *a.MaxActiveSessions, want, want)
		}
	}
	if fmt.Sprint(newV2BenchCity(t, sessions, templates).Sessions) != fmt.Sprint(city.Sessions) {
		t.Fatal("generator is not deterministic")
	}
}
