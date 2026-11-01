//go:build !windows

package main

import (
	"bytes"
	"fmt"
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
// split city, live and as a dry run, with --graph pointed at a private copy of
// the plan judged (bd_split_judged_files.go) and the argv otherwise unchanged.
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
			read := bdReadsFile(t, bdflags.InventoryByName(inventory), scope, g.args, "--graph", "")
			if len(g.args) != len(argv)+1 || !slices.Equal(g.args[:len(argv)], argv) || g.args[len(argv)] != "--graph="+read || read == filepath.Join(scope, "plan.json") {
				t.Errorf("%s: gc bd %s forwarded %q, want the argv with --graph pointed at a private copy", inventory, strings.Join(argv, " "), g.args)
			}
			if got, err := os.ReadFile(read); err != nil || string(got) != graphJudgeWorkOnlyPlan {
				t.Errorf("%s: gc bd %s: bd reads %q (err %v), want the plan judged", inventory, strings.Join(argv, " "), got, err)
			}
			if g.cleanup != nil {
				g.cleanup()
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
		{"JSON null gc.root_bead_id, graph as a native Dolt store reads it", `{"nodes":[` + kick + `,{"key":"c1","title":"c","metadata":{"gc.root_bead_id":null}}]}`, true, "native Dolt store"},
		{"JSON null on two class keys names the deciding one", `{"nodes":[{"key":"a","title":"a","metadata":{"gc.kind":null,"gc.root_bead_id":null}}]}`, true, "gc.root_bead_id differently"},
		{"JSON null gc.root_bead_id beside a spliced metadata_ref", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","metadata":{"gc.root_bead_id":null},"metadata_refs":{"kickoff_id":"a"}}]}`, true, "native Dolt store"},
		{"JSON null session_id on an own-session gate", `{"nodes":[{"key":"a","title":"a","type":"gate","labels":["session:null"],"metadata":{"session_id":null}}]}`, true, "sessions-class"},
		{"case-variant Labels", `{"nodes":[{"key":"a","title":"a","Labels":["gc:session"]}]}`, true, "sessions-class"},
		{"padded label", `{"nodes":[{"key":"a","title":"a","labels":[" gc:session"]}]}`, true, "sessions-class"},
		{"gc.kind wisp", `{"nodes":[{"key":"a","title":"a","metadata":{"gc.kind":"wisp"}}]}`, true, "class"},
		{"class key in metadata_refs", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","metadata_refs":{"gc.root_bead_id":"a"}}]}`, true, "graph-class"},
		{"metadata_refs writes over its key", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","metadata":{"gc.root_bead_id":""},"metadata_refs":{"gc.root_bead_id":"a"}}]}`, true, "graph-class"},
		{"metadata_refs naming an explicit id a label names", `{"nodes":[{"key":"s","title":"s","id":"demo-s1"},{"key":"w","title":"w","type":"gate","labels":["session:demo-s1"],"metadata_refs":{"session_id":"s"}}]}`, true, "sessions-class"},
		{"metadata_refs naming a minted id a label could name", `{"nodes":[{"key":"s","title":"s"},{"key":"w","title":"w","type":"gate","labels":["session:demo-7"],"metadata_refs":{"session_id":"s"}}]}`, true, "sessions-class"},
		{"raw spelling only", `{"nodes":[{"key":"g","title":"g","type":"gate","labels":["session:S "],"metadata":{"session_id":"S "}}]}`, true, "sessions-class"},
		{"plan id the class binding holds, named by an edge", `{"nodes":[{"key":"a","title":"a","id":"relic-1"},{"key":"b","title":"b"}],"edges":[{"from_key":"b","to_id":"relic-1"}]}`, true, "class binding holds a bead with id relic-1"},
		{"padded id names no plan node", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b"}],"edges":[{"from_key":"b","to_id":" demo-x1"}]}`, true, "whitespace"},
		{"padded to_id naming a work bead", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":" demo-1","type":"blocks"}]}`, true, "whitespace"},
		{"padded from_id naming a work bead", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_id":"demo-1 ","to_key":"a"}]}`, true, "whitespace"},
		{"whitespace-only to_id", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":" "}]}`, true, "whitespace"},
		{"padded parent_id naming a work bead", `{"nodes":[{"key":"a","title":"a","parent_id":"\tdemo-2"}]}`, true, "whitespace"},
		{"padded deps target naming a work bead", `{"nodes":[{"key":"a","title":"a","deps":[{"target":"demo-1 "}]}]}`, true, "whitespace"},
		{"whitespace-only deps target", `{"nodes":[{"key":"a","title":"a","deps":[{"target":" "}]}]}`, true, "whitespace"},
		{"padded spawner_id naming a work bead", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":"demo-1","type":"waits-for","spawner_id":" demo-1"}]}`, true, "whitespace"},
		{"padded thread_id naming a work bead", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b"}],"edges":[{"from_key":"a","to_key":"b","type":"replies-to","thread_id":" demo-1"}]}`, true, "whitespace"},
		{"reserved explicit id", `{"nodes":[{"key":"a","title":"a","id":"gcg-abc"}]}`, true, "reserved namespace"},
		{"explicit id the class binding holds", `{"nodes":[{"key":"a","title":"a","id":"relic-1"}]}`, true, "class binding holds a bead with id relic-1"},
		{"a second node's id the class binding holds", `{"nodes":[{"key":"a","title":"a","id":"demo-9"},{"key":"b","title":"b","id":"relic-1"}]}`, true, "relic-1"},
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
		{"JSON null on keys no reading sets", `{"nodes":[{"key":"a","title":"a","metadata":{"gc.kind":null,"gc.formula_contract":null,"note":null}}]}`, false, ""},
		{"edge to a work bead", `{"nodes":[{"key":"a","title":"a"}],"edges":[{"from_key":"a","to_id":"demo-1"}]}`, false, ""},
		{"parent_id naming a work bead", `{"nodes":[{"key":"a","title":"a","parent_id":"demo-2"}]}`, false, ""},
		{"deps target that is a plan key", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","deps":[{"target":"a"}]}]}`, false, ""},
		{"deps target that is a padded plan key", `{"nodes":[{"key":" a","title":"a"},{"key":"b","title":"b","deps":[{"target":" a"}]}]}`, false, ""},
		{"external: dep", `{"nodes":[{"key":"a","title":"a","deps":[{"target":"external:p:cap"}]}]}`, false, ""},
		{"free explicit id", `{"nodes":[{"key":"a","title":"a","id":"demo-9"}]}`, false, ""},
		{"deps target naming a plan id", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b","deps":[{"target":"demo-x1"}]}]}`, false, ""},
		{"edge to_id naming a plan id", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b"}],"edges":[{"from_key":"b","to_id":"demo-x1"}]}`, false, ""},
		{"edge from_id naming a plan id", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b"}],"edges":[{"from_id":"demo-x1","to_key":"b"}]}`, false, ""},
		{"parent_id naming a plan id", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b","parent_id":"demo-x1"}]}`, false, ""},
		{"thread_id naming a plan id", `{"nodes":[{"key":"a","title":"a","id":"demo-x1"},{"key":"b","title":"b"},{"key":"c","title":"c"}],"edges":[{"from_key":"b","to_key":"c","type":"replies-to","thread_id":"demo-x1"}]}`, false, ""},
		{"parent_id beside parent_key", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","parent_key":"a","parent_id":"gcg-p"}]}`, false, ""},
		{"parent_id beside parent", `{"nodes":[{"key":"a","title":"a"},{"key":"b","title":"b","parent":"a","parent_id":"gcg-p"}]}`, false, ""},
		{"metadata_refs naming an explicit id no label names", `{"nodes":[{"key":"s","title":"s","id":"demo-s1"},{"key":"w","title":"w","type":"gate","labels":["session:demo-s2"],"metadata_refs":{"session_id":"s"}}]}`, false, ""},
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

// TestGcBdSplitCreateGraphMetadataRefsOnAGraphHoldingSplit: on a partial
// split whose work store holds the graph class, a node whose metadata_refs
// writes gc.root_bead_id is no flip; on a full split it is refused as a
// graph-class bead.
func TestGcBdSplitCreateGraphMetadataRefsOnAGraphHoldingSplit(t *testing.T) {
	city := splitGuardTestCity()
	city.Storage.Classes.Graph = config.StorageWorkBinding
	for _, plan := range []string{
		`{"nodes":[{"key":"root","title":"r"},{"key":"s","title":"s","metadata_refs":{"gc.root_bead_id":"root"}}]}`,
		`{"nodes":[{"key":"root","title":"r","id":"demo-r1"},{"key":"s","title":"s","metadata_refs":{"gc.root_bead_id":"root"}}]}`,
	} {
		scope := writeGraphPlan(t, plan)
		for _, inventory := range graphJudgeInventories {
			if g := bdClassFlipGuard(city, scope, []string{"create", "--graph", "plan.json"}, graphJudgeDeps(inventory)); g.refusal != "" {
				t.Errorf("%s: %s on a graph-on-work split was refused: %q", inventory, plan, g.refusal)
			}
			if g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"create", "--graph", "plan.json"}, graphJudgeDeps(inventory)); !strings.Contains(g.refusal, "graph-class") {
				t.Errorf("%s: %s on a full split was not refused as graph-class: %q", inventory, plan, g.refusal)
			}
		}
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

// TestBdCreateFileSelectionUsesTheFinalOccurrence covers file selection without
// reinterpreting separated flag operands as flags.
func TestBdCreateFileSelectionUsesTheFinalOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		args           []string
		form           fileCreateForm
		path           string
	}{
		{"argv only", "create", []string{"--title", "x"}, fileCreateNone, ""},
		{"separated graph replaces graph", "create", []string{"--graph", "infra.json", "--graph", "work.json"}, fileCreateGraph, "work.json"},
		{"inline graph replaces graph", "create", []string{"--graph=infra.json", "--graph=work.json"}, fileCreateGraph, "work.json"},
		{"separated graph replaces inline", "create", []string{"--graph=infra.json", "--graph", "work.json"}, fileCreateGraph, "work.json"},
		{"inline empty graph", "create", []string{"--graph", "infra.json", "--graph="}, fileCreateGraph, ""},
		{"separated empty graph", "create", []string{"--graph", "infra.json", "--graph", ""}, fileCreateGraph, ""},
		{"missing final graph operand", "create", []string{"--graph", "infra.json", "--graph"}, fileCreateGraph, ""},
		{"bulk replaces graph", "create", []string{"--graph", "work.json", "--file", "issues.md"}, fileCreateBulk, "issues.md"},
		{"inline bulk replaces graph", "create", []string{"--graph", "work.json", "--file=issues.md"}, fileCreateBulk, "issues.md"},
		{"attached bulk replaces graph", "create", []string{"--graph", "work.json", "-fissues.md"}, fileCreateBulk, "issues.md"},
		{"short bulk replaces graph", "create", []string{"--graph", "work.json", "-f", "issues.md"}, fileCreateBulk, "issues.md"},
		{"graph replaces bulk", "create", []string{"-fold.md", "--graph=work.json"}, fileCreateGraph, "work.json"},
		{"bulk replaces bulk", "create", []string{"--file", "old.md", "-fnew.md"}, fileCreateBulk, "new.md"},
		{"graph operand resembles bulk flag", "create", []string{"--graph", "old.json", "--graph", "--file"}, fileCreateGraph, "--file"},
		{"bulk operand resembles graph flag", "create", []string{"--file", "--graph", "--graph=work.json"}, fileCreateGraph, "work.json"},
		{"title operand resembles graph flag", "create", []string{"--graph=work.json", "--title", "--graph"}, fileCreateGraph, "work.json"},
		{"title operand resembles bulk flag", "create", []string{"--title", "-f", "--graph=work.json"}, fileCreateGraph, "work.json"},
		{"verb has no file form", "q", []string{"--graph", "work.json", "--file=issues.md", "-fmore.md"}, fileCreateNone, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form, path := bdRelocatedClassCreateFileForm(tc.manifest, tc.args)
			if form != tc.form || path != tc.path {
				t.Fatalf("file selection = (%v, %q), want (%v, %q)", form, path, tc.form, tc.path)
			}
		})
	}
}

// finalGraphFlagCity records the forwarded argv and the bytes bd reads from
// the judge's private copy, while that copy still exists.
func finalGraphFlagCity(t *testing.T) (capture, cityDir string) {
	t.Helper()
	capture, cityDir = bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	for name, body := range map[string]string{
		"infra.json": `{"nodes":[{"key":"s","title":"session","labels":["gc:session"]}]}`,
		"work.json":  graphJudgeWorkOnlyPlan,
	} {
		if err := os.WriteFile(filepath.Join(cityDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := t.TempDir()
	stub := `#!/bin/sh
case "$1" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac
printf '%s\n' "$*" >> "$CAPTURE_PATH"
for arg in "$@"; do
  case "$arg" in
    --graph=?*)
      copy_path="${arg#--graph=}"
      printf 'COPY %s\n' "$copy_path" >> "$CAPTURE_PATH"
      cat "$copy_path" >> "$CAPTURE_PATH" || exit 1
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return capture, cityDir
}

// TestGcBdSplitCreateGraphUsesTheFinalWorkPayload proves final payload selection
// and the private-copy lifetime across the doBd subprocess boundary.
func TestGcBdSplitCreateGraphUsesTheFinalWorkPayload(t *testing.T) {
	capture, cityDir := finalGraphFlagCity(t)
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--graph", "infra.json", "--graph", "work.json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("final work plan: exit %d, want 0; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	argv, copyRecord, ok := strings.Cut(string(data), "\nCOPY ")
	copyPath, payload, hasPayload := strings.Cut(copyRecord, "\n")
	if !ok || !hasPayload || copyPath == "" || argv != "create --graph infra.json --graph work.json --graph="+copyPath {
		t.Fatalf("bd did not receive the original argv followed by a private-copy override: %q", data)
	}
	if !filepath.IsAbs(copyPath) {
		copyPath = filepath.Join(cityDir, copyPath)
	}
	if copyPath == filepath.Join(cityDir, "infra.json") || copyPath == filepath.Join(cityDir, "work.json") {
		t.Fatalf("bd read the caller's file %q instead of a private copy", copyPath)
	}
	if payload != graphJudgeWorkOnlyPlan {
		t.Errorf("bd read %q, want the final work.json bytes", payload)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("private copy outlived doBd: stat %q: %v", copyPath, err)
	}
}

// TestGcBdSplitCreateGraphRefusesTheFinalSessionNode identifies the refused node
// before invoking bd, even when an earlier file is work-only.
func TestGcBdSplitCreateGraphRefusesTheFinalSessionNode(t *testing.T) {
	capture, _ := finalGraphFlagCity(t)
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--graph", "work.json", "--graph", "infra.json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("final session plan: exit %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `node "s"`) || !strings.Contains(stderr.String(), "sessions-class") {
		t.Errorf("refusal must identify the disallowed sessions-class node: %q", stderr.String())
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Errorf("bd was invoked for a refused plan: stat %q: %v", capture, err)
	}
}

// TestGcBdSplitCreateEmptyFinalGraphUsesTheArgvShape keeps an empty final graph
// from inheriting the superseded file's storage class.
func TestGcBdSplitCreateEmptyFinalGraphUsesTheArgvShape(t *testing.T) {
	capture, _ := finalGraphFlagCity(t)
	args := []string{"create", "--graph", "infra.json", "--graph=", "--title", "x"}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("empty final graph with a work-shaped argv: exit %d, want 0; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.Join(args, " ")+"\n" {
		t.Fatalf("bd received %q, want the argv-shaped create forwarded verbatim", data)
	}
}

// graphPlanPipe returns the /dev/fd path of a pipe holding body, as a shell
// passes `<(...)`: it can be read once.
func graphPlanPipe(t *testing.T, body string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := w.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("/dev/fd/%d", r.Fd())
}

// TestGcBdSplitCreateGraphEndToEnd runs gc bd create --graph through doBd on a
// split city: a work-only plan, in a file or on a pipe, reaches bd as a
// private copy of the plan judged, removed when gc bd returns, and a mixed
// plan, or a graph plan on a pipe, is refused before bd starts, with the node
// and class named and a gc_bd.refused event recorded.
func TestGcBdSplitCreateGraphEndToEnd(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	dir := t.TempDir()
	work := filepath.Join(dir, "work.json")
	if err := os.WriteFile(work, []byte(graphJudgeWorkOnlyPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pipe reaches bd only when the node-by-node judge's read is the one
	// that drains it.
	for _, plan := range []string{work, graphPlanPipe(t, graphJudgeWorkOnlyPlan)} {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"create", "--graph", plan, "--json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("work-only plan %s: exit %d; stderr=%q", plan, code, stderr.String())
		}
		data, err := os.ReadFile(capture)
		_, copyPath, ok := strings.Cut(string(data), "create --graph "+plan+" --json --graph=")
		copyPath, _, _ = strings.Cut(copyPath, "\n")
		if err != nil || !ok || copyPath == "" || copyPath == plan {
			t.Fatalf("bd was not pointed at a private copy of the plan %s: %q (err %v)", plan, data, err)
		}
		if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
			t.Errorf("the copy %s outlived gc bd (stat err %v)", copyPath, err)
		}
		if err := os.Remove(capture); err != nil {
			t.Fatal(err)
		}
	}

	mixed := filepath.Join(dir, "mixed.json")
	if err := os.WriteFile(mixed, []byte(`{"nodes":[{"key":"root","title":"r"},{"key":"s","title":"s","labels":["gc:session"],"parent_key":"root"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ plan, node, class string }{
		{mixed, `node "s"`, "sessions-class"},
		// The relocated-class guard leaves a pipe unread, so the judge is
		// the one that refuses a graph plan on it.
		{graphPlanPipe(t, `{"nodes":[{"key":"root","title":"r","metadata":{"gc.root_bead_id":"gcg-abc123"}}]}`), `node "root"`, "graph-class"},
	} {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"create", "--graph", c.plan}, &stdout, &stderr); code != 1 {
			t.Fatalf("plan %s: exit %d, want 1; stderr=%q", c.plan, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), c.node) || !strings.Contains(stderr.String(), c.class) {
			t.Errorf("plan %s: the refusal does not name the %s and %s: %q", c.plan, c.node, c.class, stderr.String())
		}
		if _, err := os.Stat(capture); !os.IsNotExist(err) {
			t.Fatalf("bd ran for refused plan %s (stat err %v)", c.plan, err)
		}
	}
	events, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("no events log: %v", err)
	}
	if !strings.Contains(string(events), `"type":"gc_bd.refused"`) {
		t.Fatalf("no gc_bd.refused event for the refused plan:\n%s", events)
	}
}
