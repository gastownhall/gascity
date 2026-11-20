package materialize

import (
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

	t.Run("no members", func(t *testing.T) {
		got := UnionSinkMembers(nil)
		if len(got.Desired) != 0 || len(got.OwnedRoots) != 0 {
			t.Fatalf("UnionSinkMembers(nil) = %#v, want empty", got)
		}
	})
}
