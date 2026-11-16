package main

// The requalification stability contract (gastownhall/gascity#5957): three
// consecutive ticks in each of {legacy, v2} x {always, on_demand} leave the
// requalified named session's row untouched where it matters — the work
// stays in_progress and assigned to the bare form, the row stays open under
// the same bead ID, the runtime is neither stopped nor started, and no
// second row appears. The legacy ticks are full daemon ticks
// (requalifyDaemonTick: build, sync with skipClose=true, then the bead
// reconciler), so a start, drain or close the reconciler would issue is in
// what they observe. On the pre-fix tree the first tick drains or
// drift-restarts the seat and mints a replacement row.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	stabilityQualified = "contentbuild/lane.golden-hon"
	stabilityBare      = "golden-hon"
)

// stabilityCity is the post-adopt config: only the bound, rig-scoped entry.
func stabilityCity(mode string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "contentbuild", Path: "contentbuild"}},
		Agents: []config.Agent{
			{Name: stabilityBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Template: stabilityBare, Dir: "contentbuild", BindingName: "lane", Mode: mode},
		},
	}
}

// stabilitySeedRow seeds the live row exactly as the pre-adopt config left
// it: bare identity everywhere, started, with the baseline a config-drift
// compare would trip over once the template requalifies.
func stabilitySeedRow(t *testing.T, store beads.Store, mode string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  stabilityBare,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               stabilityBare,
			"alias":                      stabilityBare,
			"template":                   stabilityBare,
			"agent_name":                 stabilityBare,
			"state":                      "active",
			"session_origin":             "named",
			"wake_mode":                  "fresh",
			"command":                    "true",
			"continuity_eligible":        "true",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: stabilityBare,
			namedSessionModeMetadata:     mode,
			"started_config_hash":        "v6:stale-core",
			"started_live_hash":          "v6:stale-live",
			"live_hash":                  "v6:stale-live",
			"started_provision_hash":     "v6:stale-prov",
			"started_launch_hash":        "v6:stale-launch",
			"core_hash_breakdown":        "{}",
		},
	})
	if err != nil {
		t.Fatalf("seeding the pre-adopt row: %v", err)
	}
	return b
}

// stabilitySeedWork claims open work under the bare assignee, the form the
// lane's gate work was stamped with.
func stabilitySeedWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	w, err := store.Create(beads.Bead{Title: "hon gate", Type: "task", Assignee: stabilityBare})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	status := "in_progress"
	if err := store.Update(w.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("claiming work: %v", err)
	}
	return w
}

// stabilityOpenRows lists the store's live session rows.
func stabilityOpenRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: sessionBeadLabel, Live: true})
	if err != nil {
		t.Fatalf("listing session rows: %v", err)
	}
	return rows
}

// AC-3, legacy half: three consecutive full legacy daemon ticks in each mode
// keep the requalified row stable — after every tick the work is still
// claimed by the bare assignee, the row is the only open one under the same
// bead ID, and the runtime has been neither stopped nor started.
func TestLegacyRequalificationIsStableAcrossThreeTicks(t *testing.T) {
	for _, mode := range []string{"always", "on_demand"} {
		t.Run(mode, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := stabilityCity(mode)
			row := stabilitySeedRow(t, store, mode)
			work := stabilitySeedWork(t, store)
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), stabilityBare, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			starts, stops := sp.CountCalls("Start", stabilityBare), sp.CountCalls("Stop", stabilityBare)
			cityPath := t.TempDir()
			clk := &clock.Fake{Time: namedEffectNow}
			dt := newDrainTracker()
			var stderr bytes.Buffer
			for tick := 1; tick <= 3; tick++ {
				requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
				w, err := store.Get(work.ID)
				if err != nil {
					t.Fatalf("tick %d: reading work: %v", tick, err)
				}
				if w.Status != "in_progress" || w.Assignee != stabilityBare {
					t.Errorf("tick %d: work = %s/%q, want in_progress assigned %q", tick, w.Status, w.Assignee, stabilityBare)
				}
				if !liveOpenSessionAssignmentExists(store, stabilityBare) {
					t.Errorf("tick %d: liveOpenSessionAssignmentExists(%q) = false", tick, stabilityBare)
				}
				rows := stabilityOpenRows(t, store)
				if len(rows) != 1 || rows[0].ID != row.ID {
					t.Fatalf("tick %d: open rows = %v, want exactly %s", tick, rows, row.ID)
				}
				if got := sp.CountCalls("Stop", stabilityBare); got != stops {
					t.Errorf("tick %d: the runtime was stopped (%d stops, want %d)", tick, got, stops)
				}
				if got := sp.CountCalls("Start", stabilityBare); got != starts {
					t.Errorf("tick %d: the runtime was started again (%d starts, want %d)", tick, got, starts)
				}
			}
		})
	}
}

