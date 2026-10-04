package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// beadGraphResponse mirrors the handler's response struct for test decoding.
// Membership is decoded as a plain string rather than beads.Membership so a
// test can catch a spelling the vocabulary does not define.
type beadGraphResponse struct {
	Root  beads.Bead   `json:"root"`
	Beads []beads.Bead `json:"beads"`
	Deps  []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
	} `json:"deps"`
	Membership string `json:"membership"`
}

type graphCountingListStore struct {
	beads.Store
	parentListCalls int
}

func (s *graphCountingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID != "" {
		s.parentListCalls++
	}
	return s.Store.List(query)
}

func createBeadWithMeta(t *testing.T, store beads.Store, title string, meta map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("create bead %q: %v", title, err)
	}
	for k, v := range meta {
		if err := store.SetMetadata(b.ID, k, v); err != nil {
			t.Fatalf("set metadata %q on %q: %v", k, b.ID, err)
		}
	}
	return b
}

func getGraph(t *testing.T, h http.Handler, fs *fakeState, rootID string) (*httptest.ResponseRecorder, beadGraphResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", cityURL(fs, "/beads/graph/"+rootID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp beadGraphResponse
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode graph response: %v", err)
		}
	}
	return rec, resp
}

func TestBeadGraphGraphV2MetadataGraphSkipsParentWalk(t *testing.T) {
	state := newFakeState(t)
	base := state.stores["myrig"]

	root := createBeadWithMeta(t, base, "Workflow Root", map[string]string{
		beadmeta.KindMetadataKey:            "workflow",
		beadmeta.FormulaContractMetadataKey: "graph.v2",
	})
	child, err := base.Create(beads.Bead{
		Title:    "Step 1",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	grandchild, err := base.Create(beads.Bead{
		Title:    "Step 1.1",
		Type:     "task",
		ParentID: child.ID,
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}

	counting := &graphCountingListStore{Store: base}
	state.stores["myrig"] = counting
	h := newTestCityHandler(t, state)

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if counting.parentListCalls != 0 {
		t.Fatalf("ParentID List calls = %d, want 0 for graph.v2 metadata graph", counting.parentListCalls)
	}
	beadIDs := map[string]bool{}
	for _, b := range resp.Beads {
		beadIDs[b.ID] = true
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		if !beadIDs[id] {
			t.Fatalf("graph beads missing %s; got %#v", id, resp.Beads)
		}
	}
	edges := map[string]bool{}
	for _, dep := range resp.Deps {
		edges[dep.From+"|"+dep.To+"|"+dep.Kind] = true
	}
	for _, edge := range []string{
		root.ID + "|" + child.ID + "|parent-child",
		child.ID + "|" + grandchild.ID + "|parent-child",
	} {
		if !edges[edge] {
			t.Fatalf("graph deps missing %s; got %#v", edge, resp.Deps)
		}
	}
}

func TestBeadGraphReturnsRootAndChildren(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Workflow Root", map[string]string{
		"gc.kind": "workflow",
	})
	child1 := createBeadWithMeta(t, store, "Step 1", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.kind":         "task",
	})
	child2 := createBeadWithMeta(t, store, "Step 2", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.kind":         "task",
	})
	child3 := createBeadWithMeta(t, store, "Step 3", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.kind":         "scope",
	})

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if resp.Root.ID != root.ID {
		t.Errorf("root.ID = %q, want %q", resp.Root.ID, root.ID)
	}
	if len(resp.Beads) != 4 {
		t.Errorf("len(beads) = %d, want 4", len(resp.Beads))
	}

	beadIDs := map[string]bool{}
	for _, b := range resp.Beads {
		beadIDs[b.ID] = true
	}
	for _, id := range []string{root.ID, child1.ID, child2.ID, child3.ID} {
		if !beadIDs[id] {
			t.Errorf("beads missing ID %q", id)
		}
	}
}

