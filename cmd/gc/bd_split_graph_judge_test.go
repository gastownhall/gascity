//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// graphJudgeWorkOnlyPlan is a planned epic filed in one call: a kickoff task
// held for a human, the epic, three children, and the blocking edges that hold
// the epic and its children on the kickoff. Every node is work-class and every
// edge is plan-local.
const graphJudgeWorkOnlyPlan = `{"nodes":[
 {"key":"kickoff","title":"kickoff","type":"task","labels":["human"]},
 {"key":"epic","title":"the epic","type":"epic"},
 {"key":"c1","title":"child 1","type":"task","parent_key":"epic"},
 {"key":"c2","title":"child 2","type":"task","parent_key":"epic"},
 {"key":"c3","title":"child 3","type":"task","parent_key":"epic"}],
 "edges":[
 {"from_key":"epic","to_key":"kickoff","type":"blocks"},
 {"from_key":"c1","to_key":"kickoff","type":"blocks"},
 {"from_key":"c2","to_key":"kickoff","type":"blocks"},
 {"from_key":"c3","to_key":"kickoff","type":"blocks"}]}`

// graphJudgeInventories are the pinned bd builds every graph-plan row runs
// under.
var graphJudgeInventories = []string{"bd-1.3.2", bdEnterpriseInventory}

// graphJudgeDeps is the runEdgeCases world: demo-1 and demo-2 are work beads,
// relic-1 is a migrated relic the class binding holds, and every other id is
// absent from the work store.
func graphJudgeDeps(inventory string) bdClassGuardDeps {
	present := map[string]bool{"demo-1": true, "demo-2": true, "relic-1": true}
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		if present[id] {
			return beads.Bead{ID: id, Type: "task"}, bdReadFound
		}
		return beads.Bead{}, bdReadNotFound
	})
	deps.held = func(ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = id == "relic-1"
		}
		return out, nil
	}
	deps.inventory = func() (*bdflags.Inventory, error) { return bdflags.InventoryByName(inventory), nil }
	return deps
}

// writeGraphPlan writes body as plan.json in a fresh scope root and returns
// the scope.
func writeGraphPlan(t *testing.T, body string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "plan.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return scope
}

// TestGcBdSplitCreateGraphAdmitsAWorkOnlyPlan: a graph plan whose every node
// is work-class and whose every edge is plan-local is forwarded to bd on a
// split city, live and as a dry run, argv unchanged.
func TestGcBdSplitCreateGraphAdmitsAWorkOnlyPlan(t *testing.T) {
	scope := writeGraphPlan(t, graphJudgeWorkOnlyPlan)
	for _, inventory := range graphJudgeInventories {
		for _, argv := range [][]string{
			{"create", "--graph", "plan.json", "--json"},
			{"create", "--graph=plan.json"},
			{"new", "--graph", "plan.json", "--dry-run"},
		} {
			g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, graphJudgeDeps(inventory))
			if g.refusal != "" {
				t.Errorf("%s: gc bd %s refused: %q", inventory, strings.Join(argv, " "), g.refusal)
				continue
			}
			if !slices.Equal(g.args, argv) {
				t.Errorf("%s: gc bd %s forwarded %q, want argv unchanged", inventory, strings.Join(argv, " "), g.args)
			}
		}
	}
}

