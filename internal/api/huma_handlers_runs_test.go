package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

// runFixtureID gives a stable, zero-padded run id for cap/ordering fixtures.
func runFixtureID(i int) string { return fmt.Sprintf("run-%02d", i) }

// beadCreatedEvent builds a bead.created event carrying b in the wrapped
// {"bead": ...} payload the recorder emits.
func beadCreatedEvent(seq uint64, b beads.Bead) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}

func writeRunEventLog(t *testing.T, cityPath string, evts ...events.Event) {
	t.Helper()
	logPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// runRootBead builds a graph.v2 run-root molecule with a resolvable city scope.
func runRootBead(id, formula, status string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     "Run " + id,
		Status:    status,
		Type:      "molecule",
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	}
}

func runChildBead(id, rootID, status string, extraMeta map[string]string) beads.Bead {
	md := map[string]string{"gc.root_bead_id": rootID}
	for k, v := range extraMeta {
		md[k] = v
	}
	return beads.Bead{
		ID:        id,
		Title:     "Step " + id,
		Status:    status,
		Type:      "task",
		CreatedAt: time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		Metadata:  md,
	}
}

func newRunServer(t *testing.T, evts ...events.Event) *Server {
	t.Helper()
	fs := newFakeState(t)
	if len(evts) > 0 {
		writeRunEventLog(t, fs.cityPath, evts...)
	}
	return &Server{state: fs}
}

