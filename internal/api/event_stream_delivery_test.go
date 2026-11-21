package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// storeGate admits a bounded number of bead-store reads and then blocks every
// later one until it is released. It stands in for a rig bead store whose cache
// misses fall through to a bd-backed backing store, where each probe is a
// subprocess.
//
// Blocking is a lifecycle signal rather than elapsed wall time: a test asserts
// that delivery completes while the stores are wedged, so it neither sleeps nor
// depends on how slow a real store happens to be.
type storeGate struct {
	release chan struct{}
	free    int

	mu   sync.Mutex
	gets int
}

func newStoreGate(t *testing.T, free int) *storeGate {
	t.Helper()
	g := &storeGate{release: make(chan struct{}), free: free}
	// Always release on the way out so a wedged Get cannot outlive the test.
	t.Cleanup(g.releaseAll)
	return g
}

func (g *storeGate) releaseAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// admit records one read and blocks once the free budget is spent.
func (g *storeGate) admit() {
	g.mu.Lock()
	g.gets++
	blocked := g.gets > g.free
	g.mu.Unlock()
	if blocked {
		<-g.release
	}
}

func (g *storeGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets
}

// gatedGetStore is a bead store whose Get is metered by a shared storeGate.
type gatedGetStore struct {
	beads.Store
	gate *storeGate
}

func (s *gatedGetStore) Get(id string) (beads.Bead, error) {
	s.gate.admit()
	return s.Store.Get(id)
}

// TestCityEventStreamDeliversEveryEventWhenBeadStoresAreWedged pins the
// delivery contract of GET /v0/city/{city}/events/stream: a subscriber receives
// every event above its cursor, in seq order, at the rate the city produces
// them — however slow the bead stores behind the optional workflow projection
// are.
//
// Regression: the stream enriched every bead.created/updated/closed event with
// a workflow projection computed inline, and resolving that projection scans
// every bead store. One slow store therefore stalled the whole stream
// (head-of-line blocking), and a subscriber received a few percent of the log
// no matter where its cursor sat.
//
// The stores here admit a small budget of reads and then wedge. A stream that
// projects every event exhausts that budget within the first handful of events
// and stops dead; a stream that stops projecting once it is behind delivers all
// of them without ever touching the stores again.
func TestCityEventStreamDeliversEveryEventWhenBeadStoresAreWedged(t *testing.T) {
	const (
		eventCount = 40
		freeReads  = 24
	)

	state := newFakeState(t)
	state.cityName = "wedged-city"
	gate := newStoreGate(t, freeReads)
	state.cityBeadStore = &gatedGetStore{Store: beads.NewMemStore(), gate: gate}
	state.stores = map[string]beads.Store{"alpha": &gatedGetStore{Store: beads.NewMemStore(), gate: gate}}

	recorder := newTestEventRecorder(t, state)
	// A city's steady-state log is dominated by bead events whose subject is not
	// a workflow bead: nothing in any store resolves them, so the projection
	// pays a full store scan and produces nothing.
	for i := range eventCount {
		recorder.Record(events.Event{
			Type:    events.BeadUpdated,
			Actor:   "cache-reconcile",
			Subject: fmt.Sprintf("gcg-%04d", i),
			Payload: wrappedBeadPayload(t, fmt.Sprintf("gcg-%04d", i)),
		})
	}

	delivered := streamCityEventSeqs(t, state, gate, eventCount)
	if len(delivered) != eventCount {
		t.Fatalf("delivered %d of %d events after %d bead-store reads; the stream must keep up with the log regardless of bead-store latency",
			len(delivered), eventCount, gate.count())
	}
	for i, seq := range delivered {
		if want := uint64(i + 1); seq != want {
			t.Fatalf("delivered[%d] seq = %d, want %d (gap or reorder in %v)", i, seq, want, delivered)
		}
	}
}

// TestCityEventStreamProjectsWorkflowEventsWhenKeepingUp is the counterweight
// to the delivery test above: skipping the workflow projection under backlog
// must not turn into skipping it always. A stream that is keeping up still
// enriches a workflow bead event with its projection.
func TestCityEventStreamProjectsWorkflowEventsWhenKeepingUp(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "wf-city"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.workflow_id":    "wf_stream",
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}

	recorder := newTestEventRecorder(t, state)
	recorder.Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "controller",
		Subject: child.ID,
		Payload: payload,
	})

	frame := streamFirstCityEventFrame(t, state)
	workflow, ok := frame["workflow"].(map[string]any)
	if !ok {
		t.Fatalf("event frame has no workflow projection: %v", frame)
	}
	if got := workflow["workflow_id"]; got != "wf_stream" {
		t.Fatalf("workflow.workflow_id = %v, want wf_stream", got)
	}
}

