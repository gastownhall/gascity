package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/importsvc"
)

// These tests pin the Idempotency-Key wire contract on the S2 create
// endpoints: a repeat POST with the same key + same body replays the first
// response without re-running the create. The helper-level semantics
// (mismatch 422, in-flight 409, unreserve-on-error) are covered by
// idempotency_helper_test.go; here each test proves the endpoint is actually
// wired through withIdempotency.

func postIdempotent(t *testing.T, h http.Handler, url, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newPostRequest(url, strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentCreateIdempotentReplay(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"coder","provider":"claude"}`
	first := postIdempotent(t, h, cityURL(fs, "/agents"), "agent-create-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}

	replay := postIdempotent(t, h, cityURL(fs, "/agents"), "agent-create-1", body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), first.Body.String())
	}
	// The create must have run exactly once.
	count := 0
	for _, a := range fs.cfg.Agents {
		if a.Name == "coder" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("agent 'coder' appears %d times in config, want 1 (replay re-ran create)", count)
	}
}

func TestAgentCreateIdempotencyMismatch(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	first := postIdempotent(t, h, cityURL(fs, "/agents"), "agent-create-1", `{"name":"coder","provider":"claude"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}
	// Same key, different body → 422, and no second agent is created.
	mismatch := postIdempotent(t, h, cityURL(fs, "/agents"), "agent-create-1", `{"name":"other","provider":"claude"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}
	for _, a := range fs.cfg.Agents {
		if a.Name == "other" {
			t.Fatal("mismatched request created agent 'other'; it must not run the create")
		}
	}
}

func TestProviderCreateIdempotentReplay(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	// fakeMutatorState.CreateProvider rejects duplicates with ErrAlreadyExists,
	// so a replay that re-ran the create would surface as a 409 here.
	body := `{"name":"ollama","command":"ollama run"}`
	first := postIdempotent(t, h, cityURL(fs, "/providers"), "prov-create-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}

	replay := postIdempotent(t, h, cityURL(fs, "/providers"), "prov-create-1", body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201 (409 means the create re-ran); body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want %s", replay.Body.String(), first.Body.String())
	}
}

func TestRigCreateIdempotentReplay(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"backend","path":"` + t.TempDir() + `"}`
	first := postIdempotent(t, h, cityURL(fs, "/rigs"), "rig-create-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}

	replay := postIdempotent(t, h, cityURL(fs, "/rigs"), "rig-create-1", body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want %s", replay.Body.String(), first.Body.String())
	}
	count := 0
	for _, r := range fs.cfg.Rigs {
		if r.Name == "backend" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("rig 'backend' appears %d times in config, want 1 (replay re-ran create)", count)
	}
}

func TestConvoyCreateIdempotentReplay(t *testing.T) {
	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)

	store := state.stores["myrig"]
	item, err := store.Create(beads.Bead{Title: "task-1"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	body := `{"rig":"myrig","title":"test convoy","items":["` + item.ID + `"]}`
	first := postIdempotent(t, h, cityURL(state, "/convoys"), "convoy-create-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}
	var firstConvoy beads.Bead
	if err := json.NewDecoder(first.Body).Decode(&firstConvoy); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	replay := postIdempotent(t, h, cityURL(state, "/convoys"), "convoy-create-1", body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201; body = %s", replay.Code, replay.Body.String())
	}
	var replayConvoy beads.Bead
	if err := json.NewDecoder(replay.Body).Decode(&replayConvoy); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if replayConvoy.ID != firstConvoy.ID {
		t.Fatalf("replay returned convoy %s, want the first convoy %s", replayConvoy.ID, firstConvoy.ID)
	}
	// Exactly one convoy bead must exist.
	convoys, err := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	if len(convoys) != 1 {
		t.Fatalf("store holds %d convoy beads, want 1 (replay re-ran create)", len(convoys))
	}
}

func TestPackAddIdempotentReplay(t *testing.T) {
	calls := 0
	orig := packAddImport
	packAddImport = func(_ fsys.FS, _, source, _, version string) (*importsvc.AddResult, error) {
		calls++
		return &importsvc.AddResult{Name: "review", Source: source, Version: version, GitBacked: true}, nil
	}
	defer func() { packAddImport = orig }()

	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"source":"https://github.com/org/repo/tree/main/packs/review"}`
	first := postIdempotent(t, h, cityURL(fs, "/packs"), "pack-add-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first add: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}

	replay := postIdempotent(t, h, cityURL(fs, "/packs"), "pack-add-1", body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want %s", replay.Body.String(), first.Body.String())
	}
	if calls != 1 {
		t.Fatalf("packAddImport ran %d times, want 1 (replay re-ran the import)", calls)
	}
}