func TestDeriveRunStatus(t *testing.T) {
	openRoot := beads.Bead{Status: "open"}
	closedRoot := func(outcome string) beads.Bead {
		md := map[string]string{}
		if outcome != "" {
			md["gc.outcome"] = outcome
		}
		return beads.Bead{Status: "closed", Metadata: md}
	}
	cases := []struct {
		name      string
		phase     string
		root      beads.Bead
		rootFound bool
		started   int
		want      RunStatus
	}{
		// Non-terminal: root open → phase + started work classify it.
		{"active-no-work", "active", openRoot, true, 0, RunStatusPending},
		{"active-with-work", "active", openRoot, true, 2, RunStatusActive},
		{"blocked", "blocked", openRoot, true, 1, RunStatusWaiting},
		// Terminal: the ROOT's own closure is authoritative, independent of phase.
		{"closed-pass", "complete", closedRoot("pass"), true, 3, RunStatusCompleted},
		{"closed-no-outcome", "complete", closedRoot(""), true, 3, RunStatusCompleted},
		{"closed-fail", "complete", closedRoot("fail"), true, 3, RunStatusFailed},
		{"closed-skipped", "complete", closedRoot("skipped"), true, 0, RunStatusSkipped},
		// F2 regression: a failed run whose lane phase is NOT complete (a lingering
		// open source bead) must still report failed because the root is closed.
		{"closed-fail-phase-active", "active", closedRoot("fail"), true, 5, RunStatusFailed},
		// Cancel: a terminal canceled outcome is a distinct terminal status.
		{"closed-canceled", "complete", closedRoot("canceled"), true, 3, RunStatusCanceled},
		// Cancel: an open root carrying the intent marker reports canceling.
		{"open-cancel-requested", "active", beads.Bead{Status: "open", Metadata: map[string]string{"gc.cancel_requested": "true"}}, true, 2, RunStatusCanceling},
		// Defensive: a dangling root (filtered upstream in practice) is non-terminal.
		{"root-missing", "complete", beads.Bead{}, false, 0, RunStatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lane := runproj.RunLane{Phase: tc.phase}
			got := deriveRunStatus(lane, tc.root, tc.rootFound, tc.started)
			if got != tc.want {
				t.Fatalf("deriveRunStatus(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestDeriveRunStepStatus(t *testing.T) {
	step := func(status, outcome string) beads.Bead {
		md := map[string]string{}
		if outcome != "" {
			md["gc.outcome"] = outcome
		}
		return beads.Bead{Status: status, Metadata: md}
	}
	cases := []struct {
		name string
		bead beads.Bead
		want RunStepStatus
	}{
		{"open", step("open", ""), RunStepStatusPending},
		{"in-progress", step("in_progress", ""), RunStepStatusActive},
		{"blocked", step("blocked", ""), RunStepStatusBlocked},
		{"closed-pass", step("closed", "pass"), RunStepStatusCompleted},
		{"closed-fail", step("closed", "fail"), RunStepStatusFailed},
		{"closed-skipped", step("closed", "skipped"), RunStepStatusSkipped},
		{"closed-canceled", step("closed", "canceled"), RunStepStatusCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveRunStepStatus(tc.bead); got != tc.want {
				t.Fatalf("deriveRunStepStatus(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestRunsListEndpoint(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-active", "mol-adopt-pr-v2", "open")),
		beadCreatedEvent(2, runChildBead("run-active.step1", "run-active", "in_progress", nil)),
	)

	out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{
		CityScope: CityScope{CityName: "test-city"},
	})
	if err != nil {
		t.Fatalf("humaHandleRunsList error: %v", err)
	}
	if len(out.Body.Runs) != 1 {
		t.Fatalf("got %d runs, want 1: %+v", len(out.Body.Runs), out.Body.Runs)
	}
	run := out.Body.Runs[0]
	if run.RunID != "run-active" {
		t.Errorf("run_id = %q, want run-active", run.RunID)
	}
	if run.Formula != "mol-adopt-pr-v2" {
		t.Errorf("formula = %q, want mol-adopt-pr-v2", run.Formula)
	}
	if run.Status != RunStatusActive {
		t.Errorf("status = %q, want active", run.Status)
	}
	if run.Scope.Kind != "city" || run.Scope.Ref != "test-city" {
		t.Errorf("scope = %+v, want {city test-city}", run.Scope)
	}
	if run.StartedAt == "" {
		t.Errorf("started_at empty, want RFC3339 timestamp")
	}
	if out.Body.StatusCounts.Active != 1 {
		t.Errorf("status_counts.active = %d, want 1", out.Body.StatusCounts.Active)
	}
}

type fakeRunCensusSource struct {
	value runproj.CanonicalRunCensus
	ok    bool
}

func (f fakeRunCensusSource) RunCensus(context.Context, string) (runproj.CanonicalRunCensus, bool) {
	return f.value, f.ok
}

func TestRunsCensusEndpointUsesWarmProjectionWithoutRows(t *testing.T) {
	s := newRunServer(t)
	s.runCensusSource = fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Ready:        true,
			StatusCounts: runproj.CanonicalRunStatusCounts{Active: 2, Waiting: 1},
		},
	}

	out, err := s.humaHandleRunsCensus(context.Background(), &RunsCensusInput{
		CityScope: CityScope{CityName: "test-city"},
	})
	if err != nil {
		t.Fatalf("humaHandleRunsCensus error: %v", err)
	}
	want := RunStatusCounts{Active: 2, Waiting: 1}
	if out.Body.StatusCounts != want {
		t.Fatalf("status_counts = %+v, want %+v", out.Body.StatusCounts, want)
	}
	if out.Body.Partial || len(out.Body.PartialErrors) != 0 {
		t.Fatalf("complete census reported partial: %+v", out.Body)
	}
}

func TestRunsCensusEndpointReportsWarmingWithoutInternalDetails(t *testing.T) {
	s := newRunServer(t)
	s.runCensusSource = fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Partial:        true,
			PartialReasons: []string{"read /private/path/events.jsonl: permission denied"},
		},
	}

	_, err := s.humaHandleRunsCensus(context.Background(), &RunsCensusInput{
		CityScope: CityScope{CityName: "test-city"},
	})
	if err == nil {
		t.Fatal("humaHandleRunsCensus error = nil, want warming 503")
	}
	var statusErr huma.StatusError
	if !errors.As(err, &statusErr) || statusErr.GetStatus() != http.StatusServiceUnavailable {
		t.Fatalf("error = %T %v, want Huma 503", err, err)
	}
	if strings.Contains(err.Error(), "/private/path") || strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("warming error leaked internal detail: %v", err)
	}
}