func TestBeadGraphIncludesParentChildChildrenAndEdges(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root, err := store.Create(beads.Bead{Title: "Root", Type: "feature"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "Child", Type: "task", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	sibling, err := store.Create(beads.Bead{Title: "Sibling", Type: "bug", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create(sibling): %v", err)
	}

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	beadIDs := map[string]bool{}
	for _, b := range resp.Beads {
		beadIDs[b.ID] = true
	}
	for _, id := range []string{root.ID, child.ID, sibling.ID} {
		if !beadIDs[id] {
			t.Fatalf("graph beads missing %s; got %#v", id, resp.Beads)
		}
	}

	edges := map[string]bool{}
	for _, dep := range resp.Deps {
		edges[dep.From+"|"+dep.To+"|"+dep.Kind] = true
	}
	for _, id := range []string{child.ID, sibling.ID} {
		key := root.ID + "|" + id + "|parent-child"
		if !edges[key] {
			t.Fatalf("graph deps missing %s; got %#v", key, resp.Deps)
		}
	}
}

func TestBeadGraphIncludesTracksConvoyMembersAndEdges(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "Child", Type: "task"})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	rec, resp := getGraph(t, h, state, convoy.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	beadIDs := map[string]bool{}
	for _, b := range resp.Beads {
		beadIDs[b.ID] = true
	}
	for _, id := range []string{convoy.ID, child.ID} {
		if !beadIDs[id] {
			t.Fatalf("graph beads missing %s; got %#v", id, resp.Beads)
		}
	}

	for _, dep := range resp.Deps {
		if dep.From == child.ID && dep.To == convoy.ID && dep.Kind == "tracks" {
			return
		}
	}
	t.Fatalf("missing tracks edge %s -> %s; deps=%#v", child.ID, convoy.ID, resp.Deps)
}

func TestBeadGraphReturnsErrorWhenGraphListFails(t *testing.T) {
	state := newFakeState(t)
	base := state.stores["myrig"]
	root, err := base.Create(beads.Bead{Title: "Root", Type: "feature"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	state.stores["myrig"] = &failingBeadStore{
		Store:   base,
		listErr: errors.New("list failed"),
	}
	h := newTestCityHandler(t, state)

	rec, _ := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestBeadGraphReturnsErrorWhenDepListFails(t *testing.T) {
	state := newFakeState(t)
	base := state.stores["myrig"]
	root, err := base.Create(beads.Bead{Title: "Root", Type: "feature"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	state.stores["myrig"] = depListFailStore{Store: base}
	h := newTestCityHandler(t, state)

	rec, _ := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestBeadGraphReturnsDeps(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child1 := createBeadWithMeta(t, store, "Step 1", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	child2 := createBeadWithMeta(t, store, "Step 2", map[string]string{
		"gc.root_bead_id": root.ID,
	})

	// child2 depends on child1 (child1 blocks child2)
	if err := store.DepAdd(child2.ID, child1.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resp.Deps) != 1 {
		t.Fatalf("len(deps) = %d, want 1", len(resp.Deps))
	}
	dep := resp.Deps[0]
	// collectWorkflowDeps convention: From=dependsOn, To=issueID
	if dep.From != child1.ID || dep.To != child2.ID {
		t.Errorf("dep = {from:%q, to:%q}, want {from:%q, to:%q}",
			dep.From, dep.To, child1.ID, child2.ID)
	}
	if dep.Kind != "blocks" {
		t.Errorf("dep.Kind = %q, want %q", dep.Kind, "blocks")
	}
}

func TestBeadGraphReturnsRawStatus(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child := createBeadWithMeta(t, store, "Done Step", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.outcome":      "pass",
	})
	// Close the child bead
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// The key assertion: status must be raw "closed", NOT mapped "completed"
	for _, b := range resp.Beads {
		if b.ID == child.ID {
			if b.Status != "closed" {
				t.Errorf("child status = %q, want raw %q (not workflow-mapped)", b.Status, "closed")
			}
			return
		}
	}
	t.Error("child bead not found in response")
}

func TestBeadGraphReturnsRawMetadata(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	createBeadWithMeta(t, store, "Step", map[string]string{
		"gc.root_bead_id":    root.ID,
		"gc.kind":            "task",
		"gc.step_ref":        "build.code",
		"gc.outcome":         "fail",
		"gc.scope_ref":       "rig:myrig",
		"gc.logical_bead_id": "logical-1",
	})

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Find the child and verify metadata is raw/unprocessed
	for _, b := range resp.Beads {
		if b.Metadata == nil {
			continue
		}
		if b.Metadata["gc.step_ref"] == "build.code" {
			// All metadata keys should be present as-is
			checks := map[string]string{
				"gc.kind":            "task",
				"gc.step_ref":        "build.code",
				"gc.outcome":         "fail",
				"gc.scope_ref":       "rig:myrig",
				"gc.logical_bead_id": "logical-1",
			}
			for k, want := range checks {
				got := b.Metadata[k]
				if got != want {
					t.Errorf("metadata[%q] = %q, want %q", k, got, want)
				}
			}
			return
		}
	}
	t.Error("child bead with step_ref not found in response")
}

func TestBeadGraphRootNotFound(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec, _ := getGraph(t, h, state, "nonexistent-id")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestBeadGraphEmptyRootID(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Request with empty rootID path segment
	req := httptest.NewRequest("GET", cityURL(state, "/beads/graph/"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should get 400 or 404, not 200
	if rec.Code == http.StatusOK {
		t.Errorf("status = %d, want non-200 for empty rootID", rec.Code)
	}
}

func TestBeadGraphNoChildren(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Lonely Root", map[string]string{
		"gc.kind": "workflow",
	})

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Root.ID != root.ID {
		t.Errorf("root.ID = %q, want %q", resp.Root.ID, root.ID)
	}
	// beads[] should contain just the root
	if len(resp.Beads) != 1 {
		t.Errorf("len(beads) = %d, want 1", len(resp.Beads))
	}
	if len(resp.Deps) != 0 {
		t.Errorf("len(deps) = %d, want 0", len(resp.Deps))
	}
}

func TestBeadGraphExcludesUnrelatedBeads(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child := createBeadWithMeta(t, store, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	// Unrelated bead — different root
	createBeadWithMeta(t, store, "Other Workflow Step", map[string]string{
		"gc.root_bead_id": "some-other-root",
	})
	// Unrelated bead — no root at all
	createBeadWithMeta(t, store, "Standalone", nil)

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resp.Beads) != 2 {
		t.Errorf("len(beads) = %d, want 2 (root + 1 child)", len(resp.Beads))
	}
	beadIDs := map[string]bool{}
	for _, b := range resp.Beads {
		beadIDs[b.ID] = true
	}
	if !beadIDs[root.ID] {
		t.Error("missing root in beads")
	}
	if !beadIDs[child.ID] {
		t.Error("missing child in beads")
	}
}

func TestBeadGraphDepsFilteredToGraphBeads(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child := createBeadWithMeta(t, store, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	outsider, _ := store.Create(beads.Bead{Title: "Outside"})

	// Dep within graph
	store.DepAdd(child.ID, root.ID, "blocks") //nolint:errcheck
	// Dep pointing outside graph — should be excluded
	store.DepAdd(child.ID, outsider.ID, "relates-to") //nolint:errcheck

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resp.Deps) != 1 {
		t.Errorf("len(deps) = %d, want 1 (only in-graph dep)", len(resp.Deps))
	}
	if len(resp.Deps) > 0 {
		dep := resp.Deps[0]
		if dep.From != root.ID || dep.To != child.ID {
			t.Errorf("dep = {from:%q, to:%q}, want {from:%q, to:%q}",
				dep.From, dep.To, root.ID, child.ID)
		}
	}
}

func TestBeadGraphMultipleStores(t *testing.T) {
	state := newFakeState(t)
	store1 := state.stores["myrig"]
	store2 := beads.NewMemStore()
	state.stores["otherrig"] = store2
	h := newTestCityHandler(t, state)

	// Root in store1
	root := createBeadWithMeta(t, store1, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	// Child also in store1
	createBeadWithMeta(t, store1, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if resp.Root.ID != root.ID {
		t.Errorf("root.ID = %q, want %q", resp.Root.ID, root.ID)
	}
	if len(resp.Beads) != 2 {
		t.Errorf("len(beads) = %d, want 2", len(resp.Beads))
	}
}

func TestBeadGraphDedupsDeps(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child := createBeadWithMeta(t, store, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})

	// Add same dep twice (MemStore deduplicates, but collectWorkflowDeps also deduplicates)
	store.DepAdd(child.ID, root.ID, "blocks") //nolint:errcheck
	store.DepAdd(child.ID, root.ID, "blocks") //nolint:errcheck

	rec, resp := getGraph(t, h, state, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resp.Deps) != 1 {
		t.Errorf("len(deps) = %d, want 1 (deduplicated)", len(resp.Deps))
	}
}

// TestBeadGraphDeclaresItsMembershipOnTheWire pins the contract the graph
// endpoint now states about itself. The response says which rule chose Beads,
// because a consumer cannot tell one rule's answer from another's by looking
// at the result: on the measured live molecule gcg-arn a root-id scan and a
// dependency walk returned 61 and 48 beads, and 48 happened to be exactly what
// the adopt-pr driver wanted. The membership field is what makes that
// difference legible without reading the handler.
func TestBeadGraphDeclaresItsMembershipOnTheWire(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Workflow Root", map[string]string{
		"gc.kind": "workflow",
	})
	// A spec sidecar: carries the root id, has no dependency edge of any kind.
	// A dep walk cannot reach it; this endpoint must return it.
	spec := createBeadWithMeta(t, store, "Step spec for work", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.kind":         "spec",
	})
	// An out-of-molecule bead the root merely blocks on. A dep walk reaches
	// it; this endpoint must not return it.
	outsider := createBeadWithMeta(t, store, "Blocker outside the molecule", nil)
	if err := store.DepAdd(root.ID, outsider.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(root -> outsider): %v", err)
	}

	rec, resp := getGraph(t, h, state, root.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Membership != string(beads.MembershipRootIDAndParentClosure) {
		t.Errorf("membership = %q, want %q — the wire must name the rule that produced beads, not leave a consumer to infer it",
			resp.Membership, beads.MembershipRootIDAndParentClosure)
	}

	got := map[string]bool{}
	for _, b := range resp.Beads {
		got[b.ID] = true
	}
	if !got[spec.ID] {
		t.Errorf("graph is missing the dependency-isolated spec sidecar %s; the endpoint would then be answering %q while declaring %q",
			spec.ID, beads.MembershipDepReachable, resp.Membership)
	}
	if got[outsider.ID] {
		t.Errorf("graph contains %s, which carries no gc.root_bead_id and is only reachable by following a blocks edge out of the molecule", outsider.ID)
	}
}

// TestBeadGraphDeclaresConvoyMembershipOnTheWire pins the widened value: a
// convoy root pulls in tracks-linked members, which is a different rule from
// the root-id/parent one and is spelled differently on the wire so a consumer
// is never told the narrower rule was applied.
func TestBeadGraphDeclaresConvoyMembershipOnTheWire(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}
	member := createBeadWithMeta(t, store, "Convoy member", nil)
	if err := store.DepAdd(convoy.ID, member.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	rec, resp := getGraph(t, h, state, convoy.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Membership != string(beads.MembershipRootIDParentClosureAndConvoy) {
		t.Errorf("membership = %q, want %q — a convoy root applies a rule the narrower spelling does not describe",
			resp.Membership, beads.MembershipRootIDParentClosureAndConvoy)
	}
}

// TestBeadGraphConvoyMembershipClosesOverTheMembers pins the half of the
// convoy spelling that was previously only implied. The parent-child walk is
// seeded from the whole member set, so a convoy member contributes its own
// transitive subtree — the documented rule says "+parent-closure", and this is
// what that closes over. Narrowing it to the root's own closure would hide
// every convoy member's subtree from the graph view, which is a worse answer
// for the display consumer this endpoint has.
func TestBeadGraphConvoyMembershipClosesOverTheMembers(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}
	member := createBeadWithMeta(t, store, "Convoy member", nil)
	if err := store.DepAdd(convoy.ID, member.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "Child of a convoy member", Type: "task", ParentID: member.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	grandchild, err := store.Create(beads.Bead{Title: "Grandchild of a convoy member", Type: "task", ParentID: child.ID})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}

	rec, resp := getGraph(t, h, state, convoy.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	got := map[string]bool{}
	for _, b := range resp.Beads {
		got[b.ID] = true
	}
	for _, want := range []beads.Bead{member, child, grandchild} {
		if !got[want.ID] {
			t.Errorf("graph is missing %s (%q); %q takes the parent closure over the convoy members as well as the root, and the doc now says so",
				want.ID, want.Title, resp.Membership)
		}
	}
}

// createEphemeralBeadWithMeta creates a bead in the ephemeral (wisps) tier.
// This is the storage a wisp molecule's beads actually land in: the wisp bead
// policy resolves to ephemeral storage under bd-1.0.5 ready semantics
// (cmd/gc/bead_policy_store.go defaultBeadStorage), so every node of a wisp
// molecule is an ephemeral row carrying gc.root_bead_id.
func createEphemeralBeadWithMeta(t *testing.T, store beads.Store, title string, meta map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task", Ephemeral: true})
	if err != nil {
		t.Fatalf("create ephemeral bead %q: %v", title, err)
	}
	if !b.Ephemeral {
		t.Fatalf("create ephemeral bead %q: store did not honor the ephemeral tier, so this test cannot exercise it", title)
	}
	for k, v := range meta {
		if err := store.SetMetadata(b.ID, k, v); err != nil {
			t.Fatalf("set metadata %q on %q: %v", k, b.ID, err)
		}
	}
	return b
}

// TestBeadGraphDeclaredMembershipCoversTheEphemeralTier holds the declaration
// true across storage tiers. "direct-root-id+parent-closure" has no tier axis,
// and the rule it names — beads.DirectMembers — reads both tiers, so a
// tier-scoped read here would answer a strictly narrower question than the
// wire says it answered. A ListQuery's zero-value TierMode is TierIssues,
// which drops every Ephemeral row, so this is a default worth pinning: the
// endpoint returned 200 and a plausible count while silently omitting a whole
// tier.
func TestBeadGraphDeclaredMembershipCoversTheEphemeralTier(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createBeadWithMeta(t, store, "Workflow Root", map[string]string{
		"gc.kind": "workflow",
	})
	ephemeralMember := createEphemeralBeadWithMeta(t, store, "Ephemeral step", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	durableMember := createBeadWithMeta(t, store, "Durable step", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	ephemeralChild, err := store.Create(beads.Bead{
		Title:     "Ephemeral parent-linked descendant",
		Type:      "task",
		ParentID:  durableMember.ID,
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(ephemeral child): %v", err)
	}

	rec, resp := getGraph(t, h, state, root.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Membership != string(beads.MembershipRootIDAndParentClosure) {
		t.Fatalf("membership = %q, want %q", resp.Membership, beads.MembershipRootIDAndParentClosure)
	}

	got := map[string]bool{}
	for _, b := range resp.Beads {
		got[b.ID] = true
	}
	if !got[durableMember.ID] {
		t.Errorf("graph is missing durable member %s", durableMember.ID)
	}
	if !got[ephemeralMember.ID] {
		t.Errorf("graph is missing ephemeral member %s, which carries gc.root_bead_id == %s; the endpoint declared %q, and that rule has no tier axis",
			ephemeralMember.ID, root.ID, resp.Membership)
	}
	if !got[ephemeralChild.ID] {
		t.Errorf("graph is missing ephemeral parent-linked descendant %s; the parent-closure half of %q is tier-scoped too",
			ephemeralChild.ID, resp.Membership)
	}

	// The declared rule's root-id half is beads.DirectMembers. Comparing
	// against it directly is what makes this a declaration test rather than a
	// count test: DirectMembers reads TierBoth through the LIVE handle, so any
	// tier the endpoint drops shows up here as a set difference.
	direct, err := beads.DirectMembers(store, root.ID)
	if err != nil {
		t.Fatalf("DirectMembers: %v", err)
	}
	for _, member := range direct {
		if !got[member.ID] {
			t.Errorf("graph omits %q, which beads.DirectMembers returns; the endpoint cannot declare %q and answer a subset of it",
				member.ID, resp.Membership)
		}
	}
}

// TestBeadGraphDeclaredMembershipCoversAnEphemeralRoot is the shape that reads
// as a finished molecule. store.Get is tier-agnostic, so an ephemeral root
// always resolves and the status is always 200; only the member list goes
// empty. A wisp molecule — whose every node is ephemeral — would therefore
// report itself complete rather than erroring, while the wire declared a rule
// that includes those members.
func TestBeadGraphDeclaredMembershipCoversAnEphemeralRoot(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	root := createEphemeralBeadWithMeta(t, store, "Wisp root", map[string]string{
		"gc.kind": "workflow",
	})
	member := createEphemeralBeadWithMeta(t, store, "Wisp step", map[string]string{
		"gc.root_bead_id": root.ID,
	})
	spec := createEphemeralBeadWithMeta(t, store, "Wisp step spec", map[string]string{
		"gc.root_bead_id": root.ID,
		"gc.kind":         "spec",
	})

	rec, resp := getGraph(t, h, state, root.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Root.ID != root.ID {
		t.Fatalf("root.ID = %q, want %q", resp.Root.ID, root.ID)
	}

	got := map[string]bool{}
	for _, b := range resp.Beads {
		got[b.ID] = true
	}
	for _, want := range []beads.Bead{member, spec} {
		if !got[want.ID] {
			t.Errorf("wisp molecule graph is missing member %s (%q); every node of a wisp molecule is ephemeral, so a tier-scoped read returns the root alone — 200 with an empty molecule, which is indistinguishable from a finished one",
				want.ID, want.Title)
		}
	}

	direct, err := beads.DirectMembers(store, root.ID)
	if err != nil {
		t.Fatalf("DirectMembers: %v", err)
	}
	if len(resp.Beads) != len(direct) {
		t.Errorf("graph returned %d beads, want %d (beads.DirectMembers) — the endpoint declared %q and must not answer a subset of the rule it names",
			len(resp.Beads), len(direct), resp.Membership)
	}
}

// TestBeadGraphDeclaredMembershipCoversTheEphemeralTierOnSQLite runs the same
// contract against a real SQLiteStore, because the two backends drop the
// ephemeral tier by different mechanisms and the MemStore case only proves
// one. MemStore filters in Go through ListQuery.matchesTier; SQLiteStore
// filters in SQL, appending `b.tier='main'` to the WHERE clause for any mode
// but TierBoth. A fix that satisfied the Go filter and not the SQL one would
// still leave the wire declaration false on every real city.
func TestBeadGraphDeclaredMembershipCoversTheEphemeralTierOnSQLite(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	ephemeral, ok := store.(beads.StorageCreateStore)
	if !ok {
		t.Fatal("SQLiteStore no longer offers CreateWithStorage; this test can no longer place a bead in the ephemeral tier")
	}

	root, err := ephemeral.CreateWithStorage(beads.Bead{Title: "Wisp root", Type: "task"}, beads.StorageEphemeral)
	if err != nil {
		t.Fatalf("CreateWithStorage(root): %v", err)
	}
	member, err := ephemeral.CreateWithStorage(beads.Bead{Title: "Wisp member", Type: "task"}, beads.StorageEphemeral)
	if err != nil {
		t.Fatalf("CreateWithStorage(member): %v", err)
	}
	if err := store.SetMetadata(member.ID, "gc.root_bead_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(member): %v", err)
	}
	durable, err := store.Create(beads.Bead{Title: "Durable member", Type: "task"})
	if err != nil {
		t.Fatalf("Create(durable): %v", err)
	}
	if err := store.SetMetadata(durable.ID, "gc.root_bead_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(durable): %v", err)
	}

	rootBead, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	graph, _, membership, err := collectBeadGraph(store, rootBead)
	if err != nil {
		t.Fatalf("collectBeadGraph: %v", err)
	}
	if membership != beads.MembershipRootIDAndParentClosure {
		t.Fatalf("membership = %q, want %q", membership, beads.MembershipRootIDAndParentClosure)
	}

	got := map[string]bool{}
	for _, b := range graph {
		got[b.ID] = true
	}
	if !got[member.ID] {
		t.Errorf("graph is missing ephemeral member %s on SQLite; the tier predicate is a SQL WHERE clause there, so the Go-side filter passing proves nothing about it", member.ID)
	}

	direct, err := beads.DirectMembers(store, root.ID)
	if err != nil {
		t.Fatalf("DirectMembers: %v", err)
	}
	if len(graph) != len(direct) {
		t.Errorf("graph returned %d beads, want %d (beads.DirectMembers) — the endpoint declared %q and must not answer a subset of the rule it names",
			len(graph), len(direct), membership)
	}
}
