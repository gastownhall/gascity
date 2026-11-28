package materialize

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sinkUnionEntry(name, source string) SkillEntry {
	return SkillEntry{Name: name, Source: source}
}

func sinkUnionDesired(u *SinkUnion) map[string]string {
	out := make(map[string]string)
	for _, e := range u.Desired() {
		out[e.Name] = e.Source
	}
	return out
}

func TestSinkUnionUnionsWritersSkills(t *testing.T) {
	u := NewSinkUnion()
	shared := CityCatalog{Entries: []SkillEntry{sinkUnionEntry("plan", "/pack/plan")}, OwnedRoots: []string{"/pack"}}
	u.Add("a", shared, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("a-notes", "/a/a-notes")}, OwnedRoot: "/a"})
	u.Add("b", shared, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("b-notes", "/b/b-notes")}, OwnedRoot: "/b"})

	want := map[string]string{"plan": "/pack/plan", "a-notes": "/a/a-notes", "b-notes": "/b/b-notes"}
	if got := sinkUnionDesired(u); !reflect.DeepEqual(got, want) {
		t.Errorf("Desired = %v, want %v", got, want)
	}
	if got, want := u.OwnedRoots(), []string{"/pack", "/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("OwnedRoots = %v, want %v", got, want)
	}
	if p := u.Problems(); len(p) != 0 {
		t.Errorf("Problems = %v, want none", p)
	}
	if got := u.WantedBy("b-notes"); got != "b" {
		t.Errorf("WantedBy(b-notes) = %q, want b", got)
	}
	if got := u.WantedBy("plan"); got != "a" {
		t.Errorf("WantedBy(plan) = %q, want a (first writer)", got)
	}
	if got := u.AgentList(); got != `"a", "b"` {
		t.Errorf("AgentList = %q", got)
	}
}

func TestSinkUnionLocalOverrideWinsSinkWide(t *testing.T) {
	for _, localFirst := range []bool{true, false} {
		u := NewSinkUnion()
		shared := CityCatalog{Entries: []SkillEntry{sinkUnionEntry("plan", "/pack/plan")}}
		add := []func(){
			func() { u.Add("m", shared, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("plan", "/m/plan")}}) },
			func() { u.Add("d", shared, AgentCatalog{}) },
		}
		if !localFirst {
			add[0], add[1] = add[1], add[0]
		}
		for _, f := range add {
			f()
		}
		if got := sinkUnionDesired(u)["plan"]; got != "/m/plan" {
			t.Errorf("localFirst=%v: plan -> %q, want the agent-local override", localFirst, got)
		}
		if got := u.WantedBy("plan"); got != "m" {
			t.Errorf("localFirst=%v: WantedBy(plan) = %q, want m", localFirst, got)
		}
		if p := u.Problems(); len(p) != 0 {
			t.Errorf("localFirst=%v: Problems = %v", localFirst, p)
		}
	}
}

func TestSinkUnionSameClassClash(t *testing.T) {
	u := NewSinkUnion()
	u.Add("m", CityCatalog{}, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("notes", "/m/notes")}})
	u.Add("r", CityCatalog{}, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("notes", "/r/notes")}})
	problems := u.Problems()
	if len(problems) != 1 {
		t.Fatalf("Problems = %v, want one clash", problems)
	}
	for _, want := range []string{`"notes"`, `"m"`, "/m/notes", `"r"`, "/r/notes"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("clash %q does not name %s", problems[0], want)
		}
	}
}

func TestSinkUnionLocalOverrideSettlesSharedClash(t *testing.T) {
	u := NewSinkUnion()
	u.Add("fe", CityCatalog{Entries: []SkillEntry{sinkUnionEntry("review", "/a/review")}}, AgentCatalog{})
	u.Add("be", CityCatalog{Entries: []SkillEntry{sinkUnionEntry("review", "/b/review")}}, AgentCatalog{})
	if len(u.Problems()) != 1 {
		t.Fatalf("shared clash not reported: %v", u.Problems())
	}
	u.Add("ops", CityCatalog{}, AgentCatalog{Entries: []SkillEntry{sinkUnionEntry("review", "/ops/review")}})
	if p := u.Problems(); len(p) != 0 {
		t.Errorf("agent-local override did not settle the shared clash: %v", p)
	}
	if got := sinkUnionDesired(u)["review"]; got != "/ops/review" {
		t.Errorf("review -> %q, want the agent-local override", got)
	}
}

func TestSinkUnionCoalescesAliasedSources(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	u := NewSinkUnion()
	u.Add("a", CityCatalog{Entries: []SkillEntry{sinkUnionEntry("notes", filepath.Join(realDir, "notes"))}}, AgentCatalog{})
	u.Add("b", CityCatalog{Entries: []SkillEntry{sinkUnionEntry("notes", filepath.Join(alias, "notes"))}}, AgentCatalog{})
	if p := u.Problems(); len(p) != 0 {
		t.Errorf("one directory reached through two paths reported as a clash: %v", p)
	}
}

func TestSinkUnionUnreadableCatalogIsAProblem(t *testing.T) {
	u := NewSinkUnion()
	u.Add("a", CityCatalog{}, AgentCatalog{})
	u.MarkUnreadable(`agent "a": LoadAgentCatalog "/a": permission denied`)
	problems := u.Problems()
	if len(problems) != 1 || !strings.Contains(problems[0], "LoadAgentCatalog") {
		t.Errorf("Problems = %v, want the unreadable catalog", problems)
	}
}

func TestSinkUnionListsAgentOnce(t *testing.T) {
	u := NewSinkUnion()
	u.Add("a", CityCatalog{}, AgentCatalog{})
	u.Add("a", CityCatalog{}, AgentCatalog{})
	if got := u.Agents(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Agents = %v, want [a]", got)
	}
}
