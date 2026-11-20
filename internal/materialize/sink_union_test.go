package materialize

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func sinkUnionEntry(name, source, origin string) SkillEntry {
	return SkillEntry{Name: name, Source: source, Origin: origin}
}

func sinkUnionDesiredSources(entries []SkillEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Name] = e.Source
	}
	return out
}

func requireSortedByName(t *testing.T, entries []SkillEntry) {
	t.Helper()
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name >= entries[i].Name {
			t.Fatalf("Desired not sorted by name: %q before %q", entries[i-1].Name, entries[i].Name)
		}
	}
}

func TestUnionSinkMembers(t *testing.T) {
	root := t.TempDir()
	packRoot := filepath.Join(root, "skills")
	plan := sinkUnionEntry("plan", filepath.Join(packRoot, "plan"), "city")
	review := sinkUnionEntry("review", filepath.Join(packRoot, "review"), "city")
	shared := CityCatalog{Entries: []SkillEntry{plan, review}, OwnedRoots: []string{packRoot}}

	alphaRoot := filepath.Join(root, "agents", "alpha", "skills")
	notes := sinkUnionEntry("notes", filepath.Join(alphaRoot, "notes"), "agent")
	alpha := AgentCatalog{Entries: []SkillEntry{notes}, OwnedRoot: alphaRoot}

	t.Run("single member equals EffectiveSet", func(t *testing.T) {
		got := UnionSinkMembers([]SinkMember{{Agent: "alpha", Shared: shared, Local: alpha}})
		if want := EffectiveSet(shared, alpha); !reflect.DeepEqual(got.Desired, want) {
			t.Fatalf("Desired = %#v, want %#v", got.Desired, want)
		}
		if want := []string{packRoot, alphaRoot}; !reflect.DeepEqual(got.OwnedRoots, want) {
			t.Fatalf("OwnedRoots = %q, want %q", got.OwnedRoots, want)
		}
	})

	t.Run("union keeps every member's agent-local skill", func(t *testing.T) {
		got := UnionSinkMembers([]SinkMember{
			{Agent: "alpha", Shared: shared, Local: alpha},
			{Agent: "implicit", Shared: shared},
		})
		want := map[string]string{"notes": notes.Source, "plan": plan.Source, "review": review.Source}
		if srcs := sinkUnionDesiredSources(got.Desired); !reflect.DeepEqual(srcs, want) {
			t.Fatalf("Desired sources = %v, want %v", srcs, want)
		}
		requireSortedByName(t, got.Desired)
		if want := []string{packRoot, alphaRoot}; !reflect.DeepEqual(got.OwnedRoots, want) {
			t.Fatalf("OwnedRoots = %q, want %q (deduplicated)", got.OwnedRoots, want)
		}
		if got.WantedBy["notes"] != "alpha" {
			t.Fatalf("WantedBy[notes] = %q, want alpha", got.WantedBy["notes"])
		}
	})

	t.Run("WantedBy is the first member in input order", func(t *testing.T) {
		got := UnionSinkMembers([]SinkMember{
			{Agent: "implicit", Shared: shared},
			{Agent: "alpha", Shared: shared, Local: alpha},
		})
		if got.WantedBy["plan"] != "implicit" || got.WantedBy["notes"] != "alpha" {
			t.Fatalf("WantedBy = %v, want plan->implicit, notes->alpha", got.WantedBy)
		}
	})

	t.Run("agent-local overrides shared in either order", func(t *testing.T) {
		localPlan := sinkUnionEntry("plan", filepath.Join(alphaRoot, "plan"), "agent")
		withPlan := AgentCatalog{Entries: []SkillEntry{localPlan, notes}, OwnedRoot: alphaRoot}
		orders := map[string][]SinkMember{
			"local first":  {{Agent: "alpha", Shared: shared, Local: withPlan}, {Agent: "implicit", Shared: shared}},
			"shared first": {{Agent: "implicit", Shared: shared}, {Agent: "alpha", Shared: shared, Local: withPlan}},
		}
		for order, members := range orders {
			got := UnionSinkMembers(members)
			if src := sinkUnionDesiredSources(got.Desired)["plan"]; src != localPlan.Source {
				t.Fatalf("%s: plan source = %q, want agent-local %q", order, src, localPlan.Source)
			}
			if got.WantedBy["plan"] != "alpha" {
				t.Fatalf("%s: WantedBy[plan] = %q, want alpha", order, got.WantedBy["plan"])
			}
			requireSortedByName(t, got.Desired)
		}
	})

	t.Run("one source seen by several members collapses", func(t *testing.T) {
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(alphaRoot, alias); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(alphaRoot, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		aliased := sinkUnionEntry("notes", filepath.Join(alias, "notes"), "agent")
		got := UnionSinkMembers([]SinkMember{
			{Agent: "alpha", Shared: shared, Local: alpha},
			{Agent: "pool-2", Shared: shared, Local: alpha},
			{Agent: "aliased", Shared: shared, Local: AgentCatalog{Entries: []SkillEntry{aliased}, OwnedRoot: alias}},
		})
		if len(got.Conflicts) != 0 {
			t.Fatalf("Conflicts = %#v, want none", got.Conflicts)
		}
		want := map[string]string{"notes": notes.Source, "plan": plan.Source, "review": review.Source}
		if srcs := sinkUnionDesiredSources(got.Desired); !reflect.DeepEqual(srcs, want) {
			t.Fatalf("Desired sources = %v, want %v", srcs, want)
		}
		if got.WantedBy["notes"] != "alpha" || got.WantedBy["plan"] != "alpha" {
			t.Fatalf("WantedBy = %v, want the first member", got.WantedBy)
		}
	})

	t.Run("agent-local vs agent-local conflict", func(t *testing.T) {
		betaRoot := filepath.Join(root, "agents", "beta", "skills")
		betaNotes := sinkUnionEntry("notes", filepath.Join(betaRoot, "notes"), "agent")
		got := UnionSinkMembers([]SinkMember{
			{Agent: "beta", Shared: shared, Local: AgentCatalog{Entries: []SkillEntry{betaNotes}, OwnedRoot: betaRoot}},
			{Agent: "alpha", Shared: shared, Local: alpha},
		})
		want := []SinkConflict{{Name: "notes", Local: true, Sources: []SinkSource{
			{Agent: "alpha", Source: notes.Source},
			{Agent: "beta", Source: betaNotes.Source},
		}}}
		if !reflect.DeepEqual(got.Conflicts, want) {
			t.Fatalf("Conflicts = %#v, want %#v", got.Conflicts, want)
		}
		if _, ok := sinkUnionDesiredSources(got.Desired)["notes"]; ok {
			t.Fatalf("conflicted name notes is desired: %#v", got.Desired)
		}
		if _, ok := got.WantedBy["notes"]; ok {
			t.Fatalf("conflicted name notes has WantedBy %q", got.WantedBy["notes"])
		}
	})

	opsA := sinkUnionEntry("ops.deploy", filepath.Join(root, "ops-a", "deploy"), "ops")
	opsB := sinkUnionEntry("ops.deploy", filepath.Join(root, "ops-b", "deploy"), "ops")
	rigA := CityCatalog{Entries: []SkillEntry{opsA, plan}, OwnedRoots: []string{packRoot, filepath.Dir(opsA.Source)}}
	rigB := CityCatalog{Entries: []SkillEntry{opsB, plan}, OwnedRoots: []string{packRoot, filepath.Dir(opsB.Source)}}

	t.Run("shared vs shared conflict", func(t *testing.T) {
		got := UnionSinkMembers([]SinkMember{{Agent: "b/beta", Shared: rigB}, {Agent: "a/alpha", Shared: rigA}})
		want := []SinkConflict{{Name: "ops.deploy", Sources: []SinkSource{
			{Agent: "a/alpha", Source: opsA.Source},
			{Agent: "b/beta", Source: opsB.Source},
		}}}
		if !reflect.DeepEqual(got.Conflicts, want) {
			t.Fatalf("Conflicts = %#v, want %#v", got.Conflicts, want)
		}
		if srcs := sinkUnionDesiredSources(got.Desired); !reflect.DeepEqual(srcs, map[string]string{"plan": plan.Source}) {
			t.Fatalf("Desired sources = %v, want only plan", srcs)
		}
		if wantRoots := []string{packRoot, filepath.Dir(opsB.Source), filepath.Dir(opsA.Source)}; !reflect.DeepEqual(got.OwnedRoots, wantRoots) {
			t.Fatalf("OwnedRoots = %q, want %q", got.OwnedRoots, wantRoots)
		}
	})

	t.Run("agent-local entry settles a shared clash", func(t *testing.T) {
		localOps := sinkUnionEntry("ops.deploy", filepath.Join(alphaRoot, "ops.deploy"), "agent")
		got := UnionSinkMembers([]SinkMember{
			{Agent: "b/beta", Shared: rigB},
			{Agent: "a/alpha", Shared: rigA, Local: AgentCatalog{Entries: []SkillEntry{localOps}, OwnedRoot: alphaRoot}},
		})
		if len(got.Conflicts) != 0 {
			t.Fatalf("Conflicts = %#v, want none", got.Conflicts)
		}
		if src := sinkUnionDesiredSources(got.Desired)["ops.deploy"]; src != localOps.Source {
			t.Fatalf("ops.deploy source = %q, want %q", src, localOps.Source)
		}
	})

	t.Run("no members", func(t *testing.T) {
		got := UnionSinkMembers(nil)
		if len(got.Desired) != 0 || len(got.OwnedRoots) != 0 {
			t.Fatalf("UnionSinkMembers(nil) = %#v, want empty", got)
		}
	})
}
