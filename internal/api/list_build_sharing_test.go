package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// gatedActiveWorkStore serves each in_progress read from the underlying
// store but holds the rows it read until that read's gate opens, closing the
// read's arrived channel once the rows are in hand. A build blocked here
// holds its snapshot while the test mutates the state behind it, so the
// build that runs next reads the mutated state.
type gatedActiveWorkStore struct {
	beads.Store
	gates   []chan struct{}
	arrived []chan struct{}
	mu      sync.Mutex
	reads   int
}

func (g *gatedActiveWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "in_progress" {
		g.mu.Lock()
		i := g.reads
		g.reads++
		g.mu.Unlock()
		rows, err := g.Store.List(query)
		if i < len(g.arrived) {
			close(g.arrived[i])
		}
		if i < len(g.gates) {
			<-g.gates[i]
		}
		return rows, err
	}
	return g.Store.List(query)
}

// awaitSlotState fails t unless cond, read under responseCacheMu, holds
// before a deadline that bounds a broken test, not the code's timing. The
// wait spins on Gosched rather than sleeping, so it carries no timing
// assumption about when a joining reader is admitted.
func awaitSlotState(t *testing.T, srv *Server, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.responseCacheMu.Lock()
		ok := cond()
		srv.responseCacheMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		runtime.Gosched()
	}
}

// TestListBuildSharingQueuedFresh pins the shared list build on GET /agents:
// a cache-miss reader arriving while a build runs joins the one build queued
// behind it; the queued build runs when the running one ends and reads the
// state when it starts, so no reader is answered by a build that read the
// state before its admission; a reader arriving while the queued build runs
// queues the build after it; and the slot is removed once the queue drains.
func TestListBuildSharingQueuedFresh(t *testing.T) {
	st := newFakeState(t)
	st.cfg.NamedSessions = nil
	st.cfg.Agents = []config.Agent{
		{Name: "worker1", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
	}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	gated := &gatedActiveWorkStore{Store: st.stores["myrig"], gates: gates, arrived: arrived}
	st.stores["myrig"] = gated
	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)

	// The response cache is off: the first build's stored body — long in
	// building, so served across index changes — would otherwise answer a
	// later reader before the shared build is consulted. Every reader here
	// is a cache miss, which is the sharing path's precondition.
	oldTTL, oldFloor := responseCacheTTL, agentListResponseTTLFloor
	responseCacheTTL, agentListResponseTTLFloor = 0, 0
	t.Cleanup(func() {
		responseCacheTTL, agentListResponseTTLFloor = oldTTL, oldFloor
	})

	// Each gate closes once: the test releases builds in order, and the
	// cleanup releases any the test never reached.
	releases := make([]func(), len(gates))
	for i := range gates {
		g := gates[i]
		releases[i] = sync.OnceFunc(func() { close(g) })
	}
	t.Cleanup(func() {
		for _, r := range releases {
			r()
		}
	})

	type agentCall struct {
		id   int
		code int
		body agentListWire
	}
	activeBeadOf := func(call agentCall, name string) string {
		for _, item := range call.body.Items {
			if item.Name == name {
				return item.ActiveBead
			}
		}
		return "<not listed>"
	}
	calls := make(chan agentCall, 3)
	fetch := func(id int) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agents"), nil))
		var body agentListWire
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("decode GET /agents: %v", err)
			calls <- agentCall{id: id, code: rec.Code}
			return
		}
		calls <- agentCall{id: id, code: rec.Code, body: body}
	}
	queuedBehindRunning := func() bool {
		for _, slot := range srv.listBuilds {
			if slot.running != nil && slot.queued != nil {
				return true
			}
		}
		return false
	}
	anyBuildRunning := func() bool {
		for _, slot := range srv.listBuilds {
			if slot.running != nil {
				return true
			}
		}
		return false
	}

	// The first reader's build reads the store and holds its rows. Wait for
	// its admission — its build running — before launching the next reader,
	// so the admission order matches the launch order whatever the scheduler
	// does with the goroutines.
	go fetch(1)
	awaitSlotState(t, srv, "the first build to start", anyBuildRunning)
	awaitSignal(t, arrived[0], "the first build's in_progress read")

	// The second reader arrives while the first build runs and joins the
	// build queued behind it.
	go fetch(2)
	awaitSlotState(t, srv, "a queued build to form behind the running one", queuedBehindRunning)

	// While the first build still holds its pre-mutation rows, work appears:
	// the queued build must read it when it starts, not the running build's
	// stale snapshot.
	first := mustCreateInProgress(t, gated.Store, "first work", "myrig/worker1")
	releases[0]()
	awaitSignal(t, arrived[1], "the queued build's in_progress read")

	// The third reader arrives while the queued build runs and queues the
	// build after it; more work appears, which only that build may read.
	go fetch(3)
	awaitSlotState(t, srv, "a second queued build to form behind the queued one", queuedBehindRunning)
	second := mustCreateInProgress(t, gated.Store, "second work", "myrig/worker1")
	releases[1]()
	awaitSignal(t, arrived[2], "the second queued build's in_progress read")
	releases[2]()

	// Pair each response with its reader by id, not by completion order: the
	// goroutines' sends can invert under scheduler delays, and which build a
	// reader waits on is fixed by its admission, not by when its response
	// lands.
	byReader := make(map[int]agentCall, 3)
	for range []int{1, 2, 3} {
		call := <-calls
		byReader[call.id] = call
	}
	if gated.reads != 3 {
		t.Errorf("GET /agents served 3 cache-miss readers with %d builds, want 3: one running build and one queued build per running build", gated.reads)
	}
	for id, call := range byReader {
		if call.code != http.StatusOK {
			t.Errorf("reader %d status = %d, want 200", id, call.code)
		}
	}
	if got := activeBeadOf(byReader[1], "myrig/worker1"); got != "" {
		t.Errorf("the first reader's build saw active_bead %q, want none: its build's rows were read before any work was created", got)
	}
	if got := activeBeadOf(byReader[2], "myrig/worker1"); got != first.ID {
		t.Errorf("the queued build's reader saw active_bead %q, want %s: the build a joiner waits on starts after its admission and reads the state then", got, first.ID)
	}
	if got := activeBeadOf(byReader[3], "myrig/worker1"); got != second.ID {
		t.Errorf("the second queued build's reader saw active_bead %q, want %s, the later work: a reader arriving while the queued build runs queues the build after it, which reads the state when it starts", got, second.ID)
	}
	srv.responseCacheMu.Lock()
	left := len(srv.listBuilds)
	srv.responseCacheMu.Unlock()
	if left != 0 {
		t.Errorf("%d list build slots left after the queue drained, want 0", left)
	}
}

