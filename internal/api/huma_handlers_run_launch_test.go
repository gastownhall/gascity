package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
)

func TestRunResourcePathEscaping(t *testing.T) {
	if got, want := runResourcePath("test-city", "gcg-run-1"), "/v0/city/test-city/runs/gcg-run-1"; got != want {
		t.Errorf("runResourcePath = %q, want %q", got, want)
	}
	if got, want := runResourcePath("test-city", "gcg/run 1"), "/v0/city/test-city/runs/gcg%2Frun%201"; got != want {
		t.Errorf("runResourcePath (escaped) = %q, want %q", got, want)
	}
	if got, want := runsListPath("test-city"), "/v0/city/test-city/runs"; got != want {
		t.Errorf("runsListPath = %q, want %q", got, want)
	}
}

// TestSlingLocationRunsListForNonWorkflow: a plain-bead sling produces no single
// run, so its Location is the runs list and it carries no run stanza.
func TestSlingLocationRunsListForNonWorkflow(t *testing.T) {
	h, state := newSlingDashboardTestServer(t, "")
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/v0/city/test-city/runs"; got != want {
		t.Errorf("Location = %q, want %q (runs list for a non-workflow sling)", got, want)
	}
	if strings.Contains(rec.Body.String(), `"run"`) {
		t.Errorf("body = %s, want no run stanza for a non-workflow sling", rec.Body.String())
	}
}

// TestSlingLocationAndRunStanzaForGraphLaunch: a graph.v2 launch mints a run root,
// so its Location deep-links the run and the body carries the run stanza.
func TestSlingLocationAndRunStanzaForGraphLaunch(t *testing.T) {
	setFormulaV2 := formulatest.LockV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prevGraphApply) })

	h, state := newSlingDashboardTestServer(t, "")
	setFormulaV2(true)
	molecule.SetGraphApplyEnabled(true)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	state.cfg.Agents = append(state.cfg.Agents,
		config.Agent{Name: config.ControlDispatcherAgentName, MaxActiveSessions: intPtr(1)},
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "myrig", MaxActiveSessions: intPtr(1)},
	)
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.stores["myrig"]
	source, err := store.Create(beads.Bead{ID: "BL-42", Title: "test task", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","formula":"graph-work","attached_bead_id":"` + source.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		WorkflowID string `json:"workflow_id"`
		Run        *struct {
			RunID  string `json:"run_id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"run"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WorkflowID == "" {
		t.Fatal("workflow_id empty, want a graph.v2 launch to mint a run root")
	}
	if got, want := rec.Header().Get("Location"), "/v0/city/test-city/runs/"+resp.WorkflowID; got != want {
		t.Errorf("Location = %q, want %q (run detail for a workflow launch)", got, want)
	}
	if resp.Run == nil {
		t.Fatal("run stanza missing, want it present for a workflow launch")
	}
	if resp.Run.RunID != resp.WorkflowID {
		t.Errorf("run.run_id = %q, want %q", resp.Run.RunID, resp.WorkflowID)
	}
	if resp.Run.Kind != "sling" {
		t.Errorf("run.kind = %q, want sling", resp.Run.Kind)
	}
	if resp.Run.Status != string(RunStatusPending) {
		t.Errorf("run.status = %q, want pending", resp.Run.Status)
	}
}

// TestOrderRunLocationRunsList: an order dispatches asynchronously, so its
// Location is the runs list (the run appears there once it materializes).
func TestOrderRunLocationRunsList(t *testing.T) {
	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/order/"+prReviewOrderName+"/run"), strings.NewReader(`{"vars":{"repo":"acme/widgets"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/v0/city/test-city/runs"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
