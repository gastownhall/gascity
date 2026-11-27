package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