// TestListBuildSharingPostMutation pins what a reader admitted while a
// build runs may expect from the build that serves it: the build queued
// behind the running one starts after that reader's admission, so it reads
// the state then — a mutation the running build never saw is in its body —
// and its X-GC-Index covers the mutation's event, while the reader admitted
// before the mutation keeps the pre-mutation body and index its own build
// read.
func TestListBuildSharingPostMutation(t *testing.T) {
	st := newFakeState(t)
	st.cfg.NamedSessions = nil
	st.cfg.Agents = []config.Agent{
		{Name: "worker1", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
	}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{})}
	gated := &gatedActiveWorkStore{Store: st.stores["myrig"], gates: gates, arrived: arrived}
	st.stores["myrig"] = gated
	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)

	// The response cache is off: every reader here is a cache miss, which is
	// the sharing path's precondition.
	oldTTL, oldFloor := responseCacheTTL, agentListResponseTTLFloor
	responseCacheTTL, agentListResponseTTLFloor = 0, 0
	t.Cleanup(func() {
		responseCacheTTL, agentListResponseTTLFloor = oldTTL, oldFloor
	})

	releases := make([]func(), len(gates))
	for i := range gates {
		g := gates[i]
		releases[i] = sync.OnceFunc(func() { close(g) })
	}
	t.Cleanup(func() {
		for _, r := range releases {
			r()
		}
	})

	type agentCall struct {
		id    int
		code  int
		index string
		body  agentListWire
	}
	calls := make(chan agentCall, 2)
	fetch := func(id int) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agents"), nil))
		var body agentListWire
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("decode GET /agents: %v", err)
			calls <- agentCall{id: id, code: rec.Code}
			return
		}
		calls <- agentCall{id: id, code: rec.Code, index: rec.Header().Get("X-GC-Index"), body: body}
	}
	activeBeadOf := func(call agentCall, name string) string {
		for _, item := range call.body.Items {
			if item.Name == name {
				return item.ActiveBead
			}
		}
		return "<not listed>"
	}
	anyBuildRunning := func() bool {
		for _, slot := range srv.listBuilds {
			if slot.running != nil {
				return true
			}
		}
		return false
	}
	queuedBehindRunning := func() bool {
		for _, slot := range srv.listBuilds {
			if slot.running != nil && slot.queued != nil {
				return true
			}
		}
		return false
	}

	// The first reader's build reads the store and holds its rows.
	go fetch(1)
	awaitSlotState(t, srv, "the first build to start", anyBuildRunning)
	awaitSignal(t, arrived[0], "the first build's in_progress read")

	// Work appears and the event log advances while the first build still
	// holds its pre-mutation rows.
	first := mustCreateInProgress(t, gated.Store, "first work", "myrig/worker1")
	st.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "test", Subject: "post-mutation"})
	mutatedSeq, err := st.eventProv.LatestSeq()
	if err != nil {
		t.Fatalf("read the event index after the mutation: %v", err)
	}

	// A second reader arrives after the mutation and joins the build queued
	// behind the running one; that build starts only once the running one
	// ends, so it reads the state after the reader's admission.
	go fetch(2)
	awaitSlotState(t, srv, "a queued build to form behind the running one", queuedBehindRunning)
	releases[0]()
	awaitSignal(t, arrived[1], "the queued build's in_progress read")
	releases[1]()

	// Pair each response with its reader by id, not by completion order (the
	// sends can invert under scheduler delays).
	byReader := make(map[int]agentCall, 2)
	for range []int{1, 2} {
		call := <-calls
		byReader[call.id] = call
	}
	for id, call := range byReader {
		if call.code != http.StatusOK {
			t.Errorf("reader %d status = %d, want 200", id, call.code)
		}
	}
	if got := activeBeadOf(byReader[1], "myrig/worker1"); got != "" {
		t.Errorf("the pre-mutation reader's build saw active_bead %q, want none: its build read the state before the work was created", got)
	}
	if got := activeBeadOf(byReader[2], "myrig/worker1"); got != first.ID {
		t.Errorf("the post-mutation reader's build saw active_bead %q, want %s: the build that serves it starts after its admission and reads the state then", got, first.ID)
	}
	servedIndex, err := strconv.ParseUint(byReader[2].index, 10, 64)
	if err != nil {
		t.Fatalf("read the serving build's X-GC-Index %q: %v", byReader[2].index, err)
	}
	if servedIndex < mutatedSeq {
		t.Errorf("the serving build's X-GC-Index is %d, want >= %d: the build that answers a reader admitted after a mutation starts after that admission, so the index it captures covers the mutation's event", servedIndex, mutatedSeq)
	}
	if gated.reads != 2 {
		t.Errorf("GET /agents served 2 cache-miss readers with %d builds, want 2: one running build and the one queued behind it", gated.reads)
	}
}

