package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// staleSeatForGCB is a seat whose row already triggers gc-b but records the
// directory of gc-a's worktree: a live seat re-pointed at gc-b keeps the dir
// its process runs in (poolTriggerBindWorkDir), and a row bound before gc-b's
// worktree evidence was stamped records the dir it was bound to then.
func staleSeatForGCB(t *testing.T, running bool) (liveSeatReuseResult, string, string) {
	t.Helper()
	_, pathA := liveSeatManagedSpec(t, "gc-a")
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	res := liveSeatReuse{
		seatDir: pathA, running: running,
		extra: map[string]string{beadmeta.TriggerBeadIDMetadataKey: "gc-b"},
		request: newDemand(SessionRequest{
			Tier: "new", WorkBeadID: "gc-b", WorkStoreRef: "rig:r1", WorktreeSpec: &specB,
		}),
	}.reused(t)
	return res, pathA, pathB
}

// A stopped seat bound again for the trigger it already records, whose
// verified worktree is not the dir its row records, starts in that worktree.
// Kept in the recorded dir it would leave its own trigger (the claim's
// worktree check) and drain, and a seat that exits without a drain-ack would
// be reused for the same trigger in the same dir again.
func TestLegacySameTriggerBindMovesAStoppedSeatToItsTriggersWorktree(t *testing.T) {
	res, _, pathB := staleSeatForGCB(t, false)
	row := res.row(t)
	if got := row.Metadata[beadmeta.WorkDirMetadataKey]; got != pathB {
		t.Fatalf("gc.work_dir = %q; want %q, the trigger's verified worktree", got, pathB)
	}
	if got := row.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != pathB {
		t.Fatalf("work_dir = %q; want %q", got, pathB)
	}
}

// A running seat keeps the dir its process runs in, even for its own
// trigger: moving the row would name a directory the process never ran in.
func TestLegacySameTriggerBindKeepsARunningSeatsDir(t *testing.T) {
	res, pathA, _ := staleSeatForGCB(t, true)
	if got := res.row(t).Metadata[beadmeta.WorkDirMetadataKey]; got != pathA {
		t.Fatalf("running seat's gc.work_dir = %q; want it kept at %q, where its process runs", got, pathA)
	}
}

// sameTriggerWorkDirStale is the pure predicate both realizations ask before
// binding a seat that can start: only a same-trigger bind of work with a
// verified worktree spec, recording another dir, is stale.
func TestSameTriggerWorkDirStale(t *testing.T) {
	cityPath := t.TempDir()
	specB, pathB := liveSeatManagedSpec(t, "gc-b")
	row := func(trigger, dir string) session.Info {
		return session.Info{ID: "s-1", TriggerBeadID: trigger, WorkDirCanonical: dir, WorkDir: dir}
	}
	bound := SessionRequest{WorkBeadID: "gc-b", WorktreeSpec: &specB}
	for _, tc := range []struct {
		name    string
		info    session.Info
		request SessionRequest
		workDir string
		want    bool
	}{
		{"same trigger, other dir", row("gc-b", "/wt/gc-a"), bound, pathB, true},
		{"same trigger, its worktree", row("gc-b", pathB), bound, pathB, false},
		{"another trigger", row("gc-a", "/wt/gc-a"), bound, pathB, false},
		{"work bound to no worktree", row("gc-b", "/wt/gc-a"), SessionRequest{WorkBeadID: "gc-b"}, "/pool", false},
		{"no recorded dir", row("gc-b", ""), bound, pathB, false},
		{"no work", row("gc-b", "/wt/gc-a"), SessionRequest{}, pathB, false},
		{"no derived dir", row("gc-b", "/wt/gc-a"), bound, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTriggerWorkDirStale(cityPath, tc.info, tc.request, tc.workDir); got != tc.want {
				t.Fatalf("sameTriggerWorkDirStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// The v2 half: a start candidate whose row already triggers gc-b but records
// gc-a's dir is bound for gc-b even though the binding patch is empty (the
// trigger and store ref are unchanged, and the plan-only work dir is ""), so
// its PreWake verifies gc-b's worktree and stamps its path as the row's work
// dir. Unbound, the seat would start in gc-a's dir, leave its own trigger,
// and drain.
func TestAllocator_SameTriggerStaleWorkDirBindsTheTriggersWorktree(t *testing.T) {
	const pathA, pathB = "/wt/gc-a", "/wt/gc-b"
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 1)}}
	spec := worktree.Spec{BeadID: "gc-b", StoreRef: "city", Path: pathB}
	row := poolRow("gc-1", "worker", 1, "active",
		beadmeta.TriggerBeadIDMetadataKey, "gc-b", beadmeta.TriggerBeadStoreRefMetadataKey, "city",
		beadmeta.WorkDirMetadataKey, pathA, beadmeta.LegacyWorkDirMetadataKey, pathA)
	f := newAllocFixture(t, cfg).sessions(row).demand("worker", "gc-b")
	d0 := f.in.Demand.Collected.DefaultDemand["worker"]
	d0.WorktreeSpecs = map[string]*worktree.Spec{"gc-b": &spec}
	f.in.Demand.Collected.DefaultDemand["worker"] = d0

	e := entryOf(t, f.decide(), "gc-1")
	if !e.InDesired || e.Binding == nil {
		t.Fatalf("indesired=%v binding %+v, want a selected start bound to gc-b's worktree", e.InDesired, e.Binding)
	}
	if e.Binding.WorktreeSpec == nil || *e.Binding.WorktreeSpec != spec || e.Binding.KeptWorkDir != "" {
		t.Fatalf("binding spec %+v kept %q, want spec %+v and no kept dir", e.Binding.WorktreeSpec, e.Binding.KeptWorkDir, spec)
	}
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if got, ok := e.Binding.Patch[key]; ok {
			t.Fatalf("binding patch %s = %q, want the PreWake's verified path to set it", key, got)
		}
	}

	k, _ := launchKit(t, "asleep", beadmeta.TriggerBeadIDMetadataKey, "gc-b",
		beadmeta.WorkDirMetadataKey, pathA, beadmeta.LegacyWorkDirMetadataKey, pathA)
	k.p.Alloc.Snapshot.Entries[k.it.Key] = &selectionEntry{Binding: e.Binding}
	old := verifyWorktree
	verifyWorktree = func(s worktree.Spec) (worktree.Report, error) { return worktree.Report{Path: s.Path}, nil }
	t.Cleanup(func() { verifyWorktree = old })
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want the start landed", s)
	}
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if got := k.meta(key); got != pathB {
			t.Errorf("row %s = %q, want %q, gc-b's verified worktree", key, got, pathB)
		}
	}
}