// TestGcBdSplitCreateGraphJudgesEveryNode: each node of a plan is classified
// as bd would mint it, each id-named endpoint obeys the cross-store edge rule,
// and a field gc has not vetted is refused. A single-store city never refuses.
func TestGcBdSplitCreateGraphJudgesEveryNode(t *testing.T) {
	kick := `{"key":"kickoff","title":"kickoff","type":"task","labels":["human"]}`
	cases := []struct {
		name, plan string
		refuse     bool
		want       string
	}{
		{"session label on a non-root node", `{"nodes":[` + kick + `,{"key":"c1","title":"c","labels":["gc:session"],"parent_key":"kickoff"}]}`, true, "sessions-class"},
		{"message type on a child", `{"nodes":[` + kick + `,{"key":"c1","title":"c","type":"message","parent_key":"kickoff"}]}`, true, "messaging-class"},
		{"numeric gc.root_bead_id", `{"nodes":[` + kick + `,{"key":"c1","title":"c","metadata":{"gc.root_bead_id":7}}]}`, true, "graph-class"},
		{"duplicate metadata objects merge", `{"nodes":[{"key":"a","title":"a","metadata":{"gc.root_bead_id":"x"},"metadata":{"note":"y"}}]}`, true, "graph-class"},
		{"case-variant Labels", `{"nodes":[{"key":"a","title":"a","Labels":["gc:session"]}]}`, true, "sessions-class"},
		{"padded label", `{"nodes":[{"key":"a","title":"a","labels":[" gc:session"]}]}`, true, "sessions-class"},
		{"gc.kind wisp", `{"nodes":[{"key":"a","title":"a","metadata":{"gc.kind":"wisp"}}]}`, true, "class"},
		{"class key in metadata_refs", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","metadata_refs":{"gc.root_bead_id":"a"}}]}`, true, "metadata_refs"},
		{"reserved explicit id", `{"nodes":[{"key":"a","title":"a","id":"gcg-abc"}]}`, true, "reserved namespace"},
		{"unknown node field", `{"nodes":[{"key":"a","title":"a","inherit_labels":true}]}`, true, "inherit_labels"},
		{"unknown long-s node field", "{\"nodes\":[{\"key\":\"a\",\"title\":\"a\",\"ſtorage_class\":\"x\"}]}", true, "does not know"},
		{"unknown edge field", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b"}],"edges":[{"from_key":"a","to_key":"b","weight":2}]}`, true, "weight"},
		{"unknown dep field", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","deps":[{"target":"a","inherit":true}]}]}`, true, "inherit"},
		{"unknown plan field", `{"nodes":[{"key":"a","title":"a"}],"defaults":{"labels":["gc:session"]}}`, true, "defaults"},
		{"edge to_id naming a binding relic", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":"relic-1"}]}`, true, "class binding"},
		{"edge to_id naming an absent id", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":"gcg-x"}]}`, true, "never written"},
		{"edge to_id beats to_key", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_key":"a","to_id":"gcg-x"}]}`, true, "never written"},
		{"parent_id absent", `{"nodes":[{"key":"a","title":"a","parent_id":"gcg-p"}]}`, true, "never written"},
		{"literal deps target absent", `{"nodes":[{"key":"a","title":"a","deps":[{"target":"gcg-q"}]}]}`, true, "never written"},
		{"thread_id naming a binding relic", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b"}],"edges":[{"from_key":"a","to_key":"b","type":"replies-to","thread_id":"relic-1"}]}`, true, "class binding"},
		{"malformed JSON", `{"nodes":[`, true, "Nothing was written"},
		{"not an object", `nope`, true, "not a JSON object"},
		{"plain metadata_refs", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","metadata_refs":{"kickoff_id":"a"}}]}`, false, ""},
		{"edge to a work bead", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":"demo-1"}]}`, false, ""},
		{"parent_id naming a work bead", `{"nodes":[{"key":"a","title":"a","parent_id":"demo-2"}]}`, false, ""},
		{"deps target that is a plan key", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","deps":[{"target":"a"}]}]}`, false, ""},
		{"external: dep", `{"nodes":[{"key":"a","title":"a","deps":[{"target":"external:p:cap"}]}]}`, false, ""},
	}
	for _, c := range cases {
		for _, inventory := range graphJudgeInventories {
			t.Run(c.name+"/"+inventory, func(t *testing.T) {
				scope := writeGraphPlan(t, c.plan)
				for _, argv := range [][]string{{"create", "--graph", "plan.json", "--json"}, {"new", "--graph=plan.json", "--dry-run"}} {
					g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, graphJudgeDeps(inventory))
					if c.refuse != (g.refusal != "") {
						t.Fatalf("gc bd %s: refused=%v, want %v; refusal=%q", strings.Join(argv, " "), g.refusal != "", c.refuse, g.refusal)
					}
					if c.refuse && (!strings.Contains(g.refusal, c.want) || !strings.Contains(g.refusal, "Nothing was written.")) {
						t.Errorf("gc bd %s: refusal %q lacks %q or 'Nothing was written.'", strings.Join(argv, " "), g.refusal, c.want)
					}
					if single := bdClassFlipGuard(&config.City{}, scope, argv, graphJudgeDeps(inventory)); single.refusal != "" {
						t.Errorf("gc bd %s on a single-store city refused: %q", strings.Join(argv, " "), single.refusal)
					}
				}
			})
		}
	}
}

// TestGcBdSplitCreateGraphUnreadablePlan: gc cannot judge a plan it cannot
// read, so it refuses rather than forwarding it unjudged.
func TestGcBdSplitCreateGraphUnreadablePlan(t *testing.T) {
	g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--graph", "missing.json"}, graphJudgeDeps("bd-1.3.2"))
	if !strings.Contains(g.refusal, "missing.json") || !strings.HasSuffix(g.refusal, "Nothing was written.") {
		t.Fatalf("an unreadable plan was not refused: %q", g.refusal)
	}
}

// TestGcBdSplitCreateGraphPartialSplit: on a partial split that leaves the
// sessions class on the work store, a gc:session node is no flip.
func TestGcBdSplitCreateGraphPartialSplit(t *testing.T) {
	city := splitGuardTestCity()
	city.Storage.Classes.Sessions = config.StorageWorkBinding
	scope := writeGraphPlan(t, `{"nodes":[{"key":"a","title":"a"},{"key":"s","title":"s","labels":["gc:session"],"parent_key":"a"}]}`)
	for _, inventory := range graphJudgeInventories {
		if g := bdClassFlipGuard(city, scope, []string{"create", "--graph", "plan.json"}, graphJudgeDeps(inventory)); g.refusal != "" {
			t.Errorf("%s: a gc:session node on a sessions-on-work split was refused: %q", inventory, g.refusal)
		}
		if g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"create", "--graph", "plan.json"}, graphJudgeDeps(inventory)); !strings.Contains(g.refusal, "sessions-class") {
			t.Errorf("%s: a gc:session node on a full split was not refused: %q", inventory, g.refusal)
		}
	}
}

// TestGcBdSplitCreateGraphFlagEdges: an empty --graph value is bd's single
// create, judged by its flags; a repeated --graph is judged by its last value,
// as pflag and bd read it.
func TestGcBdSplitCreateGraphFlagEdges(t *testing.T) {
	for _, argv := range [][]string{
		{"create", "--graph", "", "--title", "x", "--labels", "gc:session"},
		{"create", "--graph=", "--title", "x", "--labels", "gc:session"},
	} {
		g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, graphJudgeDeps("bd-1.3.2"))
		if !strings.Contains(g.refusal, "sessions-class") {
			t.Errorf("gc bd %q: an empty --graph did not fall through to the single-create judge: %q", argv, g.refusal)
		}
	}
	scope := writeGraphPlan(t, graphJudgeWorkOnlyPlan)
	if err := os.WriteFile(filepath.Join(scope, "infra.json"), []byte(`{"nodes":[{"key":"s","title":"s","labels":["gc:session"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"create", "--graph", "plan.json", "--graph", "infra.json"}, graphJudgeDeps("bd-1.3.2")); !strings.Contains(g.refusal, "sessions-class") {
		t.Errorf("a last --graph naming an infra plan was not refused: %q", g.refusal)
	}
	if g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"create", "--graph", "infra.json", "--graph", "plan.json"}, graphJudgeDeps("bd-1.3.2")); g.refusal != "" {
		t.Errorf("a last --graph naming a work-only plan was refused: %q", g.refusal)
	}
}

// TestGcBdSplitCreateGraphEndToEnd runs gc bd create --graph through doBd on a
// split city: a work-only plan reaches bd unchanged, and a mixed plan is
// refused before bd starts, with the node and class named and a gc_bd.refused
// event recorded.
func TestGcBdSplitCreateGraphEndToEnd(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	dir := t.TempDir()
	work := filepath.Join(dir, "work.json")
	if err := os.WriteFile(work, []byte(graphJudgeWorkOnlyPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--graph", work, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("work-only plan: exit %d; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil || !strings.Contains(string(data), "create --graph "+work+" --json") {
		t.Fatalf("bd did not receive the plan unchanged: %q (err %v)", data, err)
	}
	if err := os.Remove(capture); err != nil {
		t.Fatal(err)
	}

	mixed := filepath.Join(dir, "mixed.json")
	if err := os.WriteFile(mixed, []byte(`{"nodes":[{"key":"root","title":"r"},{"key":"s","title":"s","labels":["gc:session"],"parent_key":"root"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"create", "--graph", mixed}, &stdout, &stderr); code != 1 {
		t.Fatalf("mixed plan: exit %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `node "s"`) || !strings.Contains(stderr.String(), "sessions-class") {
		t.Errorf("mixed plan refusal does not name the node and class: %q", stderr.String())
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("bd ran for a refused plan (stat err %v)", err)
	}
	events, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("no events log: %v", err)
	}
	if !strings.Contains(string(events), `"type":"gc_bd.refused"`) {
		t.Fatalf("no gc_bd.refused event for the refused plan:\n%s", events)
	}
}