// TestListBuildSharingIsolation pins what the shared list build does not
// couple: a reader on another cache key — and an inline read (?peek=true),
// which takes no cache key at all — builds on its own and completes while a
// shared build for the plain list is still held, leaving that build's slot
// untouched; and at the helper level, two waiters joined onto one build each
// receive their own deep copy, so what one waiter does with its response
// cannot reach the other.
func TestListBuildSharingIsolation(t *testing.T) {
	st := newFakeState(t)
	st.cfg.NamedSessions = nil
	st.cfg.Agents = []config.Agent{
		{Name: "worker1", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
	}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	gated := &gatedActiveWorkStore{Store: st.stores["myrig"], gates: gates, arrived: arrived}
	st.stores["myrig"] = gated
	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)

	// The response cache is off: every shared-key reader here is a cache
	// miss, which is the sharing path's precondition.
	oldTTL, oldFloor := responseCacheTTL, agentListResponseTTLFloor
	responseCacheTTL, agentListResponseTTLFloor = 0, 0
	t.Cleanup(func() {
		responseCacheTTL, agentListResponseTTLFloor = oldTTL, oldFloor
	})

	releases := make([]func(), len(gates))
	for i := range gates {
		g := gates[i]
		releases[i] = sync.OnceFunc(func() { close(g) })
	}
	t.Cleanup(func() {
		for _, r := range releases {
			r()
		}
	})

	type agentCall struct {
		id   int
		code int
		body agentListWire
	}
	calls := make(chan agentCall, 3)
	fetch := func(query string, id int) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agents"+query), nil))
		var body agentListWire
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("decode GET /agents%s: %v", query, err)
			calls <- agentCall{id: id, code: rec.Code}
			return
		}
		calls <- agentCall{id: id, code: rec.Code, body: body}
	}
	activeBeadOf := func(call agentCall, name string) string {
		for _, item := range call.body.Items {
			if item.Name == name {
				return item.ActiveBead
			}
		}
		return "<not listed>"
	}
	anyBuildRunning := func() bool {
		for _, slot := range srv.listBuilds {
			if slot.running != nil {
				return true
			}
		}
		return false
	}
	// oneSlotHoldsBuild holds while exactly one slot remains — the held
	// build's own — with nothing queued behind it: the inline and
	// other-key builds left no slot of theirs behind.
	oneSlotHoldsBuild := func() bool {
		n := 0
		queued := false
		for _, slot := range srv.listBuilds {
			n++
			if slot.running == nil || slot.queued != nil {
				queued = true
			}
		}
		return n == 1 && !queued
	}

	// The plain list's shared build reads the store and holds its rows.
	go fetch("", 1)
	awaitSlotState(t, srv, "the first build to start", anyBuildRunning)
	awaitSignal(t, arrived[0], "the first build's in_progress read")

	// A ?peek=true reader builds inline — no cache key, no shared build —
	// so it completes while the shared build is still held and leaves no
	// slot behind.
	go fetch("?peek=true", 2)
	awaitSignal(t, arrived[1], "the peek read's in_progress read")
	releases[1]()
	r2 := <-calls
	if r2.id != 2 {
		t.Fatalf("the peek response is reader %d's, want 2: responses are paired by id, not arrival order", r2.id)
	}
	if r2.code != http.StatusOK {
		t.Errorf("the peek reader status = %d, want 200", r2.code)
	}
	if got := activeBeadOf(r2, "myrig/worker1"); got != "" {
		t.Errorf("the peek reader saw active_bead %q, want none: no work was created", got)
	}
	awaitSlotState(t, srv, "only the held build's slot to remain after the peek build", oneSlotHoldsBuild)

	// A reader on a different key (?running=false changes the input, hence
	// the cache key) builds on its own the same way.
	go fetch("?running=false", 3)
	awaitSignal(t, arrived[2], "the other-key read's in_progress read")
	releases[2]()
	r3 := <-calls
	if r3.id != 3 {
		t.Fatalf("the other-key response is reader %d's, want 3: responses are paired by id, not arrival order", r3.id)
	}
	if r3.code != http.StatusOK {
		t.Errorf("the other-key reader status = %d, want 200", r3.code)
	}
	if got := activeBeadOf(r3, "myrig/worker1"); got != "" {
		t.Errorf("the other-key reader saw active_bead %q, want none: no work was created", got)
	}
	awaitSlotState(t, srv, "only the held build's slot to remain after the other-key build", oneSlotHoldsBuild)

	// Release the held build: it serves its own reader, and the slot drains.
	releases[0]()
	r1 := <-calls
	if r1.id != 1 {
		t.Fatalf("the held build's response is reader %d's, want 1: responses are paired by id, not arrival order", r1.id)
	}
	if r1.code != http.StatusOK {
		t.Errorf("the held build's reader status = %d, want 200", r1.code)
	}
	if got := activeBeadOf(r1, "myrig/worker1"); got != "" {
		t.Errorf("the held build's reader saw active_bead %q, want none: no work was created", got)
	}
	if gated.reads != 3 {
		t.Errorf("GET /agents ran %d builds for the three readers, want 3: the shared key stacked one build, the peek and other-key readers built their own", gated.reads)
	}
	awaitSlotState(t, srv, "the slot map to drain once the held build ends", func() bool {
		return len(srv.listBuilds) == 0
	})

	// Helper level: two waiters joined onto one build each receive their own
	// deep copy. The first caller starts the build; the second queues the
	// build behind it; the third joins that queued build — two waiters, one
	// outcome.
	type probeList struct {
		IDs []string
	}
	probeKey := "isolation-probe"
	probeGate := make(chan struct{})
	probeBuild := func(ctx context.Context) (probeList, error) {
		select {
		case <-probeGate:
			return probeList{IDs: []string{"a", "b"}}, nil
		case <-ctx.Done():
			return probeList{}, ctx.Err()
		}
	}
	type probeResult struct {
		who string
		v   probeList
		err error
	}
	probeResults := make(chan probeResult, 3)
	probe := func(who string) {
		v, err := shareListBuild(context.Background(), srv, probeKey, probeBuild)
		probeResults <- probeResult{who: who, v: v, err: err}
	}
	go probe("starter")
	awaitSlotState(t, srv, "the probe build to start", func() bool {
		slot := srv.listBuilds[probeKey]
		return slot != nil && slot.running != nil
	})
	go probe("queued")
	awaitSlotState(t, srv, "the probe build to queue behind the running one", func() bool {
		slot := srv.listBuilds[probeKey]
		return slot != nil && slot.running != nil && slot.queued != nil
	})
	go probe("joiner")
	close(probeGate)
	byWho := make(map[string]probeResult, 3)
	for range []string{"starter", "queued", "joiner"} {
		r := <-probeResults
		if r.err != nil {
			t.Errorf("the %s waiter errored: %v", r.who, r.err)
			continue
		}
		byWho[r.who] = r
	}
	for _, who := range []string{"starter", "queued", "joiner"} {
		r, ok := byWho[who]
		if !ok {
			t.Fatalf("the %s waiter's result is missing", who)
		}
		if len(r.v.IDs) != 2 || r.v.IDs[0] != "a" || r.v.IDs[1] != "b" {
			t.Errorf("the %s waiter's copy is %v, want [a b]", who, r.v.IDs)
		}
	}
	// The queued waiter and the joiner wait on the same build's outcome, so
	// the joiner's copy must be its own: appending to it cannot reach the
	// queued waiter's.
	joiner := byWho["joiner"]
	joiner.v.IDs = append(joiner.v.IDs, "mutated")
	if got := byWho["queued"].v.IDs; len(got) != 2 {
		t.Errorf("the queued waiter's copy is %v after the joiner appended to its own, want [a b]: every waiter gets a deep copy of the build's outcome", got)
	}
	awaitSlotState(t, srv, "the probe key's slot to drain", func() bool {
		return srv.listBuilds[probeKey] == nil
	})
}

