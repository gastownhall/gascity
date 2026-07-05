//go:build integration

package dashport_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/runproj"
)

// TestAnchorRunProjection is the serve-level analog of the run-view guardrails
// round-trip: it asserts the seeded run is PRESENT and non-empty at every
// endpoint the run view consumes. The run is seeded two ways from one corpus —
// as a store-resident molecule (the /workflow/{id} read) AND as a bead.* event
// stream in <cityPath>/.gc/events.jsonl (the runproj-backed /api run routes) —
// so a projection break on either path fails here even though every request
// still returns 200. This is the regression this whole harness exists to catch.
func TestAnchorRunProjection(t *testing.T) {
	h := newHarness(t)

	t.Run("workflow snapshot (store projection)", func(t *testing.T) {
		var snap genclient.WorkflowSnapshotResponse
		h.getJSON(h.cityURL("/workflow/"+anchorRunID), &snap)

		if snap.WorkflowId != anchorRunID {
			t.Fatalf("workflow_id = %q, want %q", snap.WorkflowId, anchorRunID)
		}
		if snap.RootBeadId != anchorRunID {
			t.Errorf("root_bead_id = %q, want %q", snap.RootBeadId, anchorRunID)
		}
		if snap.Beads == nil || len(*snap.Beads) == 0 {
			t.Fatal("workflow snapshot has no beads; seeded run projected empty")
		}
		// The root + both steps must all appear.
		gotRoot, gotStep := false, false
		for _, b := range *snap.Beads {
			switch b.Id {
			case anchorRunID:
				gotRoot = true
				if b.Kind != "workflow" {
					t.Errorf("root kind = %q, want workflow", b.Kind)
				}
			case anchorStepID:
				gotStep = true
				// in_progress + an assignee projects as "active" (workflowStatus).
				if b.Status != "active" {
					t.Errorf("preflight step status = %q, want active", b.Status)
				}
			}
		}
		if !gotRoot || !gotStep {
			t.Errorf("snapshot beads missing root=%v or step=%v", gotRoot, gotStep)
		}
		// The root→step dependency edge must project.
		if snap.Deps == nil || len(*snap.Deps) == 0 {
			t.Error("workflow snapshot has no deps; step edge dropped")
		}
	})

	t.Run("run summary (event-log projection)", func(t *testing.T) {
		var summary runproj.RunSummary
		h.getJSON(h.apiURL("/runs/summary"), &summary)

		total := summary.TotalActive + summary.TotalHistorical
		if total == 0 && len(summary.Lanes) == 0 && len(summary.HistoricalLanes) == 0 {
			t.Fatal("run summary projected zero lanes; seeded run absent from the event log projection")
		}
		if !laneRunPresent(summary) {
			t.Errorf("seeded run %q not present in run summary lanes", anchorRunID)
		}
	})

	t.Run("run detail (event-log projection)", func(t *testing.T) {
		var detail runproj.FormulaRunDetail
		h.getJSON(h.apiURL("/runs/"+anchorRunID+"/detail"), &detail)

		if detail.RunID != anchorRunID {
			t.Fatalf("runId = %q, want %q", detail.RunID, anchorRunID)
		}
		if detail.Title != anchorFormula {
			t.Errorf("title = %q, want %q", detail.Title, anchorFormula)
		}
		if len(detail.Nodes) == 0 {
			t.Fatal("run detail has no nodes; seeded run detail projected empty")
		}
		if len(detail.Lanes) == 0 {
			t.Error("run detail has no lanes; seeded run detail projected empty")
		}
	})

	t.Run("formulas feed lists the run", func(t *testing.T) {
		var feed genclient.FormulaFeedBody
		h.getJSON(h.cityURL("/formulas/feed?scope_kind=city&scope_ref="+corpusCityName), &feed)
		if feed.Items == nil || len(*feed.Items) == 0 {
			t.Fatal("formulas feed empty; seeded run not surfaced")
		}
		if !feedRunPresent(feed) {
			t.Errorf("seeded run %q not present in formulas feed", anchorRunID)
		}
	})
}

// laneRunPresent reports whether the seeded run appears across any lane bucket.
func laneRunPresent(s runproj.RunSummary) bool {
	for _, bucket := range [][]runproj.RunLane{s.Lanes, s.HistoricalLanes, s.BlockedLanes} {
		for _, lane := range bucket {
			if lane.ID == anchorRunID {
				return true
			}
		}
	}
	return false
}

// feedRunPresent reports whether the seeded run appears in the formula feed.
func feedRunPresent(f genclient.FormulaFeedBody) bool {
	if f.Items == nil {
		return false
	}
	for _, item := range *f.Items {
		if item.Id == anchorRunID || (item.RootBeadId != nil && *item.RootBeadId == anchorRunID) {
			return true
		}
	}
	return false
}

// TestBeadsView asserts the beads list federates the seeded city store and one
// bead detail projects.
func TestBeadsView(t *testing.T) {
	h := newHarness(t)

	var list genclient.ListBodyBead
	h.getJSON(h.cityURL("/beads?all=true"), &list)
	if list.Items == nil || len(*list.Items) == 0 {
		t.Fatal("beads list empty; seeded beads not federated")
	}
	if !containsBead(list, corpusWorkBeadID) {
		t.Errorf("beads list missing seeded work bead %q", corpusWorkBeadID)
	}

	var bead genclient.Bead
	h.getJSON(h.cityURL("/bead/"+corpusWorkBeadID), &bead)
	if bead.Id != corpusWorkBeadID {
		t.Errorf("bead detail id = %q, want %q", bead.Id, corpusWorkBeadID)
	}
	if bead.Title == "" {
		t.Error("bead detail has empty title; detail projected thin")
	}
}