func TestRunsCensusWireContainsOnlyCountsAndPartialProvenance(t *testing.T) {
	s := newRunServer(t)
	s.runCensusSource = fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Ready:        true,
			StatusCounts: runproj.CanonicalRunStatusCounts{Pending: 1, Active: 2},
		},
	}
	h := newTestCityHandlerWith(t, s.state, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(s.state, "/runs/census"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(fields) != 1 || fields["status_counts"] == nil {
		t.Fatalf("response keys = %v, want only status_counts", fields)
	}
	for _, forbidden := range []string{"runs", "title", "target", "scope", "last_error", "assignee"} {
		if strings.Contains(rec.Body.String(), `"`+forbidden+`"`) {
			t.Fatalf("census response contains forbidden field %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestRunsCensusWireReturnsSanitized503WhileWarming(t *testing.T) {
	s := newRunServer(t)
	s.runCensusSource = fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Partial:        true,
			PartialReasons: []string{"read /private/path/events.jsonl: permission denied"},
		},
	}
	h := newTestCityHandlerWith(t, s.state, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(s.state, "/runs/census"), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/private/path") || strings.Contains(rec.Body.String(), "permission denied") {
		t.Fatalf("warming response leaked internal detail: %s", rec.Body.String())
	}
}

func TestRunsCensusWireSanitizesReadyPartialReasons(t *testing.T) {
	s := newRunServer(t)
	s.runCensusSource = fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Ready:          true,
			Partial:        true,
			PartialReasons: []string{"read /private/path/events.jsonl: permission denied"},
		},
	}
	h := newTestCityHandlerWith(t, s.state, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(s.state, "/runs/census"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/private/path") || strings.Contains(rec.Body.String(), "permission denied") {
		t.Fatalf("partial response leaked internal detail: %s", rec.Body.String())
	}
	var body RunsCensusOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &body.Body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Body.Partial || len(body.Body.PartialErrors) != 1 || body.Body.PartialErrors[0] != "run projection is incomplete" {
		t.Fatalf("partial response = %+v, want one sanitized incomplete reason", body.Body)
	}
}

func TestSupervisorMuxThreadsRunCensusSourceIntoLazyCityServer(t *testing.T) {
	state := newFakeState(t)
	mux := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now())
	mux.WithRunCensusSource(fakeRunCensusSource{
		ok: true,
		value: runproj.CanonicalRunCensus{
			Ready:        true,
			StatusCounts: runproj.CanonicalRunStatusCounts{Waiting: 3},
		},
	})
	h := wrapTestSupervisorMiddleware(mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/runs/census"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body RunsCensusOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &body.Body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Body.StatusCounts.Waiting != 3 {
		t.Fatalf("status_counts = %+v, want waiting=3", body.Body.StatusCounts)
	}
}

func TestRunsListStatusCountsAreNotTruncatedByLimit(t *testing.T) {
	completed := runRootBead("run-complete", "mol-adopt-pr-v2", "closed")
	completed.Metadata["gc.outcome"] = "pass"
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-active", "mol-adopt-pr-v2", "open")),
		beadCreatedEvent(2, runChildBead("run-active.step", "run-active", "in_progress", nil)),
		beadCreatedEvent(3, completed),
	)

	out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{
		CityScope: CityScope{CityName: "test-city"},
		Limit:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Runs) != 1 {
		t.Fatalf("len(runs) = %d, want limit 1", len(out.Body.Runs))
	}
	if out.Body.StatusCounts.Active != 1 || out.Body.StatusCounts.Completed != 1 {
		t.Fatalf("status_counts = %+v, want active=1 completed=1", out.Body.StatusCounts)
	}
	if !out.Body.Partial {
		t.Fatal("Partial = false, want true when the row list is limited")
	}
}

func TestRunsListStatusCountsIncludeHistoryBeyondTheLaneCap(t *testing.T) {
	const completedRuns = 55
	events := make([]events.Event, 0, completedRuns)
	for i := range completedRuns {
		root := runRootBead(runFixtureID(i), "mol-adopt-pr-v2", "closed")
		root.Metadata["gc.outcome"] = "pass"
		events = append(events, beadCreatedEvent(uint64(i+1), root))
	}
	s := newRunServer(t, events...)

	out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{
		CityScope: CityScope{CityName: "test-city"},
		Limit:     maxRunsListLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.StatusCounts.Completed != completedRuns {
		t.Fatalf("status_counts.completed = %d, want %d", out.Body.StatusCounts.Completed, completedRuns)
	}
	if len(out.Body.Runs) != 50 || !out.Body.Partial {
		t.Fatalf("rows/partial = %d/%v, want capped 50/true", len(out.Body.Runs), out.Body.Partial)
	}
}

func TestRunsListEmptyCity(t *testing.T) {
	s := newRunServer(t) // no event log written
	out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{
		CityScope: CityScope{CityName: "test-city"},
	})
	if err != nil {
		t.Fatalf("humaHandleRunsList error: %v", err)
	}
	if len(out.Body.Runs) != 0 {
		t.Fatalf("got %d runs, want 0 for a city with no event log", len(out.Body.Runs))
	}
}

