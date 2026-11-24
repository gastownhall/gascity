package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
)

// pollingRunProjection stands in for the dashboard plane's per-city tailer: it
// serves the snapshot it last folded from the event log and folds again only
// when asked (its poll, or SyncRunProjection), so a read between a write and
// the next fold is stale exactly as production's is.
type pollingRunProjection struct {
	t       *testing.T
	logPath string

	mu        sync.Mutex
	published runproj.RunProjectionSnapshot
	syncCalls int
}

func newPollingRunProjection(t *testing.T, logPath string) *pollingRunProjection {
	p := &pollingRunProjection{t: t, logPath: logPath}
	p.fold()
	return p
}

func (p *pollingRunProjection) fold() {
	proj := runproj.NewProjector()
	if err := proj.ColdLoad(p.logPath); err != nil {
		p.t.Errorf("fold %s: %v", p.logPath, err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = runproj.RunProjectionSnapshot{Ready: true, Beads: runproj.FilterRunBeads(proj.Beads())}
}

func (p *pollingRunProjection) RunCensus(context.Context, string) (runproj.CanonicalRunCensus, bool) {
	return runproj.CanonicalRunCensus{Ready: true}, true
}

func (p *pollingRunProjection) RunProjection(context.Context, string) (runproj.RunProjectionSnapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.published, true
}

func (p *pollingRunProjection) RunProjectionMissInGrace(context.Context, string, string) bool {
	return false
}

func (p *pollingRunProjection) ForgetRunProjectionMiss(context.Context, string, string) {}

func (p *pollingRunProjection) SyncRunProjection(context.Context, string) bool {
	p.mu.Lock()
	p.syncCalls++
	p.mu.Unlock()
	p.fold()
	return true
}

func (p *pollingRunProjection) syncs() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.syncCalls
}

// TestRunReadAfterCancelReflectsTheCancel reproduces the live read-your-writes
// miss: POST /runs/{id}/cancel answered 202 {status: canceled}, and the GET
// /runs/{id} that followed still read "active" for ~3 s. The cancel's
// bead.closed events were already in the event log when it answered; the warm
// projection simply had not folded them yet. The cancel now syncs the
// projection before it responds.
func TestRunReadAfterCancelReflectsTheCancel(t *testing.T) {
	c := newOutOfProcessClaimCity(t)
	source := newPollingRunProjection(t, filepath.Join(c.state.cityPath, ".gc", "events.jsonl"))
	c.h = wrapTestSupervisorMiddleware(
		NewSupervisorMux(&stateCityResolver{state: c.state}, nil, false, "test", "", time.Now()).
			WithRunCensusSource(source))

	if v := c.view(t); v.runStatus != string(RunStatusPending) {
		t.Fatalf("before cancel: %+v, want a pending run", v)
	}

	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, newPostRequest(cityURL(c.state, "/runs/"+c.rootID+"/cancel"), nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST cancel = %d: %s", rec.Code, rec.Body.String())
	}

	v := c.view(t)
	if v.runStatus != string(RunStatusCanceled) {
		t.Errorf("GET /runs/%s right after a 202 cancel = %q, want canceled", c.rootID, v.runStatus)
	}
	if v.stepStatus != string(RunStepStatusCanceled) {
		t.Errorf("GET /runs/%s/steps step right after a 202 cancel = %q, want canceled", c.rootID, v.stepStatus)
	}
	if got := source.syncs(); got != 1 {
		t.Errorf("projection syncs = %d, want exactly one, after the successful cancel", got)
	}

	// A refused cancel wrote nothing, so it does not sync.
	again := httptest.NewRecorder()
	c.h.ServeHTTP(again, newPostRequest(cityURL(c.state, "/runs/"+c.rootID+"/cancel"), nil))
	if again.Code != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409: %s", again.Code, again.Body.String())
	}
	if got := source.syncs(); got != 1 {
		t.Errorf("projection syncs after a refused cancel = %d, want still 1", got)
	}
}
