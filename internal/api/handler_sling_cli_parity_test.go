package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// These tests pin POST /sling to the behavior `gc sling` has for the same
// inputs. Both now go through sling.(*Sling).Dispatch; before that the API
// kept its own dispatch switch and its own graph-store choice, and a default
// `gc init` city showed the drift: slinging a rig bead to a rig pool (for
// example demo/claude, whose default formula is mol-do-work) cooked the
// workflow into the city store, where the rig's workers never look, so the
// bead was never claimed.

// newDefaultFormulaParityFixture builds a default (non-relocated) city: a
// city store distinct from the rig store, no graph binding, and a rig pool
// target whose default_sling_formula is a graph.v2 formula, like the implicit
// demo/claude agent and mol-do-work.
func newDefaultFormulaParityFixture(t *testing.T, formulaBody string) (http.Handler, *fakeMutatorState, beads.Store, beads.Store, beads.Bead) {
	t.Helper()
	enableGraphV2 := lockGraphV2SlingFlagsForTest(t)

	srv, state := newSlingTestServer(t)
	enableGraphV2()

	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(formulaBody), 0o644); err != nil {
		t.Fatal(err)
	}
	defaultFormula := "graph-work"
	state.cfg.Agents = []config.Agent{
		{
			Name:                "claude",
			Dir:                 "myrig",
			Provider:            "test-agent",
			DefaultSlingFormula: &defaultFormula,
			MinActiveSessions:   intPtr(0),
			MaxActiveSessions:   intPtr(-1),
		},
		{Name: config.ControlDispatcherAgentName, MaxActiveSessions: intPtr(1)},
		{Name: config.ControlDispatcherAgentName, Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}

	// A default city's HQ store is its own database, separate from every rig
	// store, and on a non-relocated city it is also the graph binding.
	cityStore := beads.NewMemStoreFrom(1000, nil, nil)
	state.cityBeadStore = cityStore

	rigStore := state.stores["myrig"]
	source, err := rigStore.Create(beads.Bead{Title: "write hello.txt containing hi", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, state, cityStore, rigStore, source
}

const parityGraphFormula = `
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "do-work"
title = "Do work"
`

func postSlingJSON(t *testing.T, srv http.Handler, state *fakeMutatorState, body string) (*httptest.ResponseRecorder, slingResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	var resp slingResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode sling response: %v; body = %s", err, rec.Body.String())
		}
	}
	return rec, resp
}

func workflowBeads(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []beads.Bead
	for _, b := range all {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
			out = append(out, b)
		}
	}
	return out
}

// TestSlingDefaultFormulaCooksWorkflowWhereTheTargetsWorkersLook is the
// regression for the fresh-VM finding: POST /sling {target, bead} to a rig
// pool must cook the target's default formula in the source bead's own store,
// exactly as `gc sling <target> <bead>` does on a non-relocated city, and must
// report the launched workflow instead of claiming a plain "direct" route.
func TestSlingDefaultFormulaCooksWorkflowWhereTheTargetsWorkersLook(t *testing.T) {
	srv, state, cityStore, rigStore, source := newDefaultFormulaParityFixture(t, parityGraphFormula)

	rec, resp := postSlingJSON(t, srv, state, `{"target":"myrig/claude","bead":"`+source.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if got := workflowBeads(t, cityStore); len(got) != 0 {
		t.Fatalf("city store holds %d workflow root(s) %v; the rig's workers query the rig store, so the default formula must be cooked there (as gc sling does)", len(got), got)
	}
	roots := workflowBeads(t, rigStore)
	if len(roots) != 1 {
		t.Fatalf("rig store workflow roots = %d, want 1 (the default formula graph-work cooked next to its source bead)", len(roots))
	}
	root := roots[0]
	// A graph.v2 default formula takes the source bead through an input
	// convoy, and that convoy lives in the same store as the bead.
	inputConvoyID := root.Metadata["gc.input_convoy_id"]
	if inputConvoyID == "" {
		t.Fatalf("workflow root %s has no gc.input_convoy_id; metadata = %v", root.ID, root.Metadata)
	}
	members, err := convoycore.Members(rigStore, inputConvoyID, true)
	if err != nil {
		t.Fatalf("Members(%s): %v", inputConvoyID, err)
	}
	if len(members) != 1 || members[0].ID != source.ID {
		t.Fatalf("input convoy %s members = %v, want only the source bead %s", inputConvoyID, members, source.ID)
	}

	updated, err := rigStore.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", source.ID, err)
	}
	if got := updated.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "myrig/claude" {
		t.Fatalf("source %s = %q, want myrig/claude", beadmeta.ExecutionRoutedToMetadataKey, got)
	}

	if resp.Mode != "attached" {
		t.Fatalf("mode = %q, want attached (the target's default formula was applied)", resp.Mode)
	}
	if resp.Formula != "graph-work" {
		t.Fatalf("formula = %q, want graph-work", resp.Formula)
	}
	if resp.WorkflowID != root.ID {
		t.Fatalf("workflow_id = %q, want %q", resp.WorkflowID, root.ID)
	}
	if resp.AttachedBeadID != source.ID {
		t.Fatalf("attached_bead_id = %q, want %q", resp.AttachedBeadID, source.ID)
	}
	if resp.Run == nil || resp.Run.RunID != root.ID {
		t.Fatalf("run = %#v, want a run reference to %s", resp.Run, root.ID)
	}
}