func TestRunGet(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run1", "mol-design-review-v2", "open")),
	)
	out, err := s.humaHandleRunGet(context.Background(), &RunGetInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "run1",
	})
	if err != nil {
		t.Fatalf("humaHandleRunGet error: %v", err)
	}
	if out.Body.RunID != "run1" {
		t.Errorf("run_id = %q, want run1", out.Body.RunID)
	}
	if out.Body.Formula != "mol-design-review-v2" {
		t.Errorf("formula = %q, want mol-design-review-v2", out.Body.Formula)
	}
}

func TestRunGetNotFound(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run1", "mol-design-review-v2", "open")),
	)
	_, err := s.humaHandleRunGet(context.Background(), &RunGetInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "ghost",
	})
	if err == nil {
		t.Fatal("humaHandleRunGet(ghost) = nil error, want run-not-found")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error = %q, want run-not-found detail", err.Error())
	}
}

func TestRunStepsEndpoint(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run1", "mol-adopt-pr-v2", "open")),
		beadCreatedEvent(2, runChildBead("run1.step1", "run1", "closed", map[string]string{"gc.outcome": "pass"})),
		beadCreatedEvent(3, runChildBead("run1.step2", "run1", "in_progress", nil)),
	)
	out, err := s.humaHandleRunSteps(context.Background(), &RunStepsInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "run1",
	})
	if err != nil {
		t.Fatalf("humaHandleRunSteps error: %v", err)
	}
	if out.Body.RunID != "run1" {
		t.Errorf("run_id = %q, want run1", out.Body.RunID)
	}
	if len(out.Body.Steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(out.Body.Steps), out.Body.Steps)
	}
	byID := map[string]RunStep{}
	for _, st := range out.Body.Steps {
		byID[st.ID] = st
	}
	if byID["run1.step1"].Status != RunStepStatusCompleted {
		t.Errorf("step1 status = %q, want completed", byID["run1.step1"].Status)
	}
	if byID["run1.step2"].Status != RunStepStatusActive {
		t.Errorf("step2 status = %q, want active", byID["run1.step2"].Status)
	}
}

// TestRunGetBeyondHistoricalCap guards the false-404 defect: with more completed
// runs than the projection's historical lane cap, every run must still resolve by
// id (the single-run path bypasses the list cap via BuildRunLane).
func TestRunGetBeyondHistoricalCap(t *testing.T) {
	var evts []events.Event
	for i := 0; i < 55; i++ {
		root := runRootBead(runFixtureID(i), "mol-adopt-pr-v2", "closed")
		root.Metadata["gc.outcome"] = "pass"
		evts = append(evts, beadCreatedEvent(uint64(i+1), root))
	}
	s := newRunServer(t, evts...)

	// run-00 is one of the oldest and would be truncated from the list.
	out, err := s.humaHandleRunGet(context.Background(), &RunGetInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     runFixtureID(0),
	})
	if err != nil {
		t.Fatalf("humaHandleRunGet(%s) error: %v (a real run must not 404 for being past the list cap)", runFixtureID(0), err)
	}
	if out.Body.RunID != runFixtureID(0) {
		t.Errorf("run_id = %q, want %q", out.Body.RunID, runFixtureID(0))
	}
	if out.Body.Status != RunStatusCompleted {
		t.Errorf("status = %q, want completed", out.Body.Status)
	}
}

// TestRunGetFailedWithOpenSource guards F2: a run whose root closed with a failure
// outcome but whose lane still holds an open source bead (so the lane phase is not
// "complete") must report failed, not active.
func TestRunGetFailedWithOpenSource(t *testing.T) {
	root := runRootBead("runf", "mol-adopt-pr-v2", "closed")
	root.Metadata["gc.outcome"] = "fail"
	// An open source bead grouped into the run via pr_review.run_root_id keeps the
	// lane phase from reaching "complete".
	source := beads.Bead{
		ID:       "ga-source",
		Title:    "source issue",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"pr_review.run_root_id": "runf"},
	}
	s := newRunServer(t,
		beadCreatedEvent(1, root),
		beadCreatedEvent(2, source),
	)

	out, err := s.humaHandleRunGet(context.Background(), &RunGetInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "runf",
	})
	if err != nil {
		t.Fatalf("humaHandleRunGet(runf) error: %v", err)
	}
	if out.Body.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed (root closed with fail outcome)", out.Body.Status)
	}
	if out.Body.LastError == nil || out.Body.LastError.Code != "fail" {
		t.Errorf("last_error = %+v, want code=fail", out.Body.LastError)
	}
}

