package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// bdDogState builds a city with a single city-scoped agent "bd.dog" (session
// name "bd__dog") whose named provider session is NOT running, a distinct city
// store, and no rig work stores. The caller opts into the relocated-graph leg
// by wiring st.graphBeadStore. This mirrors a maintainer-city agent whose work
// runs under agent-agnostic graph.v2 wisp sessions.
func bdDogState(t *testing.T) *fakeState {
	t.Helper()
	st := newFakeState(t)
	// A singleton agent (MaxActiveSessions=1, no pool overrides) so it expands
	// to exactly one identity "bd.dog" with session name "bd__dog".
	st.cfg.Agents = []config.Agent{{Name: "dog", BindingName: "bd", MaxActiveSessions: intPtr(1)}}
	st.cfg.NamedSessions = nil
	st.cfg.Rigs = nil
	st.stores = map[string]beads.Store{}
	st.cityBeadStore = beads.NewMemStore()
	return st
}

// seedGraphWisp creates an in-progress bead in store whose assignee is the wisp
// form "<sessionName>-gcg-session-<uuid>" and returns its id. This is exactly
// the shape a relocated graph store carries for live agent work (verified
// against the maintainer-city graph sqlite).
func seedGraphWisp(t *testing.T, store beads.Store, sessionName, uuid string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Type: "task", Title: "wisp step"})
	if err != nil {
		t.Fatalf("Create wisp bead: %v", err)
	}
	status := "in_progress"
	assignee := sessionName + "-gcg-session-" + uuid
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update wisp bead: %v", err)
	}
	return b.ID
}

