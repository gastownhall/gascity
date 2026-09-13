package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// runDetailRootEvent builds the canonical graph.v2 run-root molecule (run "run1",
// formula mol-adopt-pr-v2) with the scope metadata the detail snapshot projection
// requires (gc.scope_kind / gc.scope_ref / gc.root_store_ref). It is the first
// event in these fixtures, so it carries seq 1.
func runDetailRootEvent() events.Event {
	const (
		runID   = "run1"
		formula = "mol-adopt-pr-v2"
	)
	return beadCreatedEvent(1, beads.Bead{
		ID:        runID,
		Title:     formula,
		Status:    "open",
		Type:      "molecule",
		Ref:       formula,
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
			"gc.run_target":       "rig:demo",
			"gc.root_store_ref":   "rig:demo",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "demo",
		},
	})
}

// runDetailStepEvent builds a step bead parented to a run root.
func runDetailStepEvent(seq uint64, id, parent, stepID, status string) events.Event {
	return beadCreatedEvent(seq, beads.Bead{
		ID:        id,
		Title:     stepID,
		Status:    status,
		Type:      "task",
		ParentID:  parent,
		Ref:       "mol-adopt-pr-v2." + stepID,
		CreatedAt: time.Date(2026, 6, 1, 10, 1, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.kind":         "step",
			"gc.root_bead_id": parent,
			"gc.step_id":      stepID,
			"gc.scope_ref":    "demo",
		},
	})
}

func beadCreatedEvent(seq uint64, b beads.Bead) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}