// panicOnceStore makes its first List panic and serves every later one from
// the wrapped store, so a test can break one shared build and let the next
// one through.
type panicOnceStore struct {
	beads.Store
	mu       sync.Mutex
	armed    bool
	panicked bool
}

func (p *panicOnceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	p.mu.Lock()
	fire := p.armed
	p.panicked = p.panicked || fire
	p.armed = false
	p.mu.Unlock()
	if fire {
		panic("boom from the store read")
	}
	return p.Store.List(query)
}

// TestListBuildSharingFailure pins how the shared list build fails: a panic
// inside a build is a typed 500 to its waiters and leaves the slot recovered,
// so the next request starts a fresh build and succeeds; and a waiter whose
// request ends while the build runs gets a typed 503 that is its alone — the
// build keeps running and the reader behind it is still served by it.
func TestListBuildSharingFailure(t *testing.T) {
	// A panic inside the build: the store's first read panics.
	st := newFakeState(t)
	st.cfg.NamedSessions = nil
	st.cfg.Agents = []config.Agent{
		{Name: "worker1", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
	}
	panicStore := &panicOnceStore{Store: st.stores["myrig"], armed: true}
	st.stores["myrig"] = panicStore
	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)

	oldTTL, oldFloor := responseCacheTTL, agentListResponseTTLFloor
	responseCacheTTL, agentListResponseTTLFloor = 0, 0
	t.Cleanup(func() {
		responseCacheTTL, agentListResponseTTLFloor = oldTTL, oldFloor
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agents"), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a panicking build answered status %d, want 500: a panic in a build is a typed failure to its waiters", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "building the list panicked") {
		t.Errorf("a panicking build answered %q, want the typed panic detail: the waiters' error names the failure, not the panic itself", body)
	}
	awaitSlotState(t, srv, "the failed build's slot to drain", func() bool {
		return len(srv.listBuilds) == 0
	})

	// The slot recovered: the next request starts a fresh build, the store
	// no longer panics, and it succeeds.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agents"), nil))
	if rec.Code != http.StatusOK {
		t.Errorf("the request after a panicking build answered status %d, want 200: the slot recovered, the next reader starts a fresh build", rec.Code)
	}
	panicStore.mu.Lock()
	panicked := panicStore.panicked
	panicStore.mu.Unlock()
	if !panicked {
		t.Error("the store never panicked: the first request's build did not reach it, so nothing was tested")
	}
	awaitSlotState(t, srv, "the recovered build's slot to drain", func() bool {
		return len(srv.listBuilds) == 0
	})

	// A waiter whose request ends while the build runs: a typed 503 that is
	// its alone, the build unaffected, the reader behind it served.
	st2 := newFakeState(t)
	st2.cfg.NamedSessions = nil
	st2.cfg.Agents = []config.Agent{
		{Name: "worker1", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
	}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{})}
	gated := &gatedActiveWorkStore{Store: st2.stores["myrig"], gates: gates, arrived: arrived}
	st2.stores["myrig"] = gated
	srv2 := New(st2)
	h2 := newTestCityHandlerWith(t, st2, srv2)

	releases := make([]func(), len(gates))
	for i := range gates {
		g := gates[i]
		releases[i] = sync.OnceFunc(func() { close(g) })
	}
	t.Cleanup(func() {
		for _, r := range releases {
			r()
		}
	})

	type agentCall struct {
		id   int
		code int
		body string
	}
	calls := make(chan agentCall, 2)
	anyBuildRunning := func() bool {
		for _, slot := range srv2.listBuilds {
			if slot.running != nil {
				return true
			}
		}
		return false
	}
	queuedBehindRunning := func() bool {
		for _, slot := range srv2.listBuilds {
			if slot.running != nil && slot.queued != nil {
				return true
			}
		}
		return false
	}

	// The canceled waiter starts the build and leaves while it holds.
	ctxA, cancelA := context.WithCancel(context.Background())
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, cityURL(st2, "/agents"), nil).WithContext(ctxA)
		h2.ServeHTTP(rec, req)
		calls <- agentCall{id: 1, code: rec.Code, body: rec.Body.String()}
	}()
	awaitSlotState(t, srv2, "the canceled waiter's build to start", anyBuildRunning)
	awaitSignal(t, arrived[0], "the canceled waiter's build's in_progress read")
	cancelA()
	rA := <-calls
	if rA.id != 1 {
		t.Fatalf("the canceled waiter's response is reader %d's", rA.id)
	}
	if rA.code != http.StatusServiceUnavailable {
		t.Errorf("a waiter whose request ended while the build ran answered status %d, want 503: the cancelation is the waiter's alone", rA.code)
	}
	if !strings.Contains(rA.body, "did not finish before the request ended") {
		t.Errorf("a canceled waiter answered %q, want the typed cancelation detail", rA.body)
	}

	// The build the canceled waiter left behind still serves the reader
	// that joins the build queued behind it.
	go func() {
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st2, "/agents"), nil))
		calls <- agentCall{id: 2, code: rec.Code, body: rec.Body.String()}
	}()
	awaitSlotState(t, srv2, "a queued build to form behind the running one", queuedBehindRunning)
	releases[0]()
	awaitSignal(t, arrived[1], "the queued build's in_progress read")
	releases[1]()
	rB := <-calls
	if rB.id != 2 {
		t.Fatalf("the second waiter's response is reader %d's", rB.id)
	}
	if rB.code != http.StatusOK {
		t.Errorf("the reader behind the canceled waiter answered status %d, want 200: the build keeps running and the queued build serves its own reader", rB.code)
	}
	if gated.reads != 2 {
		t.Errorf("GET /agents ran %d builds for the two readers, want 2: the canceled waiter's build ended, the queued one served the next reader", gated.reads)
	}
	awaitSlotState(t, srv2, "the slots to drain once both builds end", func() bool {
		return len(srv2.listBuilds) == 0
	})
}
