package materialize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sinkUnionCase is one SinkUnion fold rule, expressed as writers added in
// order and assertions over the resulting fold.
type sinkUnionCase struct {
	name string
	// city entries per writer, as (name, source) pairs; one shared
	// catalog shape per writer keeps the table about the fold rules.
	city [][2]string
	// agent-local entries per writer.
	local [][2]string
	// wantLinks is the desired set as "name -> source" lines.
	wantLinks []string
	// wantHeld is a substring each held-problem line must contain; when
	// set, the fold must hold and wantLinks must be ignored.
	wantHeld []string
	// wantWantedBy maps a name to the writer its skip is attributed to.
	wantWantedBy map[string]string
}

func (tc sinkUnionCase) run(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	entry := func(p [2]string) SkillEntry {
		return SkillEntry{Name: p[0], Source: filepath.Join(root, p[1])}
	}
	union := NewSinkUnion()
	writers := len(tc.city)
	if len(tc.local) > writers {
		writers = len(tc.local)
	}
	for i := range writers {
		city := CityCatalog{}
		if i < len(tc.city) {
			city.Entries = []SkillEntry{entry(tc.city[i])}
		}
		var local AgentCatalog
		if i < len(tc.local) {
			local.Entries = []SkillEntry{entry(tc.local[i])}
		}
		union.AddAgent(writerName(i), city, local)
	}

	if problems := union.Problems(); len(problems) > 0 {
		if len(tc.wantHeld) == 0 {
			t.Fatalf("union held with problems %q, want a reconcilable fold", problems)
		}
		for _, want := range tc.wantHeld {
			found := false
			for _, p := range problems {
				if strings.Contains(p, want) {
					found = true
				}
			}
			if !found {
				t.Errorf("problems %q do not mention %q", problems, want)
			}
		}
		return
	}
	if len(tc.wantHeld) > 0 {
		t.Fatalf("union reconciled, want it held mentioning %q", tc.wantHeld)
	}

	var got []string
	for _, e := range union.Desired() {
		got = append(got, e.Name+" -> "+e.Source)
	}
	if len(got) != len(tc.wantLinks) {
		t.Fatalf("desired = %q, want %q", got, tc.wantLinks)
	}
	for i, want := range tc.wantLinks {
		want = strings.ReplaceAll(want, "$ROOT", root)
		if got[i] != want {
			t.Errorf("desired[%d] = %q, want %q", i, got[i], want)
		}
	}
	for name, want := range tc.wantWantedBy {
		if got := union.WantedBy(name); got != want {
			t.Errorf("WantedBy(%q) = %q, want %q", name, got, want)
		}
	}
}

func writerName(i int) string { return []string{"mayor", "deputy"}[i%2] }

func TestSinkUnionLocalOverrideWinsSinkWide(t *testing.T) {
	sinkUnionCase{
		name:         "agent-local overrides the shared entry for the whole sink",
		city:         [][2]string{{"plan", "shared/plan"}},
		local:        [][2]string{{"plan", "agents/mayor/skills/plan"}},
		wantLinks:    []string{"plan -> $ROOT/agents/mayor/skills/plan"},
		wantWantedBy: map[string]string{"plan": "mayor"},
	}.run(t)
}

func TestSinkUnionSameClassClashHolds(t *testing.T) {
	sinkUnionCase{
		name:      "two agent-local sources for one name clash",
		local:     [][2]string{{"dup", "agents/mayor/skills/dup"}, {"dup", "agents/deputy/skills/dup"}},
		wantHeld:  []string{`skill "dup" is provided by agent "mayor"`, `and by agent "deputy"`},
		wantLinks: nil,
	}.run(t)
}

func TestSinkUnionLocalOverrideSettlesSharedClash(t *testing.T) {
	sinkUnionCase{
		name:  "an agent-local override settles a shared clash",
		city:  [][2]string{{"review", "ops-a/review"}, {"review", "ops-b/review"}},
		local: [][2]string{{"review", "agents/mayor/skills/review"}},
		wantLinks: []string{
			"review -> $ROOT/agents/mayor/skills/review",
		},
	}.run(t)
}

func TestSinkUnionIdenticalSourcesCoalesce(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "ops", "review")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "ops-alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	union := NewSinkUnion()
	union.AddAgent("mayor", CityCatalog{Entries: []SkillEntry{{Name: "ops.review", Source: source}}}, AgentCatalog{})
	union.AddAgent("deputy", CityCatalog{Entries: []SkillEntry{{Name: "ops.review", Source: alias}}}, AgentCatalog{})
	if problems := union.Problems(); len(problems) != 0 {
		t.Fatalf("one source reached through two paths reported as problems %q", problems)
	}
	desired := union.Desired()
	if len(desired) != 1 || desired[0].Name != "ops.review" {
		t.Fatalf("desired = %q, want one ops.review entry", desired)
	}
}

func TestSinkUnionUnreadableCatalogHolds(t *testing.T) {
	union := NewSinkUnion()
	union.AddAgent("mayor", CityCatalog{Entries: []SkillEntry{{Name: "plan", Source: "/city/skills/plan"}}}, AgentCatalog{})
	union.MarkUnreadable(`agent "deputy": LoadAgentCatalog "/city/agents/deputy/skills": permission denied`)
	problems := union.Problems()
	if len(problems) != 1 || !strings.Contains(problems[0], `agent "deputy"`) {
		t.Fatalf("problems = %q, want the unreadable writer named once", problems)
	}
}

func TestSinkUnionSharedSameSourceByTwoWritersIsOneWant(t *testing.T) {
	union := NewSinkUnion()
	shared := CityCatalog{Entries: []SkillEntry{{Name: "plan", Source: "/city/skills/plan"}}}
	union.AddAgent("mayor", shared, AgentCatalog{})
	union.AddAgent("deputy", shared, AgentCatalog{})
	if problems := union.Problems(); len(problems) != 0 {
		t.Fatalf("two writers wanting one shared source reported problems %q", problems)
	}
	if got := union.WantedBy("plan"); got != "mayor" {
		t.Errorf("WantedBy(plan) = %q, want the first writer %q", got, "mayor")
	}
	if got := union.Agents(); len(got) != 2 || got[0] != "mayor" || got[1] != "deputy" {
		t.Errorf("Agents() = %q, want fold order", got)
	}
	owned := union.OwnedRoots()
	if len(owned) != 0 {
		t.Errorf("OwnedRoots() = %q, want none for a catalog without roots", owned)
	}
}