func containsBead(list genclient.ListBodyBead, id string) bool {
	if list.Items == nil {
		return false
	}
	for _, b := range *list.Items {
		if b.Id == id {
			return true
		}
	}
	return false
}

// TestMailView asserts the seeded mail message projects in the mail list.
func TestMailView(t *testing.T) {
	h := newHarness(t)

	var list genclient.MailListBody
	h.getJSON(h.cityURL("/mail"), &list)
	if list.Items == nil || len(*list.Items) == 0 {
		t.Fatal("mail list empty; seeded message not projected")
	}
	found := false
	for _, m := range *list.Items {
		if m.Subject == corpusMailSubject {
			found = true
		}
	}
	if !found {
		t.Errorf("mail list missing seeded subject %q", corpusMailSubject)
	}
}

// TestAgentsRigsStatusView asserts the config-projection views surface the
// seeded agent, rig, and city status.
func TestAgentsRigsStatusView(t *testing.T) {
	h := newHarness(t)

	var agents genclient.ListBodyAgentResponse
	h.getJSON(h.cityURL("/agents"), &agents)
	if agents.Items == nil || len(*agents.Items) == 0 {
		t.Fatal("agents list empty; seeded agent not projected")
	}

	var rigs genclient.ListBodyRigResponse
	h.getJSON(h.cityURL("/rigs"), &rigs)
	if rigs.Items == nil || len(*rigs.Items) == 0 {
		t.Fatal("rigs list empty; seeded rig not projected")
	}
	found := false
	for _, r := range *rigs.Items {
		if r.Name == corpusRigName {
			found = true
		}
	}
	if !found {
		t.Errorf("rigs list missing seeded rig %q", corpusRigName)
	}

	// Status is read into the raw map only to assert the endpoint serves the
	// city name; decoding the full StatusBody would over-couple this smoke to
	// unrelated store-health fields, so a targeted subset decode is used.
	var status struct {
		Name string `json:"name"`
	}
	h.getJSON(h.cityURL("/status"), &status)
	if status.Name != corpusCityName {
		t.Errorf("status name = %q, want %q", status.Name, corpusCityName)
	}
}

// TestEventsView asserts the events feed projects the seeded log in order with
// typed payloads, and the SSE stream serves.
func TestEventsView(t *testing.T) {
	h := newHarness(t)

	var list genclient.ListBodyWireEvent
	h.getJSON(h.cityURL("/events"), &list)
	if list.Total == 0 || list.Items == nil || len(*list.Items) == 0 {
		t.Fatal("events feed empty; seeded event log not projected")
	}
	// The seeded log carries five events (3 created + woke + updated); the mail
	// send also records a bead.created, so the feed is at least the log length.
	if list.Total < 5 {
		t.Errorf("events total = %d, want >= 5 seeded events", list.Total)
	}

	// The SSE stream endpoint must serve (a heartbeat/frame is enough — the run
	// tailer already asserts the projection). The stream stays open by design, so
	// streamStatus bounds the read with a deadline.
	if code := h.streamStatus(h.cityURL("/events/stream?after_seq=0")); code != http.StatusOK {
		t.Errorf("events/stream status = %d, want 200", code)
	}
}

// TestHealthPlaneAndBFF asserts the typed /health and the host-side /api health
// plane both serve same-origin off the one listener.
func TestHealthPlaneAndBFF(t *testing.T) {
	h := newHarness(t)

	var health genclient.HealthOutputBody
	h.getJSON(h.cityURL("/health"), &health)
	if health.Status == "" {
		t.Error("typed /health returned empty status")
	}

	// The host-side /api plane health endpoint (dashboardbff) serves off the same
	// origin. Its body is an untyped {ok,ts} shape on the non-typed plane.
	code, body := h.getRaw(h.server.URL + "/api/health")
	if code != http.StatusOK {
		t.Fatalf("/api/health status = %d, want 200 (body: %s)", code, truncate(body))
	}
}

// TestSameOriginInvariants promotes the reserved-prefix, SPA-fallback, and
// mutation-CSRF invariants from supervisor_dashboard_test.go to the seeded
// serve-level harness.
func TestSameOriginInvariants(t *testing.T) {
	h := newHarness(t)

	t.Run("SPA shell at root", func(t *testing.T) {
		code, body := h.getRaw(h.rootURL("/"))
		if code != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200", code)
		}
		if !bytes.Contains(body, []byte(`id="root"`)) {
			t.Errorf("GET / did not serve the SPA shell (body: %s)", truncate(body))
		}
	})

	t.Run("SPA fallback for a client route", func(t *testing.T) {
		code, body := h.getRaw(h.rootURL("/city/" + h.cityName + "/agents"))
		if code != http.StatusOK || !bytes.Contains(body, []byte(`id="root"`)) {
			t.Errorf("client route did not fall back to SPA shell: status=%d body=%s", code, truncate(body))
		}
	})

	t.Run("unknown /v0 path is 404, not the SPA shell", func(t *testing.T) {
		if code := h.status(http.MethodGet, h.rootURL("/v0/does-not-exist")); code != http.StatusNotFound {
			t.Errorf("unknown /v0 path status = %d, want 404", code)
		}
	})

	t.Run("api mutation without CSRF header is refused", func(t *testing.T) {
		code := h.status(http.MethodPost, h.server.URL+"/api/city/"+h.cityName+"/config")
		if code != http.StatusForbidden {
			t.Errorf("CSRF-less /api mutation status = %d, want 403", code)
		}
	})
}