// stabilityBareRow is the live row the pre-adopt config minted, in the census
// shape: bare identity, alias and session name everywhere, started, with the
// stale baseline a config-drift compare would trip over.
func stabilityBareRow(mode string) beads.Bead {
	return sessionRow("gc-1",
		"template", stabilityBare, "state", "active",
		"session_name", stabilityBare, "alias", stabilityBare, "agent_name", stabilityBare,
		"command", "true",
		namedSessionMetadataKey, "true",
		namedSessionIdentityMetadata, stabilityBare,
		"continuity_eligible", "true",
		"configured_named_mode", mode,
		"started_config_hash", "v6:stale-core",
		"started_live_hash", "v6:stale-live",
		"live_hash", "v6:stale-live",
		"started_provision_hash", "v6:stale-prov",
		"started_launch_hash", "v6:stale-launch",
		"core_hash_breakdown", "{}",
	)
}

// stabilityStableBeads checks the AC-3 invariants on one pass's settled
// state: the work still claimed by the bare assignee through the live row,
// the row the only open one, and no runtime churn.
func stabilityStableBeads(t *testing.T, s *sim, pass int) {
	t.Helper()
	snap := s.snapshot()
	if w := snap[simCityLeg+"/gw-gc-1"]; w.Status != "in_progress" || w.Assignee != stabilityBare {
		t.Errorf("pass %d: work = %s/%q, want in_progress assigned %q", pass, w.Status, w.Assignee, stabilityBare)
	}
	if r := snap[simCityLeg+"/gc-1"]; r.Status == "closed" {
		t.Errorf("pass %d: the row was closed", pass)
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 {
		t.Errorf("pass %d: kills %v starts %v, want no runtime churn", pass, s.sp.kills, s.sp.starts)
	}
	open := 0
	for _, b := range snap {
		if b.Type == sessionBeadType && b.Status != "closed" && strings.Contains(b.Metadata[namedSessionIdentityMetadata], "golden-hon") {
			open++
		}
	}
	if open != 1 {
		t.Errorf("pass %d: %d open rows exist for the named session, want exactly one", pass, open)
	}
	if !liveOpenSessionAssignmentExists(s.legs[0].backing, stabilityBare) {
		t.Errorf("pass %d: liveOpenSessionAssignmentExists(%q) = false", pass, stabilityBare)
	}
}

// AC-3, v2 half: three consecutive planner passes in each mode keep the
// requalified row stable under the same invariants.
func TestSimRequalificationIsStableAcrossThreePasses(t *testing.T) {
	for _, mode := range []string{"always", "on_demand"} {
		t.Run(mode, func(t *testing.T) {
			row := stabilityBareRow(mode)
			work := beads.Bead{ID: "gw-gc-1", Title: "work", Type: "task", Status: "in_progress", Assignee: stabilityBare, Metadata: map[string]string{"gc.routed_to": "worker"}}
			s := newSim(t, 3, simOpts{rows: func(s *sim) (city, rig []beads.Bead) {
				// The post-adopt config: `gc rig add --adopt` requalified
				// golden-hon to, live in the same pass's config.
				s.cfg.Rigs = append(s.cfg.Rigs, config.Rig{Name: "contentbuild", Path: t.TempDir()})
				s.cfg.Agents = append(s.cfg.Agents, config.Agent{Name: stabilityBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"})
				s.cfg.NamedSessions = []config.NamedSession{{Template: stabilityBare, Dir: "contentbuild", BindingName: "lane", Mode: mode}}
				s.sp.put(row.Metadata["session_name"], row.ID, row.Metadata["generation"], row.Metadata["instance_token"])
				return []beads.Bead{row, work}, nil
			}})
			s.lag = false
			for pass := 1; pass <= 3; pass++ {
				s.advance(simPatrol)
				s.settle()
				stabilityStableBeads(t, s, pass)
			}
			s.noViolations(t)
		})
	}
}
