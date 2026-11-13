package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// outOfProcessClaimCity is a city whose run store is a supervisor CachingStore
// recording its bead.* notifications into the city's own event log — the
// production wiring (cmd/gc wrapWithCachingStore + cacheChangeRecorder) that
// feeds the event-sourced run projection behind GET /runs/{id} and
// /runs/{id}/steps — over a MemStore that a second "process" writes directly.
type outOfProcessClaimCity struct {
	state   *fakeState
	backing *beads.MemStore
	cache   *beads.CachingStore
	clock   *atomic.Int64
	h       http.Handler
	rootID  string
	stepID  string
}

func newOutOfProcessClaimCity(t *testing.T) *outOfProcessClaimCity {
	t.Helper()
	state := newFakeState(t)
	rec, err := events.NewFileRecorder(filepath.Join(state.cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).UnixNano())
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		if err := rec.RecordAck(events.Event{Type: eventType, Actor: "cache", Subject: beadID, Payload: payload}); err != nil {
			t.Errorf("record %s %s: %v", eventType, beadID, err)
		}
	}, beads.WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache

	root, err := cache.Create(beads.Bead{
		Title: "run root",
		Type:  "molecule",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.formula":          "run-cancel-probe",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := cache.Create(beads.Bead{
		Title:    "run one foreground shell command",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	return &outOfProcessClaimCity{
		state: state, backing: backing, cache: cache, clock: clock,
		h: newTestCityHandler(t, state), rootID: root.ID, stepID: step.ID,
	}
}

func (c *outOfProcessClaimCity) get(t *testing.T, path string, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(c.state, path), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode GET %s: %v; raw=%s", path, err, rec.Body.String())
	}
}

type outOfProcessClaimView struct {
	beadStatus, beadAssignee string
	runStatus                string
	stepStatus, stepAssignee string
}

func (c *outOfProcessClaimCity) view(t *testing.T) outOfProcessClaimView {
	t.Helper()
	var bead struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
	}
	c.get(t, "/bead/"+c.stepID, &bead)
	var run struct {
		Status string `json:"status"`
	}
	c.get(t, "/runs/"+c.rootID, &run)
	var steps struct {
		Steps []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Assignee string `json:"assignee"`
		} `json:"steps"`
	}
	c.get(t, "/runs/"+c.rootID+"/steps", &steps)
	v := outOfProcessClaimView{beadStatus: bead.Status, beadAssignee: bead.Assignee, runStatus: run.Status}
	for _, s := range steps.Steps {
		if s.ID == c.stepID {
			v.stepStatus, v.stepAssignee = s.Status, s.Assignee
		}
	}
	return v
}

// TestRunViewsAgreeWithBeadAfterOutOfProcessClaim reproduces the live
// disagreement: a stock worker claimed a run's step with `gc hook --claim`, a
// one-shot process writing bd directly. GET /bead/{step} showed it in_progress
// with the assignee, while GET /runs/{id} said pending and /runs/{id}/steps
// showed the step pending with no assignee for the whole execution, then jumped
// straight to completed.
//
// The out-of-process write announces nothing. A supervisor read of the run's
// children (here GET /bead/{root}/deps; in production the workflow and session
// readers do the same live list continuously) refetched the claimed row into
// the cache, so GET /bead served it — but that read announced nothing either,
// and the reconcile scan that followed found the cache already agreeing with
// bd and announced nothing. The run projection folds only bead.* events, so it
// never saw the claim.
func TestRunViewsAgreeWithBeadAfterOutOfProcessClaim(t *testing.T) {
	c := newOutOfProcessClaimCity(t)
	if v := c.view(t); v.runStatus != string(RunStatusPending) || v.stepStatus != string(RunStepStatusPending) {
		t.Fatalf("before the claim: %+v, want a pending run and step", v)
	}

	// Past the recent-local-write window, then the claim lands in bd from
	// another process.
	c.clock.Add(int64(time.Minute))
	status, assignee := "in_progress", "rc-codex"
	if err := c.backing.Update(c.stepID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("out-of-process claim: %v", err)
	}
	// A supervisor read of the run's children refetches the claimed row.
	c.get(t, "/bead/"+c.rootID+"/deps", nil)

	v := c.view(t)
	if v.beadStatus != "in_progress" || v.beadAssignee != assignee {
		t.Fatalf("GET /bead/%s = status %q assignee %q, want the claim", c.stepID, v.beadStatus, v.beadAssignee)
	}
	if v.stepStatus != string(RunStepStatusActive) || v.stepAssignee != assignee {
		t.Errorf("GET /runs/%s/steps step = status %q assignee %q, want active %q like GET /bead",
			c.rootID, v.stepStatus, v.stepAssignee, assignee)
	}
	if v.runStatus != string(RunStatusActive) {
		t.Errorf("GET /runs/%s status = %q, want active once a step is claimed", c.rootID, v.runStatus)
	}

	// The reconcile that follows neither loses nor reverts the claim.
	c.cache.ReconcileNowForTest()
	if after := c.view(t); after != v {
		t.Errorf("after reconcile: %+v, want unchanged %+v", after, v)
	}
}
