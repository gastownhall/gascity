package dashboardbff

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runproj"
)

// sseFrame is one parsed SSE frame: its id (empty when the frame carried none),
// its event name, and the joined data payload. Comment/heartbeat lines (":"
// prefixed) are surfaced via the comment field so a test can assert a heartbeat
// fired without confusing it for a data frame.
type sseFrame struct {
	id      string
	event   string
	data    string
	comment string
}

// readSSEFrame reads one whole SSE frame (up to the blank-line terminator) from
// the scanner, returning false at EOF. It coalesces multi-line data per the SSE
// grammar and captures a leading comment line as a heartbeat marker.
func readSSEFrame(t *testing.T, sc *bufio.Scanner) (sseFrame, bool) {
	t.Helper()
	var f sseFrame
	var data []string
	sawField := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if sawField {
				f.data = strings.Join(data, "\n")
				return f, true
			}
			continue // leading blank line between frames
		}
		sawField = true
		switch {
		case strings.HasPrefix(line, ":"):
			f.comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id:"):
			f.id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "event:"):
			f.event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	return sseFrame{}, false
}

// startDetailStream opens the detail stream against the plane over a real
// httptest.Server (so the ResponseWriter is a flushing network writer, not the
// buffering httptest.ResponseRecorder) and returns the response plus a scanner
// positioned at the first frame. The caller closes the returned func to tear the
// connection down.
func startDetailStream(t *testing.T, srv *httptest.Server) (*http.Response, *bufio.Scanner, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/city/alpha/runs/run1/detail/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	return resp, sc, func() {
		cancel()
		_ = resp.Body.Close()
	}
}

// TestRunDetailStreamFirstFrame connects and asserts exactly one detail frame
// arrives immediately, its id equals the tailer's lastSeq, and its data decodes
// to the run's FormulaRunDetail.
func TestRunDetailStreamFirstFrame(t *testing.T) {
	dir := t.TempDir()
	writeEventLog(
		t, filepath.Join(dir, ".gc", "events.jsonl"),
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frame, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no first frame received")
	}
	if frame.event != "detail" {
		t.Errorf("event = %q, want detail", frame.event)
	}
	if frame.id != "2" {
		t.Errorf("frame id = %q, want 2 (lastSeq)", frame.id)
	}
	var detail runproj.FormulaRunDetail
	if err := json.Unmarshal([]byte(frame.data), &detail); err != nil {
		t.Fatalf("decode frame data: %v; data=%q", err, frame.data)
	}
	if detail.RunID != "run1" {
		t.Errorf("detail.RunID = %q, want run1", detail.RunID)
	}
	if len(detail.Nodes) != 2 {
		t.Errorf("detail nodes = %d, want 2 (root + preflight)", len(detail.Nodes))
	}
}

// TestRunDetailStreamFirstFrameReflectsBuildRacingConnect is the regression guard
// for the missed-frame race: a build() that publishes in the [precheck → connect]
// window bumps the fold generation but reaches no subscriber yet, so a first frame
// built from the PRECHECK value would pin the connection on a stale generation
// until the next build (which may never come for a run that goes idle). The fix
// re-reads detail() AFTER subscribe and sends THAT as the first frame. The
// runDetailStreamAfterPrecheck seam stages exactly one such racing fold, so the
// first frame must carry the NEWER lastSeq/bytes, not the precheck's.
func TestRunDetailStreamFirstFrameReflectsBuildRacingConnect(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent()) // seq 1 only at connect-precheck time
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	// Warm the tailer so the precheck is a true read (lastSeq==1), not a warming
	// wait, and grab the live tailer to fold the racing event into.
	_ = getRunSummary(t, p, "alpha")
	tl, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("tailer not found")
	}

	// Stage a build in the [precheck → connect] window: append a run-member event
	// and fold it directly, advancing lastSeq 1→2 with the subscriber not yet
	// registered. Before the fix the first frame carried the precheck's seq 1.
	var once sync.Once
	defer func(prev func()) { runDetailStreamAfterPrecheck = prev }(runDetailStreamAfterPrecheck)
	runDetailStreamAfterPrecheck = func() {
		once.Do(func() {
			appendEvents(t, logPath, runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"))
			proj := runproj.NewProjector()
			if err := proj.ColdLoad(logPath); err != nil {
				t.Errorf("cold load in seam: %v", err)
				return
			}
			tl.build(proj, nil, nil)
		})
	}

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()
	_ = resp

	frame, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no first frame")
	}
	if frame.id != "2" {
		t.Fatalf("first frame id = %q, want 2 — the build racing connect must be reflected in the FIRST frame (stale seq 1 = the missed-frame race)", frame.id)
	}
	var detail runproj.FormulaRunDetail
	if err := json.Unmarshal([]byte(frame.data), &detail); err != nil {
		t.Fatalf("decode first frame: %v", err)
	}
	if len(detail.Nodes) != 2 {
		t.Errorf("first frame nodes = %d, want 2 (root + the racing preflight step)", len(detail.Nodes))
	}
}

// TestRunDetailStreamPushesOnRunMemberEvent proves a new run-member bead event
// (lastSeq++) pushes exactly one NEW frame with a higher id.
func TestRunDetailStreamPushesOnRunMemberEvent(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent())
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()
	_ = resp

	first, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no first frame")
	}
	if first.id != "1" {
		t.Fatalf("first frame id = %q, want 1", first.id)
	}

	// A new run-member step lands on the log; the tailer folds it and must push.
	appendEvents(t, logPath, runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"))

	next, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no push frame after run-member event")
	}
	if next.id != "2" {
		t.Errorf("push frame id = %q, want 2 (higher than first)", next.id)
	}
	var detail runproj.FormulaRunDetail
	if err := json.Unmarshal([]byte(next.data), &detail); err != nil {
		t.Fatalf("decode push frame: %v", err)
	}
	if len(detail.Nodes) != 2 {
		t.Errorf("pushed detail nodes = %d, want 2 (root + preflight)", len(detail.Nodes))
	}
}