// TestProjectWorkflowEventWithSlackSkipsUnderBacklog pins the gate itself: with
// a backlog the projection is not computed, and — the property that matters —
// it does not read the bead stores at all.
func TestProjectWorkflowEventWithSlackSkipsUnderBacklog(t *testing.T) {
	state := newFakeState(t)
	gate := newStoreGate(t, 0)
	state.cityBeadStore = &gatedGetStore{Store: beads.NewMemStore(), gate: gate}
	state.stores = map[string]beads.Store{}
	event := events.Event{Type: events.BeadUpdated, Subject: "gcg-1", Seq: 1}

	if projection := projectWorkflowEventWithSlack(state, event, 1); projection != nil {
		t.Fatalf("projection = %+v, want nil under backlog", projection)
	}
	if reads := gate.count(); reads != 0 {
		t.Fatalf("backlogged projection made %d bead-store reads, want 0", reads)
	}
}

// newTestEventRecorder gives state a real file-backed event recorder.
func newTestEventRecorder(t *testing.T, state *fakeState) *events.FileRecorder {
	t.Helper()
	recorder, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	state.eventProv = recorder
	return recorder
}

// wrappedBeadPayload builds the {"bead": {...}} envelope bead.updated uses.
func wrappedBeadPayload(t *testing.T, id string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"bead": map[string]any{
		"id":     id,
		"title":  "session",
		"status": "open",
	}})
	if err != nil {
		t.Fatalf("Marshal(payload): %v", err)
	}
	return payload
}

// streamRecorder is an http.ResponseWriter that captures the bytes an SSE
// handler flushes, so a stream handler can be driven in-process rather than
// through a loopback listener. Flush doubles as the test's wakeup signal, which
// keeps frame collection free of polling and of elapsed-time waits.
type streamRecorder struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	header http.Header
	status int

	flushed chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{
		header:  make(http.Header),
		status:  http.StatusOK,
		flushed: make(chan struct{}, 1),
	}
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *streamRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func (r *streamRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *streamRecorder) code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// driveCityEventStream runs the city event SSE handler in-process against a
// streamRecorder until enough reports it has seen what it needs, then unwinds
// the handler and returns the captured body.
//
// Unwinding releases gate before waiting: a handler parked on a wedged bead
// store does not observe request cancellation (Store.Get takes no context), so
// waiting first would hang a failing test instead of failing it.
func driveCityEventStream(t *testing.T, state *fakeState, gate *storeGate, enough func(string) bool) string {
	t.Helper()

	mux := NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now()).
		WithAnyHostAllowed()
	handler := mux.Handler()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet,
		"/v0/city/"+state.cityName+"/events/stream?after_seq=0", nil).WithContext(ctx)
	rec := newStreamRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	unwind := func() string {
		cancel()
		if gate != nil {
			gate.releaseAll()
		}
		<-done
		return rec.body()
	}

	for !enough(rec.body()) {
		select {
		case <-rec.flushed:
		case <-ctx.Done():
			return unwind()
		}
	}
	body := unwind()

	if code := rec.code(); code != http.StatusOK {
		t.Fatalf("events/stream status = %d, want 200; body: %s", code, body)
	}
	return body
}

// streamCityEventSeqs collects up to want event seqs from the city stream.
func streamCityEventSeqs(t *testing.T, state *fakeState, gate *storeGate, want int) []uint64 {
	t.Helper()
	body := driveCityEventStream(t, state, gate, func(b string) bool {
		return len(parseStreamEventSeqs(b)) >= want
	})
	return parseStreamEventSeqs(body)
}

// streamFirstCityEventFrame returns the first decoded event frame. Heartbeat
// frames also carry a data line, so frames without a seq are skipped.
func streamFirstCityEventFrame(t *testing.T, state *fakeState) map[string]any {
	t.Helper()
	body := driveCityEventStream(t, state, nil, func(b string) bool {
		return firstStreamEventFrame(b) != nil
	})
	frame := firstStreamEventFrame(body)
	if frame == nil {
		t.Fatalf("stream produced no event frame; body: %s", body)
	}
	return frame
}

// parseStreamEventSeqs returns the `id:` line of every SSE event frame in body.
// Heartbeat frames carry no id and contribute nothing.
func parseStreamEventSeqs(body string) []uint64 {
	var seqs []uint64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
		if err != nil {
			continue
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

// firstStreamEventFrame decodes the first event `data:` line in body, skipping
// heartbeat frames (which carry no seq). It returns nil when none is complete.
func firstStreamEventFrame(body string) map[string]any {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			continue
		}
		if _, ok := frame["seq"]; !ok {
			continue
		}
		return frame
	}
	return nil
}