// runDetailWire is the decoded detail body — a structural contract check that the
// wire carries the FormulaRunDetail shape the SPA renderer reads.
type runDetailWire struct {
	RunID    string `json:"runId"`
	ScopeRef string `json:"scopeRef"`
	Title    string `json:"title"`
	Formula  struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"formula"`
	Phase string `json:"phase"`
	Nodes []struct {
		ID string `json:"id"`
	} `json:"nodes"`
	Lanes []struct {
		ID string `json:"id"`
	} `json:"lanes"`
	FormulaDetail struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Target  string `json:"target"`
		Reason  string `json:"reason"`
		Failure string `json:"failure"`
	} `json:"formulaDetail"`
}

// TestRunDetailEndpoint drives the full endpoint: the warm fold projects one
// run's detail graph (root + step) off the same tailer the summary uses.
func TestRunDetailEndpoint(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "alpha", "run1")
	if resp.RunID != "run1" {
		t.Errorf("runId = %q, want run1", resp.RunID)
	}
	if resp.ScopeRef != "demo" {
		t.Errorf("scopeRef = %q, want demo", resp.ScopeRef)
	}
	if resp.Title != "mol-adopt-pr-v2" {
		t.Errorf("title = %q, want mol-adopt-pr-v2", resp.Title)
	}
	if resp.Formula.Kind != "known" || resp.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("formula = %+v, want known/mol-adopt-pr-v2", resp.Formula)
	}
	if len(resp.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (root + preflight)", len(resp.Nodes))
	}
	if len(resp.Lanes) != 1 || resp.Lanes[0].ID != "demo" {
		t.Errorf("lanes = %+v, want one lane 'demo'", resp.Lanes)
	}
	if resp.Phase == "" {
		t.Errorf("phase is empty, want a classified phase")
	}
}

// TestRunDetailEndpointFiltersNonRunBeads is the regression guard for the live
// projection bypassing RunBeadFilter: message, session, and gc:-labeled control
// beads that share a run root must be dropped at the projection boundary (the
// analog of the frontend defaultBeadFilter) so they never surface as detail
// nodes. Without the filter, the gc:-labeled child and the message bead below
// are selected as run members and would inflate the node count past the real
// root+step graph.
func TestRunDetailEndpointFiltersNonRunBeads(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	// A distinct run id and city (filtering is not run- or city-specific) also
	// keep the shared getRunDetail helper exercised with more than one run/city.
	const runID = "runf1"
	writeEventLog(t, logPath,
		beadCreatedEvent(1, beads.Bead{
			ID:        runID,
			Title:     "mol-adopt-pr-v2",
			Status:    "open",
			Type:      "molecule",
			Ref:       "mol-adopt-pr-v2",
			CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Metadata: map[string]string{
				"gc.formula_contract": "graph.v2",
				"gc.kind":             "run",
				"gc.formula":          "mol-adopt-pr-v2",
				"gc.run_target":       "rig:demo",
				"gc.root_store_ref":   "rig:demo",
				"gc.scope_kind":       "rig",
				"gc.scope_ref":        "demo",
			},
		}),
		runDetailStepEvent(2, runID+".1", runID, "preflight", "in_progress"),
		// A gc:-labeled control bead whose id sits under the run root: without the
		// filter, snapshotForRun selects it as a member and it becomes a node.
		beadCreatedEvent(3, beads.Bead{
			ID:        runID + ".ctl",
			Title:     "control bead",
			Status:    "open",
			Type:      "task",
			ParentID:  runID,
			Labels:    []string{"gc:control"},
			CreatedAt: time.Date(2026, 6, 1, 10, 2, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 6, 1, 10, 6, 0, 0, time.UTC),
			Metadata:  map[string]string{"gc.root_bead_id": runID},
		}),
		// A message bead carrying the run root: not an engineering type and not
		// gc.kind=run, so RunBeadFilter drops it.
		beadCreatedEvent(4, beads.Bead{
			ID:        "msg1",
			Title:     "convoy message",
			Status:    "open",
			Type:      "message",
			CreatedAt: time.Date(2026, 6, 1, 10, 3, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 6, 1, 10, 7, 0, 0, time.UTC),
			Metadata:  map[string]string{"gc.root_bead_id": runID},
		}),
	)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"beta": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "beta", runID)
	if len(resp.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (root + preflight); non-run/gc:-labeled beads must be filtered, got %+v", len(resp.Nodes), resp.Nodes)
	}
	for _, node := range resp.Nodes {
		if node.ID == runID+".ctl" || node.ID == "msg1" {
			t.Errorf("node %q leaked into detail; RunBeadFilter must drop it", node.ID)
		}
	}
}

// TestRunDetailEndpointLayersCompiledFormulaDetail proves the endpoint fetches
// the supervisor's compiled formula detail at request time (like sessions) so a
// graph.v2 run with a name+target resolves to an "available" formula-detail
// state rather than the synthetic fetch_failed/upstream_error the bead-derived
// projection emits on its own.
func TestRunDetailEndpointLayersCompiledFormulaDetail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)

	var gotFormulaQuery string
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/city/alpha/formulas/mol-adopt-pr-v2" {
			gotFormulaQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"mol-adopt-pr-v2","steps":[{"id":"preflight"},{"id":"apply-fixes"}],"preview":{"nodes":[{"id":"preflight"},{"id":"apply-fixes"}]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer supervisor.Close()

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "alpha", "run1")
	if resp.FormulaDetail.Kind != "available" {
		t.Errorf("formulaDetail.kind = %q, want available; full=%+v", resp.FormulaDetail.Kind, resp.FormulaDetail)
	}
	if resp.FormulaDetail.Name != "mol-adopt-pr-v2" || resp.FormulaDetail.Target != "rig:demo" {
		t.Errorf("formulaDetail = %+v, want name mol-adopt-pr-v2 / target rig:demo", resp.FormulaDetail)
	}
	if resp.FormulaDetail.Failure != "" {
		t.Errorf("formulaDetail.failure = %q, want empty (no synthetic upstream error)", resp.FormulaDetail.Failure)
	}
	// The run root is rig-scoped (gc.scope_kind=rig, gc.scope_ref=demo). The BFF
	// must derive that scope from the run root and send it alongside target, so
	// the endpoint resolves the compiled formula against the rig formula layer
	// instead of the wrong layer or a required-scope rejection.
	gotQuery, err := url.ParseQuery(gotFormulaQuery)
	if err != nil {
		t.Fatalf("parse compiled-formula fetch query %q: %v", gotFormulaQuery, err)
	}
	if got := gotQuery.Get("target"); got != "rig:demo" {
		t.Errorf("compiled-formula fetch target = %q, want rig:demo (query %q)", got, gotFormulaQuery)
	}
	if got := gotQuery.Get("scope_kind"); got != "rig" {
		t.Errorf("compiled-formula fetch scope_kind = %q, want rig (query %q)", got, gotFormulaQuery)
	}
	if got := gotQuery.Get("scope_ref"); got != "demo" {
		t.Errorf("compiled-formula fetch scope_ref = %q, want demo (query %q)", got, gotFormulaQuery)
	}
}

// TestRunDetailEndpointFormulaFetchFailureStaysHonest proves that when the
// compiled-formula fetch is attempted but fails upstream, the detail state falls
// back to the honest fetch_failed arm rather than fabricating availability.
func TestRunDetailEndpointFormulaFetchFailureStaysHonest(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent())

	var formulaFetchAttempted bool
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/city/alpha/formulas/mol-adopt-pr-v2" {
			formulaFetchAttempted = true
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer supervisor.Close()

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "alpha", "run1")
	if !formulaFetchAttempted {
		t.Error("compiled-formula fetch was not attempted for a graph.v2 run with a target")
	}
	if resp.FormulaDetail.Kind != "unavailable" || resp.FormulaDetail.Reason != "fetch_failed" {
		t.Errorf("formulaDetail = %+v, want unavailable/fetch_failed on upstream failure", resp.FormulaDetail)
	}
	if resp.FormulaDetail.Failure != "upstream_error" {
		t.Errorf("formulaDetail.failure = %q, want upstream_error", resp.FormulaDetail.Failure)
	}
}

// TestRunDetailEndpointFormulaFetch404IsNotFound proves the BFF preserves the
// distinct not_found failure reason across the BFF/runproj boundary: a compiled
// formula that the supervisor reports as HTTP 404 must resolve to
// fetch_failed/not_found — a genuinely missing formula — rather than collapsing
// into the generic upstream_error the non-404 path (see the sibling test above)
// reports. Before this fix the BFF discarded the status code, so a 404 rendered
// the wrong operator diagnostic on the run-detail page.
func TestRunDetailEndpointFormulaFetch404IsNotFound(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent())

	var formulaFetchAttempted bool
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/city/alpha/formulas/mol-adopt-pr-v2" {
			formulaFetchAttempted = true
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer supervisor.Close()

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "alpha", "run1")
	if !formulaFetchAttempted {
		t.Error("compiled-formula fetch was not attempted for a graph.v2 run with a target")
	}
	if resp.FormulaDetail.Kind != "unavailable" || resp.FormulaDetail.Reason != "fetch_failed" {
		t.Errorf("formulaDetail = %+v, want unavailable/fetch_failed on a 404", resp.FormulaDetail)
	}
	if resp.FormulaDetail.Failure != "not_found" {
		t.Errorf("formulaDetail.failure = %q, want not_found (a 404 is a missing formula, not a generic upstream error)", resp.FormulaDetail.Failure)
	}
}

// TestRunDetailEndpointUnknownCity404 confirms an unresolvable city 404s.
func TestRunDetailEndpointUnknownCity404(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/runs/run1/detail", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown city", rec.Code)
	}
}

// A missing run once the tailer is warm answers 503 for the unknown-run grace
// window and 404 after it expires — covered by
// TestRunDetailEndpointUnknownRunWarmingGrace in rundetail_grace_test.go.

// TestRunDetailEndpointNotRunView maps a non-graph.v2 run to 422 with the
// not_run_view reason so the SPA renders the honest list-only message.
func TestRunDetailEndpointNotRunView(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	// A molecule run marker but NO gc.formula_contract=graph.v2 → not a run view.
	writeEventLog(t, logPath, beadCreatedEvent(1, beads.Bead{
		ID:        "v1run",
		Title:     "legacy v1 run",
		Status:    "open",
		Type:      "molecule",
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{"gc.kind": "run"},
	}))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	rec := getRunDetailRaw(t, p, "alpha", "v1run")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var body runDetailErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, rec.Body.String())
	}
	if body.Reason != "not_run_view" {
		t.Errorf("reason = %q, want not_run_view", body.Reason)
	}
}

func getRunDetailRaw(t *testing.T, p *Plane, city, runID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/"+city+"/runs/"+runID+"/detail", nil))
	return rec
}

func expectRunDetailStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

// getRunDetail fetches a run's detail and decodes the success (200) body. Non-2xx
// paths use getRunDetailRaw / expectRunDetailStatus, so the expected status is
// fixed here rather than a parameter.
func getRunDetail(t *testing.T, p *Plane, city, runID string) runDetailWire {
	t.Helper()
	rec := getRunDetailRaw(t, p, city, runID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp runDetailWire
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}
