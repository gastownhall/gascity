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

	t.Run("run census (typed event-log projection)", func(t *testing.T) {
		var census genclient.RunsCensusOutputBody
		h.getJSON(h.cityURL("/runs/census"), &census)

		if census.StatusCounts.Active != 1 {
			t.Fatalf("run census active = %d, want 1 seeded active run", census.StatusCounts.Active)
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

// TestCompletedRunProjection is the close-side analog of TestAnchorRunProjection.
// The anchor run only ever exercises the in-progress projection (open root, one
// started step); this asserts the SECOND seeded run — a fully closed molecule
// (root + both steps closed, capped by a molecule.resolved event) — projects
// with TERMINAL status across the same run views. It is the guardrail for the
// close-edge class of break: a projection that silently drops closed roots or
// mis-buckets a completed run leaves the census/summary/detail wrong here even
// though every request still returns 200.
func TestCompletedRunProjection(t *testing.T) {
	h := newHarness(t)

	t.Run("run census counts one active and one completed", func(t *testing.T) {
		var census genclient.RunsCensusOutputBody
		h.getJSON(h.cityURL("/runs/census"), &census)

		// The in-progress anchor run stays active; the closed run-done is the one
		// completed run. A close-side projection break shows up as completed=0.
		if census.StatusCounts.Active != 1 {
			t.Errorf("census active = %d, want 1 (the in-progress anchor run)", census.StatusCounts.Active)
		}
		if census.StatusCounts.Completed != 1 {
			t.Errorf("census completed = %d, want 1 (the closed run-done)", census.StatusCounts.Completed)
		}
		if census.StatusCounts.Failed != 0 {
			t.Errorf("census failed = %d, want 0 (run-done carries no failing gc.outcome)", census.StatusCounts.Failed)
		}
	})

	t.Run("run summary places the completed run in a historical lane", func(t *testing.T) {
		var summary runproj.RunSummary
		h.getJSON(h.apiURL("/runs/summary"), &summary)

		if summary.TotalHistorical == 0 {
			t.Fatal("run summary TotalHistorical = 0; completed run absent from history")
		}
		if !laneInHistorical(summary, completedRunID) {
			t.Errorf("completed run %q not present in summary HistoricalLanes", completedRunID)
		}
		// The in-progress anchor run must NOT leak into history — the phase
		// bucketing (all-closed → complete) is what separates them, so a bug that
		// treats an open root as terminal fails here.
		if laneInHistorical(summary, anchorRunID) {
			t.Errorf("in-progress anchor run %q leaked into HistoricalLanes", anchorRunID)
		}
	})

	t.Run("run detail projects the completed run as terminal", func(t *testing.T) {
		var detail runproj.FormulaRunDetail
		h.getJSON(h.apiURL("/runs/"+completedRunID+"/detail"), &detail)

		if detail.RunID != completedRunID {
			t.Fatalf("runId = %q, want %q", detail.RunID, completedRunID)
		}
		if detail.Title != completedFormula {
			t.Errorf("title = %q, want %q", detail.Title, completedFormula)
		}
		if len(detail.Nodes) == 0 {
			t.Fatal("completed run detail has no nodes; projected empty")
		}
		if len(detail.Lanes) == 0 {
			t.Error("completed run detail has no lanes; projected empty")
		}
		// Root + both closed steps must all read a terminal presentation status,
		// so the whole run reports terminal and phase "complete".
		if !detail.Progress.Terminal {
			t.Error("completed run progress.terminal = false, want true (root + both steps closed)")
		}
		if detail.Phase != "complete" {
			t.Errorf("completed run phase = %q, want \"complete\"", detail.Phase)
		}
	})
}

// laneInHistorical reports whether the run appears specifically in the summary's
// historical (completed) lane bucket, so a caller can assert a completed run
// landed in history and did not leak into the active lanes.
func laneInHistorical(s runproj.RunSummary, id string) bool {
	for _, lane := range s.HistoricalLanes {
		if lane.ID == id {
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

	// The all=true (IncludeClosed) read must surface the close-side rows: the
	// completed run's closed molecule + both closed steps and the closed source
	// task. Closed work is hidden without all=true, so a regression that drops
	// IncludeClosed — or a beads view that filters closed run beads — fails here.
	for _, id := range []string{completedRunID, completedStepA, completedStepB, corpusSourceBeadID} {
		if !beadClosed(list, id) {
			t.Errorf("beads list (all=true) missing seeded closed bead %q with status=closed", id)
		}
	}

	var bead genclient.Bead
	h.getJSON(h.cityURL("/bead/"+corpusWorkBeadID), &bead)
	if bead.Id != corpusWorkBeadID {
		t.Errorf("bead detail id = %q, want %q", bead.Id, corpusWorkBeadID)
	}
	if bead.Title != corpusWorkBeadName {
		t.Errorf("bead detail title = %q, want %q", bead.Title, corpusWorkBeadName)
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

// beadClosed reports whether the list contains a bead with the given id AND a
// "closed" status. It proves the beads view surfaces a real close-side row, not
// merely that the id is present at some non-terminal status.
func beadClosed(list genclient.ListBodyBead, id string) bool {
	if list.Items == nil {
		return false
	}
	for _, b := range *list.Items {
		if b.Id == id {
			return b.Status == "closed"
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

// TestMailThreadProjection is the wire-level guard for the mail thread-detail
// render: the seeded operator↔agent thread must project TWO messages (an
// operator handoff and the agent's reply) with their real bodies through the
// /mail/thread/{id} read the SPA opens a thread with. A regression that collapses
// a thread to one message or drops a body fails here even though /mail still 200s.
func TestMailThreadProjection(t *testing.T) {
	h := newHarness(t)

	var list genclient.MailListBody
	h.getJSON(h.cityURL("/mail"), &list)
	if list.Items == nil {
		t.Fatal("mail list empty; operator thread not projected")
	}
	threadID := ""
	for _, m := range *list.Items {
		if m.Subject == corpusOperatorSubject && m.ThreadId != nil {
			threadID = *m.ThreadId
			break
		}
	}
	if threadID == "" {
		t.Fatalf("no seeded operator thread with subject %q carried a thread_id", corpusOperatorSubject)
	}

	var thread genclient.MailListBody
	h.getJSON(h.cityURL("/mail/thread/"+threadID), &thread)
	if thread.Items == nil || len(*thread.Items) != 2 {
		got := 0
		if thread.Items != nil {
			got = len(*thread.Items)
		}
		t.Fatalf("thread %q projected %d messages, want 2 (handoff + reply)", threadID, got)
	}
	gotHandoff, gotReply := false, false
	for _, m := range *thread.Items {
		switch m.Body {
		case corpusOperatorBody:
			gotHandoff = true
		case corpusAgentReplyBody:
			gotReply = true
		}
	}
	if !gotHandoff || !gotReply {
		t.Errorf("thread bodies incomplete: handoff=%v reply=%v", gotHandoff, gotReply)
	}
}

// TestAgentSessionProjection guards the data the agent-detail view (/agents/{slug})
// resolves against: the seeded session must project into the sessions list with
// its alias/rig/template, and the in-progress AnchorStepID bead must project as
// assigned to that agent's alias — the assignment the AgentBeadsAssigned panel
// renders. Without the session the detail page has only its not-found shell, and
// without the assignment its beads panel is empty; both are load-bearing.
func TestAgentSessionProjection(t *testing.T) {
	h := newHarness(t)

	var sessions genclient.ListBodySessionResponse
	h.getJSON(h.cityURL("/sessions"), &sessions)
	if sessions.Items == nil || len(*sessions.Items) == 0 {
		t.Fatal("sessions list empty; seeded session not projected")
	}
	var seeded *genclient.SessionResponse
	for i := range *sessions.Items {
		s := &(*sessions.Items)[i]
		if s.Alias != nil && *s.Alias == corpusAgentSlug {
			seeded = s
			break
		}
	}
	if seeded == nil {
		t.Fatalf("sessions list missing seeded session with alias %q", corpusAgentSlug)
	}
	if seeded.Rig == nil || *seeded.Rig != corpusRigName {
		t.Errorf("seeded session rig = %v, want %q", seeded.Rig, corpusRigName)
	}
	if seeded.Template != corpusAgentTemplate {
		t.Errorf("seeded session template = %q, want %q", seeded.Template, corpusAgentTemplate)
	}
	if seeded.State == "" {
		t.Error("seeded session projected an empty state; agent-detail StatusBadge would be blank")
	}

	var assigned genclient.ListBodyBead
	h.getJSON(h.cityURL("/beads?assignee="+corpusAgentSlug+"&all=true&limit=200"), &assigned)
	if !beadWithStatus(assigned, anchorStepID, "in_progress") {
		t.Errorf("assigned-beads read missing in-progress %q for agent %q", anchorStepID, corpusAgentSlug)
	}
}

// TestBeadDependencyProjection guards the edge the bead-detail modal renders as
// its populated BeadDependencies branch: AnchorReviewStepID must project a "needs"
// edge onto AnchorStepID. The dashboard builds the dependency graph client-side
// from the bead's needs field, so a projection that drops needs leaves the modal
// showing "No dependencies." even though the beads list still 200s.
func TestBeadDependencyProjection(t *testing.T) {
	h := newHarness(t)

	var list genclient.ListBodyBead
	h.getJSON(h.cityURL("/beads?all=true"), &list)
	if list.Items == nil {
		t.Fatal("beads list empty; dependency edge not projected")
	}
	var review *genclient.Bead
	for i := range *list.Items {
		b := &(*list.Items)[i]
		if b.Id == anchorReviewStepID {
			review = b
			break
		}
	}
	if review == nil {
		t.Fatalf("beads list missing %q", anchorReviewStepID)
	}
	if review.Needs == nil || !contains(*review.Needs, anchorStepID) {
		t.Errorf("%q needs = %v, want to include %q", anchorReviewStepID, review.Needs, anchorStepID)
	}
}

// beadWithStatus reports whether the list contains a bead with the given id AND
// status, proving a real row (not merely a matching id at some other status).
func beadWithStatus(list genclient.ListBodyBead, id, status string) bool {
	if list.Items == nil {
		return false
	}
	for _, b := range *list.Items {
		if b.Id == id {
			return b.Status == status
		}
	}
	return false
}

// contains reports whether s is in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
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
	// The seeded event log carries fifteen records: the five open-run events
	// (3 bead.created + session.woke + bead.updated) plus the completed run's
	// full close-side lifecycle (3 bead.created + session.woke + 2 bead.updated
	// transitions + 3 bead.closed close edges + 1 molecule.resolved). Seeded mail
	// does NOT appear here: it is written via beadmail over MemStore.Create, which
	// emits no event-log entry, and is asserted separately by TestMailView.
	if list.Total < 15 {
		t.Errorf("events total = %d, want >= 15 seeded events", list.Total)
	}

	// Tally the typed envelope union to prove the completed run's close edges and
	// its molecule.resolved record project with their real discriminated types
	// (not a lossy generic decode). A close-side projection break — dropping a
	// bead.closed root, or a molecule.resolved that no longer decodes — fails here
	// even though the feed still returns 200.
	closedSubjects := map[string]bool{}
	moleculeResolvedIssue := ""
	for _, item := range *list.Items {
		kind, err := item.Discriminator()
		if err != nil {
			t.Fatalf("event discriminator: %v", err)
		}
		switch kind {
		case "bead.closed":
			ev, err := item.AsTypedEventStreamEnvelopeBeadClosed()
			if err != nil {
				t.Fatalf("decode bead.closed envelope: %v", err)
			}
			if ev.Subject != nil {
				closedSubjects[*ev.Subject] = true
			}
		case "molecule.resolved":
			ev, err := item.AsTypedEventStreamEnvelopeMoleculeResolved()
			if err != nil {
				t.Fatalf("decode molecule.resolved envelope: %v", err)
			}
			moleculeResolvedIssue = ev.Payload.IssueId
		}
	}

	// Every step close AND the root close must project as a bead.closed edge.
	for _, subject := range []string{completedStepA, completedStepB, completedRunID} {
		if !closedSubjects[subject] {
			t.Errorf("events feed missing bead.closed close edge for %q", subject)
		}
	}
	// The molecule.resolved event projects with its typed payload naming the
	// resolved run — the attribution join for the completed molecule.
	if moleculeResolvedIssue != completedRunID {
		t.Errorf("molecule.resolved payload issue_id = %q, want %q", moleculeResolvedIssue, completedRunID)
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