// TestSlingDefaultFormulaReceivesTitleAndVars pins that title and vars reach
// the target's default formula, as `gc sling <target> <bead> --var k=v` does.
func TestSlingDefaultFormulaReceivesTitleAndVars(t *testing.T) {
	srv, state, _, rigStore, source := newDefaultFormulaParityFixture(t, `
formula = "graph-work"
version = 2
contract = "graph.v2"

[vars.greeting]
required = true

[[steps]]
id = "do-work"
title = "Say {{greeting}}"
`)

	rec, resp := postSlingJSON(t, srv, state, `{"target":"myrig/claude","bead":"`+source.ID+`","vars":{"greeting":"hi"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if resp.WorkflowID == "" {
		t.Fatalf("workflow_id is empty; body = %s", rec.Body.String())
	}
	all, err := rigStore.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range all {
		if b.Title == "Say hi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no step titled %q in the rig store: the greeting var never reached the default formula", "Say hi")
	}
}

// TestSlingNoFormulaRoutesRawBeadLikeCLI pins no_formula to `gc sling
// --no-formula`: a plain gc.routed_to route with no workflow.
func TestSlingNoFormulaRoutesRawBeadLikeCLI(t *testing.T) {
	srv, state, cityStore, rigStore, source := newDefaultFormulaParityFixture(t, parityGraphFormula)

	rec, resp := postSlingJSON(t, srv, state, `{"target":"myrig/claude","bead":"`+source.ID+`","no_formula":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if resp.Mode != "direct" || resp.WorkflowID != "" || resp.Formula != "" {
		t.Fatalf("response = %+v, want a direct route with no formula or workflow", resp)
	}
	if n := len(workflowBeads(t, rigStore)) + len(workflowBeads(t, cityStore)); n != 0 {
		t.Fatalf("%d workflow root(s) cooked, want none with no_formula", n)
	}
	updated, err := rigStore.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata[beadmeta.RoutedToMetadataKey]; got != "myrig/claude" {
		t.Fatalf("gc.routed_to = %q, want myrig/claude", got)
	}
}

// TestSlingConvoyRoutesEachChildLikeCLI pins convoy expansion: `gc sling
// <target> <convoy>` routes every open tracked child, never the container.
func TestSlingConvoyRoutesEachChildLikeCLI(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var children []beads.Bead
	for _, title := range []string{"first", "second"} {
		child, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}

	rec, resp := postSlingJSON(t, h, state, `{"target":"myrig/worker","bead":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, child := range children {
		got, err := store.Get(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata[beadmeta.RoutedToMetadataKey] != "myrig/worker" {
			t.Fatalf("child %s gc.routed_to = %q, want myrig/worker", child.ID, got.Metadata[beadmeta.RoutedToMetadataKey])
		}
	}
	gotConvoy, err := store.Get(convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := gotConvoy.Metadata[beadmeta.RoutedToMetadataKey]; v != "" {
		t.Fatalf("convoy container gc.routed_to = %q, want it left unrouted", v)
	}
	if resp.Batch == nil || resp.Batch.Routed != 2 || resp.Batch.Total != 2 || resp.Batch.ContainerType != "convoy" {
		t.Fatalf("batch = %+v, want convoy with 2 of 2 routed", resp.Batch)
	}
}

// TestSlingOnFormulaAttachesEachConvoyChildLikeCLI pins `--on` over the API:
// POST /sling {formula, attached_bead_id: <convoy>} attaches a v1 formula to
// every open tracked child, as `gc sling <target> <convoy> --on <formula>`
// does, and leaves the container bare. The batch it reports is what tells a
// remote CLI that the server expanded the convoy; an older server attached the
// wisp to the container and reported no batch.
func TestSlingOnFormulaAttachesEachConvoyChildLikeCLI(t *testing.T) {
	h, state := newSlingTestServer(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	v1Formula := "formula = \"code-review\"\nversion = 1\n\n[[steps]]\nid = \"review\"\ntitle = \"Review\"\n"
	if err := os.WriteFile(filepath.Join(formulaDir, "code-review.toml"), []byte(v1Formula), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var children []beads.Bead
	for _, title := range []string{"first", "second"} {
		child, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}

	rec, resp := postSlingJSON(t, h, state, `{"target":"myrig/worker","formula":"code-review","attached_bead_id":"`+convoy.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	molecules := map[string]bool{}
	for _, child := range children {
		got, err := store.Get(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		molecule := got.Metadata[beadmeta.MoleculeIDMetadataKey]
		if molecule == "" {
			t.Fatalf("child %s has no molecule_id, want the formula attached to it", child.ID)
		}
		molecules[molecule] = true
		if got.Metadata[beadmeta.RoutedToMetadataKey] != "myrig/worker" {
			t.Fatalf("child %s gc.routed_to = %q, want myrig/worker", child.ID, got.Metadata[beadmeta.RoutedToMetadataKey])
		}
	}
	if len(molecules) != len(children) {
		t.Fatalf("children share molecules %v, want one wisp per child", molecules)
	}
	gotConvoy, err := store.Get(convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := gotConvoy.Metadata[beadmeta.MoleculeIDMetadataKey]; v != "" {
		t.Fatalf("convoy container molecule_id = %q, want no wisp on the container", v)
	}
	if v := gotConvoy.Metadata[beadmeta.RoutedToMetadataKey]; v != "" {
		t.Fatalf("convoy container gc.routed_to = %q, want it left unrouted", v)
	}
	if resp.Mode != "attached" || resp.Formula != "code-review" || resp.AttachedBeadID != convoy.ID {
		t.Fatalf("response = %+v, want code-review attached on %s", resp, convoy.ID)
	}
	if resp.Batch == nil || resp.Batch.Routed != 2 || resp.Batch.Total != 2 || resp.Batch.ContainerType != "convoy" {
		t.Fatalf("batch = %+v, want convoy with 2 of 2 routed", resp.Batch)
	}
}
