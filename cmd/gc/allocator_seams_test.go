package main

import (
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Tests for the seams P3-5a cut into legacy so the allocator's pass can reuse
// its code without I/O: the demand merge, the plan-only work dir, and the
// awake input builder. Legacy calls each in place.

// TestMergeCollectedDemandMatchesPreRefactor pins the extracted demand merge
// against the inline block it replaced, over random collections: clamps,
// custom versus default counts, partial sets, control-dispatcher demand and
// retention, and the ready routed work the counts select.
func TestMergeCollectedDemandMatchesPreRefactor(t *testing.T) {
	templates := []string{"pool-a", "pool-b", "rig/pool-c", config.ControlDispatcherAgentName}
	cfg := &config.City{Agents: []config.Agent{
		{Name: "pool-a"},
		{Name: "pool-b"},
		{Name: "pool-c", Dir: "rig"},
		{Name: config.ControlDispatcherAgentName, StartCommand: "gc convoy control --serve"},
	}}
	boolSet := func(r *rand.Rand) map[string]bool {
		if r.Intn(3) == 0 {
			return nil
		}
		out := make(map[string]bool)
		for _, tmpl := range templates {
			if r.Intn(3) == 0 {
				out[tmpl] = r.Intn(4) != 0
			}
		}
		return out
	}
	for seed := int64(0); seed < 400; seed++ {
		r := rand.New(rand.NewSource(seed))
		var rows []beads.Bead
		var refs []string
		for i := 0; i < r.Intn(6); i++ {
			b := beads.Bead{ID: fmt.Sprintf("w-%d", i), Status: "open", Metadata: map[string]string{}}
			if r.Intn(4) == 0 {
				b.Assignee = "someone"
			}
			if r.Intn(2) == 0 {
				b.Metadata[beadmeta.RoutedToMetadataKey] = templates[r.Intn(len(templates))]
			}
			rows = append(rows, b)
			refs = append(refs, []string{"city", "rig"}[r.Intn(2)])
		}
		custom := map[string]int{}
		for _, tmpl := range templates {
			if r.Intn(2) == 0 {
				custom[tmpl] = r.Intn(4)
			}
		}
		defaultCounts := map[string]int{}
		defaultDemand := map[string]scaleCheckDemand{}
		for _, tmpl := range templates {
			if r.Intn(2) == 0 {
				n := r.Intn(4)
				defaultCounts[tmpl] = n
				d := scaleCheckDemand{StoreRefs: map[string]string{}, Titles: map[string]string{}}
				for i := 0; i < n && i < len(rows); i++ {
					id := rows[r.Intn(len(rows))].ID
					d.WorkBeadIDs = append(d.WorkBeadIDs, id)
					d.StoreRefs[id] = refs[0]
					d.Titles[id] = "t-" + id
				}
				defaultDemand[tmpl] = d
			}
		}
		in := collectedDemand{
			CustomCounts:            custom,
			CustomPartials:          boolSet(r),
			DefaultProbed:           r.Intn(4) != 0,
			DefaultCounts:           defaultCounts,
			DefaultDemand:           defaultDemand,
			DefaultPartials:         boolSet(r),
			ColdWakeTemplates:       boolSet(r),
			NamedOnDemandTemplates:  boolSet(r),
			UnassignedRouted:        rows,
			UnassignedRoutedRefs:    refs,
			UnassignedRoutedPartial: r.Intn(3) == 0,
			NamedPartials:           boolSet(r),
		}
		namedProbed := in.NamedPartials != nil
		want := mergeDemandPreRefactor(cfg, maps.Clone(in.CustomCounts), maps.Clone(in.CustomPartials), in.DefaultProbed,
			in.DefaultCounts, in.DefaultDemand, in.DefaultPartials, in.ColdWakeTemplates, in.NamedOnDemandTemplates,
			rows, refs, in.UnassignedRoutedPartial, namedProbed, in.NamedPartials)
		customBefore, partialsBefore := maps.Clone(in.CustomCounts), maps.Clone(in.CustomPartials)
		got := mergeCollectedDemand(cfg, in)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: mergeCollectedDemand diverged from the inline merge:\n got  %+v\n want %+v", seed, got, want)
		}
		if !maps.Equal(in.CustomCounts, customBefore) || !maps.Equal(in.CustomPartials, partialsBefore) {
			t.Fatalf("seed %d: mergeCollectedDemand edited its input maps", seed)
		}
	}
}

// TestPoolTriggerWorkDirPlanOnlySkipsStaleAncestorCheck pins the plan-only
// work dir: the path is the same, but the stale-ancestor worktree check (a
// filesystem read) does not run in the allocator's pass; legacy keeps it and
// drops the work dir.
func TestPoolTriggerWorkDirPlanOnlySkipsStaleAncestorCheck(t *testing.T) {
	cityPath := t.TempDir()
	// A stale worktree pointer in an ancestor of the work dir: legacy refuses
	// the path (gascity#1556).
	stale := filepath.Join(cityPath, "work")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, ".git"), []byte("gitdir: "+filepath.Join(cityPath, "missing", ".git", "worktrees", "x")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &config.Agent{Name: "worker", WorkDir: "work/{{.AgentBase}}"}
	request := SessionRequest{WorkBeadID: "w-1"}

	legacy := &agentBuildParams{cityPath: cityPath, cityName: "c"}
	if got := poolTriggerWorkDir(legacy, agent, "worker-1", request); got != "" {
		t.Fatalf("legacy work dir = %q; the fixture's stale ancestor must refuse it", got)
	}
	plan := &agentBuildParams{cityPath: cityPath, cityName: "c", planOnly: true}
	want := filepath.Join(cityPath, "work", "worker-1")
	if got := poolTriggerWorkDir(plan, agent, "worker-1", request); got != want {
		t.Fatalf("plan-only work dir = %q, want %q (the plan carries the path; the effect validates it)", got, want)
	}
}

// TestNewAwakeInputFromSnapshotReadsSuspensionThroughCaller pins the awake
// builder's one parameter of note: agent suspension comes from the caller,
// so the allocator supplies it from its inputs instead of a file read.
func TestNewAwakeInputFromSnapshotReadsSuspensionThroughCaller(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "a"}, {Name: "b"}}}
	input := newAwakeInputFromSnapshot(cfg, func(a *config.Agent) bool { return a.Name == "b" },
		nil, nil, nil, nil, nil, nil, nil, nil, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	var suspended []string
	for _, a := range input.Agents {
		if a.Suspended {
			suspended = append(suspended, a.QualifiedName)
		}
	}
	if !slices.Equal(suspended, []string{"b"}) {
		t.Fatalf("suspended agents = %v, want [b]", suspended)
	}
	if input.RunningSessions == nil || input.AttachedSessions == nil || input.PendingSessions == nil {
		t.Fatal("runtime maps must be allocated for the caller to fill")
	}
}
