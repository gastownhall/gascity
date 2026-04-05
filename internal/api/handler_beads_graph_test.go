package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// beadGraphResponse mirrors the handler's response struct for test decoding.
type beadGraphResponse struct {
	Root  beads.Bead   `json:"root"`
	Beads []beads.Bead `json:"beads"`
	Deps  []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
	} `json:"deps"`
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

func getGraph(t *testing.T, srv http.Handler, rootID string) (*httptest.ResponseRecorder, beadGraphResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v0/beads/graph/"+rootID, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var resp beadGraphResponse
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode graph response: %v", err)
		}
	}
	return rec, resp
}

func TestBeadGraphReturnsRootAndChildren(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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

func TestBeadGraphReturnsDeps(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

	rec, _ := getGraph(t, srv, "nonexistent-id")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestBeadGraphEmptyRootID(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Request with empty rootID path segment
	req := httptest.NewRequest("GET", "/v0/beads/graph/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Should get 400 or 404, not 200
	if rec.Code == http.StatusOK {
		t.Errorf("status = %d, want non-200 for empty rootID", rec.Code)
	}
}

func TestBeadGraphNoChildren(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	srv := New(state)

	root := createBeadWithMeta(t, store, "Lonely Root", map[string]string{
		"gc.kind": "workflow",
	})

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

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

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

	// Root in store1
	root := createBeadWithMeta(t, store1, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	// Child also in store1
	createBeadWithMeta(t, store1, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})

	rec, resp := getGraph(t, srv, root.ID)

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
	srv := New(state)

	root := createBeadWithMeta(t, store, "Root", map[string]string{
		"gc.kind": "workflow",
	})
	child := createBeadWithMeta(t, store, "Child", map[string]string{
		"gc.root_bead_id": root.ID,
	})

	// Add same dep twice (MemStore deduplicates, but collectWorkflowDeps also deduplicates)
	store.DepAdd(child.ID, root.ID, "blocks") //nolint:errcheck
	store.DepAdd(child.ID, root.ID, "blocks") //nolint:errcheck

	rec, resp := getGraph(t, srv, root.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(resp.Deps) != 1 {
		t.Errorf("len(deps) = %d, want 1 (deduplicated)", len(resp.Deps))
	}
}