// TestRunDetailStreamByteDedupeUnrelatedRun is the correctness filter: an event
// for a DIFFERENT run bumps the fold's lastSeq but does not change THIS run's
// marshaled detail bytes, so no new frame is sent on this connection.
func TestRunDetailStreamByteDedupeUnrelatedRun(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent())
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()
	_ = resp

	first, ok := readSSEFrame(t, sc)
	if !ok || first.id != "1" {
		t.Fatalf("first frame = %+v, ok=%v; want id 1", first, ok)
	}

	// An unrelated run's molecule lands: it advances lastSeq (→ a subscriber
	// notify) but run1's detail bytes are unchanged, so THIS connection must send
	// nothing. Then a real run1 member lands and must break through — proving the
	// dedupe suppresses only the unrelated frame, not a genuine change.
	appendEvents(t, logPath, runMoleculeEvent(2, "otherRun", "mol-design-review-v2", "worker-x"))
	appendEvents(t, logPath, runDetailStepEvent(3, "run1.1", "run1", "preflight", "in_progress"))

	next, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no frame after the run1 member event")
	}
	// The dedupe must have SKIPPED the otherRun frame (id 2), so the next frame
	// the connection sees is the run1 change at id 3 — never id 2.
	if next.id != "3" {
		t.Errorf("next frame id = %q, want 3 (the otherRun event at seq 2 must be byte-deduped away)", next.id)
	}
}

// TestRunDetailStreamReconnectFreshSnapshot proves a reconnect gets one fresh
// current snapshot frame (frames are whole snapshots; no replay needed).
func TestRunDetailStreamReconnectFreshSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeEventLog(
		t, filepath.Join(dir, ".gc", "events.jsonl"),
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp1, sc1, close1 := startDetailStream(t, srv)
	f1, ok := readSSEFrame(t, sc1)
	if !ok || f1.id != "2" {
		t.Fatalf("first connection frame = %+v ok=%v, want id 2", f1, ok)
	}
	_ = resp1
	close1()

	// A brand-new connection must immediately get the current snapshot again.
	resp2, sc2, close2 := startDetailStream(t, srv)
	defer close2()
	_ = resp2
	f2, ok := readSSEFrame(t, sc2)
	if !ok {
		t.Fatal("reconnect received no snapshot frame")
	}
	if f2.event != "detail" || f2.id != "2" {
		t.Errorf("reconnect frame = %+v, want event detail id 2", f2)
	}
	if f2.data != f1.data {
		t.Errorf("reconnect snapshot bytes differ from the original snapshot")
	}
}

// TestRunDetailStreamDeregistersOnDisconnect proves the subscriber registry
// returns to empty after a client disconnects — the goroutine-leak guard.
func TestRunDetailStreamDeregistersOnDisconnect(t *testing.T) {
	dir := t.TempDir()
	writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"), runDetailRootEvent())
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	tl, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("tailer not found")
	}

	resp, sc, closeStream := startDetailStream(t, srv)
	if _, ok := readSSEFrame(t, sc); !ok {
		t.Fatal("no first frame")
	}
	_ = resp

	// Subscriber registered while the connection is live.
	if got := tl.subscriberCount(); got != 1 {
		t.Fatalf("subscriber count = %d while connected, want 1", got)
	}

	closeStream()

	// The handler goroutine must observe the disconnect and deregister.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tl.subscriberCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber count = %d after disconnect, want 0 (leak)", tl.subscriberCount())
}

// TestRunDetailStreamUnknownCity404 confirms an unresolvable city 404s before
// any stream body.
func TestRunDetailStreamUnknownCity404(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/runs/run1/detail/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown city", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must not be an event-stream on a precheck 404", ct)
	}
}

// TestRunDetailStreamUnsupportedRun422 confirms a v1/wisp run is rejected 422
// with the not_run_view reason BEFORE the stream body, mirroring the GET.
func TestRunDetailStreamUnsupportedRun422(t *testing.T) {
	dir := t.TempDir()
	// A molecule run marker but NO gc.formula_contract=graph.v2 → not a run view.
	writeEventLog(
		t, filepath.Join(dir, ".gc", "events.jsonl"),
		beadCreatedEvent(1, beads.Bead{
			ID:        "v1run",
			Title:     "legacy v1 run",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			Metadata:  map[string]string{"gc.kind": "run"},
		}),
	)
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/v1run/detail/stream", nil))
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

// A missing run once the tailer is warm answers 503 for the unknown-run grace
// window and 404 after it expires, before any stream body — covered by
// TestRunDetailStreamUnknownRunWarmingGrace in rundetail_grace_test.go.

// TestRunDetailStreamHeartbeat proves a heartbeat comment frame is emitted after
// the (shortened) heartbeat interval when no data change fires.
func TestRunDetailStreamHeartbeat(t *testing.T) {
	defer func(prev time.Duration) { runDetailStreamHeartbeat = prev }(runDetailStreamHeartbeat)
	runDetailStreamHeartbeat = 25 * time.Millisecond

	dir := t.TempDir()
	writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"), runDetailRootEvent())
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()
	_ = resp

	if _, ok := readSSEFrame(t, sc); !ok {
		t.Fatal("no first frame")
	}
	// With no data change, the next frame must be the heartbeat comment.
	hb, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no heartbeat frame")
	}
	if hb.comment == "" {
		t.Errorf("expected a heartbeat comment frame, got %+v", hb)
	}
}