// TestRunGetFailedExposesFailureReason guards the last_error contract: a failed
// run root stamps the actionable machine reason in gc.failure_reason (as
// dispatch control/drain do on a hard fail), so last_error.code must surface that
// reason — not the coarse gc.outcome=fail — and last_error.message must carry the
// controller's human-readable error rather than the never-written close_reason.
func TestRunGetFailedExposesFailureReason(t *testing.T) {
	root := runRootBead("runfr", "mol-adopt-pr-v2", "closed")
	root.Metadata["gc.outcome"] = "fail"
	root.Metadata["gc.failure_reason"] = "rate_limited"
	root.Metadata["gc.controller_error"] = "provider returned 429: slow down"
	s := newRunServer(t, beadCreatedEvent(1, root))

	out, err := s.humaHandleRunGet(context.Background(), &RunGetInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "runfr",
	})
	if err != nil {
		t.Fatalf("humaHandleRunGet(runfr) error: %v", err)
	}
	if out.Body.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", out.Body.Status)
	}
	if out.Body.LastError == nil {
		t.Fatal("last_error = nil, want the graph failure reason exposed on the wire")
	}
	if out.Body.LastError.Code != "rate_limited" {
		t.Errorf("last_error.code = %q, want rate_limited (the actionable gc.failure_reason, not the coarse outcome)", out.Body.LastError.Code)
	}
	if out.Body.LastError.Message != "provider returned 429: slow down" {
		t.Errorf("last_error.message = %q, want the controller error text", out.Body.LastError.Message)
	}
}

// TestRunsWireRoute drives the endpoints through the real SupervisorMux HTTP
// router — verifying route registration, {cityName} binding, JSON serialization,
// the closed status enum on the wire, and that a missing run maps to a 404.
func TestRunsWireRoute(t *testing.T) {
	fs := newFakeState(t)
	writeRunEventLog(t, fs.cityPath,
		beadCreatedEvent(1, runRootBead("run-wire", "mol-adopt-pr-v2", "open")),
		beadCreatedEvent(2, runChildBead("run-wire.s1", "run-wire", "in_progress", nil)),
	)
	sm := newTestSupervisorMux(t, map[string]*fakeState{"test-city": fs})

	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/city/test-city/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Runs []struct {
			RunID   string `json:"run_id"`
			Status  string `json:"status"`
			Formula string `json:"formula"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode /runs body: %v; raw=%s", err, rec.Body.String())
	}
	if len(list.Runs) != 1 || list.Runs[0].RunID != "run-wire" {
		t.Fatalf("runs = %+v, want one run-wire", list.Runs)
	}
	if list.Runs[0].Status != string(RunStatusActive) {
		t.Errorf("wire status = %q, want %q", list.Runs[0].Status, RunStatusActive)
	}
	if list.Runs[0].Formula != "mol-adopt-pr-v2" {
		t.Errorf("wire formula = %q, want mol-adopt-pr-v2", list.Runs[0].Formula)
	}

	// A missing run must route to a 404 on the wire.
	rec404 := httptest.NewRecorder()
	sm.ServeHTTP(rec404, httptest.NewRequest(http.MethodGet, "/v0/city/test-city/runs/ghost", nil))
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("GET /runs/ghost = %d, want 404; body=%s", rec404.Code, rec404.Body.String())
	}
	if !strings.Contains(rec404.Body.String(), "run-not-found") {
		t.Errorf("404 body missing run-not-found code: %s", rec404.Body.String())
	}
}

func TestRunStepsNotFound(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run1", "mol-adopt-pr-v2", "open")),
	)
	_, err := s.humaHandleRunSteps(context.Background(), &RunStepsInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     "ghost",
	})
	if err == nil {
		t.Fatal("humaHandleRunSteps(ghost) = nil error, want run-not-found")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error = %q, want run-not-found detail", err.Error())
	}
}