// getAgentByName drives GET /v0/city/<city>/agents through the full handler and
// returns the response row for the given qualified agent name.
func getAgentByName(t *testing.T, st *fakeState, name string) agentResponse {
	t.Helper()
	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)
	req := httptest.NewRequest("GET", cityURL(st, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Items []agentResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode /agents: %v", err)
	}
	for _, a := range resp.Items {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q not found in /agents response (%d items)", name, len(resp.Items))
	return agentResponse{}
}

// TestGraphResidentWorkCountsAsRunning pins the core fix: on a RELOCATED-graph
// city, an agent whose only signal of life is an in-progress wisp bead in the
// dedicated graph store (its named provider session is down) is counted in
// agents.running by the status handler and reads "working" — not "stopped" —
// via /v0/agents, with its active_bead pointing at the graph bead.
func TestGraphResidentWorkCountsAsRunning(t *testing.T) {
	st := bdDogState(t)
	graph := beads.NewMemStore()
	st.graphBeadStore = graph
	beadID := seedGraphWisp(t, graph, "bd__dog", "917c4dd26bd772e2463919029a889e0e")

	// (b) Status handler counts the agent as running.
	body := New(st).buildStatusBody(context.Background(), false)
	if body.Agents.Total != 1 {
		t.Fatalf("agents.total = %d, want 1", body.Agents.Total)
	}
	if body.Agents.Running != 1 {
		t.Errorf("agents.running = %d, want 1 (graph-resident work must count)", body.Agents.Running)
	}
	if body.Running != 1 {
		t.Errorf("top-level running = %d, want 1", body.Running)
	}

	// (a) /v0/agents reports the agent working with the graph bead active.
	got := getAgentByName(t, st, "bd.dog")
	if got.State != "working" {
		t.Errorf("agent state = %q, want %q", got.State, "working")
	}
	if !got.Running {
		t.Errorf("agent running = false, want true")
	}
	if got.ActiveBead != beadID {
		t.Errorf("agent active_bead = %q, want %q", got.ActiveBead, beadID)
	}
}

// TestGraphResidentSingleStoreByteIdentical pins the single-store invariant:
// with the graph class NOT relocated (GraphBeadStore() == CityBeadStore()), the
// graph leg is inert — the very same wisp bead placed in the shared store is
// ignored, so the agent stays stopped and uncounted, byte-identical to the
// pre-seam behavior. This is the regression guard for the relocation gate.
func TestGraphResidentSingleStoreByteIdentical(t *testing.T) {
	st := bdDogState(t)
	// graphBeadStore left nil => GraphBeadStore() falls back to the city store.
	if st.GraphBeadStore().Store != st.CityBeadStore() {
		t.Fatalf("precondition: expected single-store (graph == city)")
	}
	// Same wisp bead as the relocated test, but in the shared city store.
	seedGraphWisp(t, st.cityBeadStore, "bd__dog", "917c4dd26bd772e2463919029a889e0e")

	body := New(st).buildStatusBody(context.Background(), false)
	if body.Agents.Running != 0 {
		t.Errorf("agents.running = %d, want 0 (single-store must not prefix-match)", body.Agents.Running)
	}
	if body.Running != 0 {
		t.Errorf("top-level running = %d, want 0", body.Running)
	}

	got := getAgentByName(t, st, "bd.dog")
	if got.State != "stopped" {
		t.Errorf("agent state = %q, want %q (single-store byte-identical)", got.State, "stopped")
	}
	if got.Running {
		t.Errorf("agent running = true, want false")
	}
	if got.ActiveBead != "" {
		t.Errorf("agent active_bead = %q, want empty", got.ActiveBead)
	}
}

// TestGraphResidentNegativeNoMatchingWorkStaysStopped proves the prefix match is
// agent-specific: a relocated graph store holding an in-progress wisp for a
// DIFFERENT agent leaves bd.dog stopped and uncounted.
func TestGraphResidentNegativeNoMatchingWorkStaysStopped(t *testing.T) {
	st := bdDogState(t)
	graph := beads.NewMemStore()
	st.graphBeadStore = graph
	// Work for a different agent session ("other__cat"), not bd__dog.
	seedGraphWisp(t, graph, "other__cat", "deadbeefdeadbeefdeadbeefdeadbeef")

	body := New(st).buildStatusBody(context.Background(), false)
	if body.Agents.Running != 0 {
		t.Errorf("agents.running = %d, want 0 (no matching wisp for bd.dog)", body.Agents.Running)
	}

	got := getAgentByName(t, st, "bd.dog")
	if got.State != "stopped" {
		t.Errorf("agent state = %q, want %q", got.State, "stopped")
	}
}

// TestGraphResidentNoDoubleCountWhenAlsoProviderRunning pins that an agent that
// is BOTH provider-running and has graph-resident work is counted exactly once.
func TestGraphResidentNoDoubleCountWhenAlsoProviderRunning(t *testing.T) {
	st := bdDogState(t)
	graph := beads.NewMemStore()
	st.graphBeadStore = graph
	seedGraphWisp(t, graph, "bd__dog", "917c4dd26bd772e2463919029a889e0e")
	if err := st.sp.Start(context.Background(), "bd__dog", runtime.Config{}); err != nil {
		t.Fatalf("Start(bd__dog): %v", err)
	}

	body := New(st).buildStatusBody(context.Background(), false)
	if body.Agents.Running != 1 {
		t.Errorf("agents.running = %d, want 1 (running || graph work counts once)", body.Agents.Running)
	}
	if body.Running != 1 {
		t.Errorf("top-level running = %d, want 1", body.Running)
	}
}

// TestSessionPrefixFromWispAssignee unit-tests the assignee splitter that backs
// the graph work index.
func TestSessionPrefixFromWispAssignee(t *testing.T) {
	tests := []struct {
		assignee   string
		wantPrefix string
		wantOK     bool
	}{
		{"bd__dog-gcg-session-917c4dd26bd772e2463919029a889e0e", "bd__dog", true},
		{"gc__implementation-worker-gcg-session-abc123", "gc__implementation-worker", true},
		{"bd__dog", "", false},                  // plain exact-assignee bead
		{"", "", false},                         // empty assignee
		{"-gcg-session-abc", "", false},         // empty prefix rejected
		{"someone-else-in_progress", "", false}, // no wisp infix
	}
	for _, tt := range tests {
		gotPrefix, gotOK := sessionPrefixFromWispAssignee(tt.assignee)
		if gotPrefix != tt.wantPrefix || gotOK != tt.wantOK {
			t.Errorf("sessionPrefixFromWispAssignee(%q) = (%q, %v), want (%q, %v)",
				tt.assignee, gotPrefix, gotOK, tt.wantPrefix, tt.wantOK)
		}
	}
}
