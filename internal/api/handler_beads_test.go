package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestBeadCRUD(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Create a bead.
	body := `{"rig":"myrig","title":"Fix login bug","type":"task"}`
	req := newPostRequest("/v0/beads", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var created beads.Bead
	json.NewDecoder(rec.Body).Decode(&created) //nolint:errcheck
	if created.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", created.Title, "Fix login bug")
	}
	if created.ID == "" {
		t.Fatal("created bead has no ID")
	}

	// Get the bead.
	req = httptest.NewRequest("GET", "/v0/bead/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got beads.Bead
	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix login bug")
	}

	// Close the bead.
	req = newPostRequest("/v0/bead/"+created.ID+"/close", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify closed.
	req = httptest.NewRequest("GET", "/v0/bead/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Status != "closed" {
		t.Errorf("Status = %q, want %q", got.Status, "closed")
	}
}

func TestBeadListFiltering(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	store.Create(beads.Bead{Title: "Open task", Type: "task"})                           //nolint:errcheck
	store.Create(beads.Bead{Title: "Message", Type: "message"})                          //nolint:errcheck
	store.Create(beads.Bead{Title: "Labeled", Type: "task", Labels: []string{"urgent"}}) //nolint:errcheck
	srv := New(state)

	// Filter by type.
	req := httptest.NewRequest("GET", "/v0/beads?type=message", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("type filter: Total = %d, want 1", resp.Total)
	}

	// Filter by label.
	req = httptest.NewRequest("GET", "/v0/beads?label=urgent", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("label filter: Total = %d, want 1", resp.Total)
	}
}

func TestBeadListCrossRig(t *testing.T) {
	state := newFakeState(t)
	store2 := beads.NewMemStore()
	state.stores["rig2"] = store2

	state.stores["myrig"].Create(beads.Bead{Title: "Bead from rig1"}) //nolint:errcheck
	store2.Create(beads.Bead{Title: "Bead from rig2"})                //nolint:errcheck
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/beads", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("cross-rig: Total = %d, want 2", resp.Total)
	}
}

func TestBeadGetNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/bead/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBeadReady(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	store.Create(beads.Bead{Title: "Open"}) //nolint:errcheck
	b2, _ := store.Create(beads.Bead{Title: "Closed"})
	store.Close(b2.ID) //nolint:errcheck
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/beads/ready", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("ready: Total = %d, want 1", resp.Total)
	}
}

func TestBeadUpdate(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	desc := "updated description"
	body := `{"description":"` + desc + `","labels":["new-label"]}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify update.
	got, _ := store.Get(b.ID)
	if got.Description != desc {
		t.Errorf("Description = %q, want %q", got.Description, desc)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "new-label" {
		t.Errorf("Labels = %v, want [new-label]", got.Labels)
	}
}

func TestBeadPatchAlias(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	desc := "patched"
	body := `{"description":"` + desc + `"}`
	req := httptest.NewRequest("PATCH", "/v0/bead/"+b.ID, bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Description != desc {
		t.Errorf("Description = %q, want %q", got.Description, desc)
	}
}

func TestBeadUpdatePriority(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	body := `{"priority":1}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want 1", got.Priority)
	}
}

func TestBeadUpdateRejectsNullPriority(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	priority := 1
	b, _ := store.Create(beads.Bead{Title: "Test", Priority: &priority})
	srv := New(state)

	body := `{"priority":null}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want unchanged 1", got.Priority)
	}
}

func TestBeadReopen(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Closed task"})
	store.Close(b.ID) //nolint:errcheck
	srv := New(state)

	// Reopen the closed bead.
	req := newPostRequest("/v0/bead/"+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Verify reopened.
	got, _ := store.Get(b.ID)
	if got.Status != "open" {
		t.Errorf("Status = %q, want %q", got.Status, "open")
	}
}

func TestBeadReopenNotClosed(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Open task"})
	srv := New(state)

	req := newPostRequest("/v0/bead/"+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestBeadAssign(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Task"})
	srv := New(state)

	body := `{"assignee":"worker-1"}`
	req := newPostRequest("/v0/bead/"+b.ID+"/assign", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Assignee != "worker-1" {
		t.Errorf("Assignee = %q, want %q", got.Assignee, "worker-1")
	}
}

func TestBeadDelete(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "To delete"})
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/bead/"+b.ID, nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Verify closed (soft delete).
	got, _ := store.Get(b.ID)
	if got.Status != "closed" {
		t.Errorf("Status = %q, want %q", got.Status, "closed")
	}
}

func TestBeadDeleteNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/bead/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBeadCreateValidation(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Missing title.
	req := newPostRequest("/v0/beads", bytes.NewBufferString(`{"rig":"myrig"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestPackList(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Packs = map[string]config.PackSource{
		"gastown": {
			Source: "https://github.com/example/gastown-pack",
			Ref:    "v1.0.0",
			Path:   "packs/gastown",
		},
	}
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/packs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Packs []packResponse `json:"packs"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Packs) != 1 {
		t.Fatalf("packs count = %d, want 1", len(resp.Packs))
	}
	if resp.Packs[0].Name != "gastown" {
		t.Errorf("Name = %q, want %q", resp.Packs[0].Name, "gastown")
	}
	if resp.Packs[0].Source != "https://github.com/example/gastown-pack" {
		t.Errorf("Source = %q", resp.Packs[0].Source)
	}
}

func TestPackListEmpty(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/packs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Packs []packResponse `json:"packs"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Packs) != 0 {
		t.Errorf("packs count = %d, want 0", len(resp.Packs))
	}
}
