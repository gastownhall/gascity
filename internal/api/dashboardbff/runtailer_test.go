package dashboardbff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type fakeResolver struct{ paths map[string]string }

func (f fakeResolver) CityPath(name string) (string, bool) {
	p, ok := f.paths[name]
	return p, ok
}

// runMoleculeEvent builds a bead.created event for a run-molecule lane carrying
// the markers isRunGroup recognizes plus an active assignee for session joins.
func runMoleculeEvent(seq uint64, id, formula, assignee string) events.Event {
	b := beads.Bead{
		ID:        id,
		Title:     formula,
		Status:    "open",
		Type:      "molecule",
		Assignee:  assignee,
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
		},
	}
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}

func writeEventLog(t *testing.T, path string, evts ...events.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// appendEvents appends events to an existing log via a plain O_APPEND handle —
// the supervisor's own write path. That it succeeds while the tailer is running
// proves the tailer is a pure reader (never a second writer holding the file).
func appendEvents(t *testing.T, path string, evts ...events.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer f.Close() //nolint:errcheck
	for _, e := range evts {
		line, _ := json.Marshal(e)
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func waitForLanes(t *testing.T, tl *cityRunTailer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tl.mu.RLock()
		n := len(tl.summary.Lanes)
		tl.mu.RUnlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tl.mu.RLock()
	n := len(tl.summary.Lanes)
	tl.mu.RUnlock()
	t.Fatalf("lane count = %d, want %d within deadline", n, want)
}

// TestRunTailerColdLoadAndLiveTail proves the tailer cold-replays the existing
// log, then picks up newly appended events on its byte-offset tail — and that an
// external writer can still append while the tail runs (no second-writer lock).
func TestRunTailerColdLoadAndLiveTail(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	m := newRunTailerManager(Deps{})
	m.enable(ctx, &wg)
	tl := m.ensure("alpha", logPath)

	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	waitForLanes(t, tl, 1)

	// Append a second run via the supervisor's own append path while the tail runs.
	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	waitForLanes(t, tl, 2)

	cancel()
	wg.Wait()
}

// TestRunSummaryEndpointEnrichesFromSessions drives the full endpoint: the warm
// fold plus request-time session enrich resolves a lane's session to available
// health and an available census.
func TestRunSummaryEndpointEnrichesFromSessions(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	sessions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/sessions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"s1","template":"t","session_name":"alpha__worker-1","title":"W","alias":"worker-1","state":"active","created_at":"2026-06-01T10:00:00Z","last_active":"2026-06-01T11:00:00Z","attached":false,"running":true,"activity":"thinking","provider":"claude"}],"total":1}`))
	}))
	defer sessions.Close()

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: sessions.URL,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	resp := getRunSummary(t, p, "alpha")
	if resp.TotalActive != 1 || len(resp.Lanes) != 1 {
		t.Fatalf("totalActive=%d lanes=%d, want 1/1", resp.TotalActive, len(resp.Lanes))
	}
	lane := resp.Lanes[0]
	if lane.Health.Status != "available" {
		t.Errorf("lane health = %q, want available", lane.Health.Status)
	}
	if lane.Health.Data.Session.Status != "resolved" {
		t.Errorf("session status = %q, want resolved", lane.Health.Data.Session.Status)
	}
	if resp.Census.Status != "available" {
		t.Errorf("census status = %q, want available", resp.Census.Status)
	}
}

// TestRunSummaryEndpointDegradesWithoutSessions proves a sessions outage degrades
// lane health to unavailable (counted unverifiable in the census) rather than
// failing the load.
func TestRunSummaryEndpointDegradesWithoutSessions(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	// No SupervisorBaseURL: the sessions read is unavailable.
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	resp := getRunSummary(t, p, "alpha")
	if len(resp.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(resp.Lanes))
	}
	if resp.Lanes[0].Health.Status != "unavailable" {
		t.Errorf("lane health = %q, want unavailable on sessions outage", resp.Lanes[0].Health.Status)
	}
	if resp.Census.Status != "available" {
		t.Errorf("census status = %q, want available", resp.Census.Status)
	}
	if resp.Census.Data.TotalInFlight < 1 || resp.Census.Data.Unverifiable < 1 {
		t.Errorf("census = %+v, want >=1 in-flight and >=1 unverifiable", resp.Census.Data)
	}
}

// TestRunSummaryEndpointUnknownCity404s confirms an unresolvable city 404s.
func TestRunSummaryEndpointUnknownCity404s(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/runs/summary", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown city", rec.Code)
	}
}

// runSummaryWire is the decoded endpoint body — a structural contract check that
// the wire carries the enriched RunSummary shape the SPA renderer reads.
type runSummaryWire struct {
	TotalActive int `json:"totalActive"`
	Lanes       []struct {
		ID     string `json:"id"`
		Health struct {
			Status string `json:"status"`
			Data   struct {
				PhaseConfidence string `json:"phaseConfidence"`
				Session         struct {
					Status string `json:"status"`
				} `json:"session"`
			} `json:"data"`
		} `json:"health"`
	} `json:"lanes"`
	Census struct {
		Status string `json:"status"`
		Data   struct {
			TotalInFlight int `json:"totalInFlight"`
			Unverifiable  int `json:"unverifiable"`
		} `json:"data"`
	} `json:"census"`
}

func getRunSummary(t *testing.T, p *Plane, city string) runSummaryWire {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/"+city+"/runs/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp runSummaryWire
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}
