package main

// Tests for the concurrent leg reads in federateBeadLegs and
// federateListBeadsWithOwner (ga-ntr7gn). Each leg is typically its own
// subprocess spawn, so reading legs one after another costs the SUM of the
// per-leg latencies where a caller only ever waits on the MAX. See
// ready_federation.go's readLegsConcurrently.
//
// cmd_ready_test.go already pins the sequential contracts these tests
// extend to the concurrent case: TestReadyDedupeIsFirstLegWins (first LEG
// wins) and TestReadyFailsLoudWhenALegErrors (any leg error aborts the whole
// federation). Both contracts are stated in terms of leg POSITION, which is
// exactly the property a naive "collect off a completion-ordered channel"
// concurrent implementation would silently violate while still passing every
// existing test, since none of them force one leg to finish before another.
//
// Overlap and completion order are forced with channels, never with sleeps or
// wall-clock budgets: TESTING.md has asynchronous tests wait for facts, not
// elapsed time.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// rendezvous holds every caller of wait until n callers have arrived and the
// test calls open. A test uses it to prove n operations were in flight at the
// same time without timing anything: a subject that runs them one after another
// never gets a second caller to arrive, so the test fails through awaitClose's
// hang budget instead of asserting a wall-clock bound.
type rendezvous struct {
	n       int32
	arrived atomic.Int32
	all     chan struct{} // closed once the n-th caller has arrived
	release chan struct{} // closed by open
	once    sync.Once
}

func newRendezvous(t *testing.T, n int) *rendezvous {
	t.Helper()
	r := &rendezvous{n: int32(n), all: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(r.open) // never strand a parked worker if the test fails before opening
	return r
}

// wait registers one caller and parks it until the test calls open.
func (r *rendezvous) wait() {
	if r.arrived.Add(1) == r.n {
		close(r.all)
	}
	<-r.release
}

// open releases every parked caller; it is safe to call more than once.
func (r *rendezvous) open() { r.once.Do(func() { close(r.release) }) }

// completionOrder lets a test force one operation to finish strictly after
// another without timing either: the operation that holds waitFor blocks until
// the operation that holds signal has finished.
type completionOrder struct {
	finished chan struct{}
	once     sync.Once
}

func newCompletionOrder(t *testing.T) *completionOrder {
	t.Helper()
	c := &completionOrder{finished: make(chan struct{})}
	t.Cleanup(c.markFinished) // never strand a held operation if the test fails first
	return c
}

func (c *completionOrder) markFinished() { c.once.Do(func() { close(c.finished) }) }

// inGoroutine runs fn on its own goroutine and returns a channel that closes
// when fn returns, so a test whose subject can wedge reports through awaitClose
// instead of hanging the whole package. Whatever fn wrote is safe to read once
// the channel is closed.
func inGoroutine(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// readyRendezvousStore parks every ready read at a rendezvous before it
// delegates, so a test can see how many legs are being read at once.
type readyRendezvousStore struct {
	beads.Store
	at *rendezvous
}

func (s readyRendezvousStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.at.wait()
	return s.Store.Ready(query...)
}

// readyOrderedStore finishes its reads after the operation it waits for and
// signals its own completion, so a test can drive one leg to complete strictly
// before or after another. Either field may be nil.
type readyOrderedStore struct {
	beads.Store
	waitFor *completionOrder
	signal  *completionOrder
}

func (s readyOrderedStore) hold() {
	if s.waitFor != nil {
		<-s.waitFor.finished
	}
}

func (s readyOrderedStore) release() {
	if s.signal != nil {
		s.signal.markFinished()
	}
}

func (s readyOrderedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.hold()
	defer s.release()
	return s.Store.Ready(query...)
}

func (s readyOrderedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.hold()
	defer s.release()
	return s.Store.List(query)
}

// TestFederateBeadLegsRunsLegsConcurrently pins that every leg is read at the
// same time: each leg's store parks at a rendezvous that only opens once ALL
// legs are in flight, which a leg-after-leg reader can never satisfy.
func TestFederateBeadLegsRunsLegsConcurrently(t *testing.T) {
	const legCount = 5
	at := newRendezvous(t, legCount)
	legs := make([]readyLeg, legCount)
	for i := range legs {
		legs[i] = readyTestLeg(fmt.Sprintf("leg-%d", i), readyRendezvousStore{
			Store: splittest.NewWorkStore(t, fmt.Sprintf("w%d", i)),
			at:    at,
		})
	}

	var err error
	done := inGoroutine(func() {
		_, err = federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
			return store.Ready()
		})
	})
	awaitClose(t, at.all, "all legs to be read at the same time")
	at.open()
	awaitClose(t, done, "federateBeadLegs to return")
	if err != nil {
		t.Fatalf("federateBeadLegs: %v", err)
	}
}

// TestFederateBeadLegsDedupeSurvivesOutOfOrderCompletion is
// TestReadyDedupeIsFirstLegWins's concurrent twin: "first leg wins" means
// first by POSITION in the leg list, not first to finish reading.
func TestFederateBeadLegsDedupeSurvivesOutOfOrderCompletion(t *testing.T) {
	work, graph := splittest.NewSplitStores(t)
	workCopy := mustCreateReadyBead(t, work, beads.Bead{Title: "work leg row", Type: "task"})
	forced, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class store %T cannot model the migration's forced foreign-id copy", graph)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: workCopy.ID, Title: "graph leg row", Type: "task"}); err != nil {
		t.Fatalf("copy %s into the class store: %v", workCopy.ID, err)
	}

	// The FIRST leg (city/work) is forced to finish AFTER the second (graph): its
	// read blocks until the graph read has completed. Position-wins must still
	// resolve to the work leg's row.
	graphFirst := newCompletionOrder(t)
	legs := []readyLeg{
		readyTestLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
		readyTestLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
	}

	var rows []beads.Bead
	var err error
	awaitClose(t, inGoroutine(func() {
		rows, err = federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
			return store.Ready()
		})
	}), "federateBeadLegs to return")
	if err != nil {
		t.Fatalf("federateBeadLegs: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "work leg row" {
		t.Fatalf("federateBeadLegs merged = %v, want exactly the first leg's row even though the second leg answered first; dedupe must be by LEG POSITION, not completion order", rows)
	}
}

// TestFederateBeadLegsReportsTheFirstFailingLegByPosition pins error-message
// determinism when more than one leg fails: the error always names the
// first-position failing leg — the one a sequential reader would have hit
// and stopped at — never whichever goroutine's error happened to land first.
func TestFederateBeadLegsReportsTheFirstFailingLegByPosition(t *testing.T) {
	legs := []readyLeg{
		readyTestLeg("city", readyFailingStore{err: errors.New("city is locked")}),
		readyTestLeg("graph", readyFailingStore{err: errors.New("graph is locked")}),
	}
	_, err := federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return store.Ready()
	})
	if err == nil {
		t.Fatal("two dead legs produced a successful answer")
	}
	if !strings.Contains(err.Error(), "city store") || !strings.Contains(err.Error(), "city is locked") {
		t.Fatalf("error = %v, want the FIRST-position leg (city) named", err)
	}
}

// readyPanickingStore panics on every ready read, standing in for a leg whose
// store hits a latent bug.
type readyPanickingStore struct {
	beads.Store
}

func (readyPanickingStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	panic("leg exploded")
}

// TestFederateBeadLegsPropagatesALegPanicToTheCaller pins that a panic inside
// a leg's read reaches the CALLER's goroutine, as it did from the sequential
// loop. Reading legs on worker goroutines must not turn a recoverable panic
// into a process crash.
func TestFederateBeadLegsPropagatesALegPanicToTheCaller(t *testing.T) {
	legs := []readyLeg{
		readyTestLeg("city", splittest.NewWorkStore(t, "gc")),
		readyTestLeg("graph", readyPanickingStore{Store: splittest.NewWorkStore(t, "gr")}),
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
			return store.Ready()
		})
	}()

	if recovered == nil {
		t.Fatal("a leg panic was swallowed; it must reach the caller as it did from the sequential loop")
	}
	if !strings.Contains(fmt.Sprint(recovered), "leg exploded") {
		t.Fatalf("recovered %v, want the leg's own panic text preserved", recovered)
	}
}

// TestFederateListBeadsWithOwnerDedupeSurvivesOutOfOrderCompletion mirrors
// TestFederateBeadLegsDedupeSurvivesOutOfOrderCompletion for
// federateListBeadsWithOwner, which restates the merge loop rather than
// sharing it (see its own doc comment) and so needs the position-wins
// guarantee — for both the merged rows and the ownership map — pinned
// separately.
func TestFederateListBeadsWithOwnerDedupeSurvivesOutOfOrderCompletion(t *testing.T) {
	work, graph := splittest.NewSplitStores(t)
	workCopy := mustCreateReadyBead(t, work, beads.Bead{Title: "work leg row", Type: "task"})
	forced, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class store %T cannot model the migration's forced foreign-id copy", graph)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: workCopy.ID, Title: "graph leg row", Type: "task"}); err != nil {
		t.Fatalf("copy %s into the class store: %v", workCopy.ID, err)
	}
	inProgress, assignee := readyStatusInProgress, "worker-1"
	if err := work.Update(workCopy.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim %s: %v", workCopy.ID, err)
	}
	if err := graph.Update(workCopy.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim %s in the graph leg's copy: %v", workCopy.ID, err)
	}

	graphFirst := newCompletionOrder(t)
	legs := []readyLeg{
		readyTestLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
		readyTestLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
	}

	var rows []beads.Bead
	var owners map[string]readyLeg
	var err error
	awaitClose(t, inGoroutine(func() {
		rows, owners, err = federateListBeadsWithOwner(legs, beads.ListQuery{Status: readyStatusInProgress})
	}), "federateListBeadsWithOwner to return")
	if err != nil {
		t.Fatalf("federateListBeadsWithOwner: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "work leg row" {
		t.Fatalf("federateListBeadsWithOwner merged = %v, want exactly the first leg's row even though the second leg answered first", rows)
	}
	if owner := owners[workCopy.ID]; owner.label != "city" {
		t.Fatalf("federateListBeadsWithOwner owner = %q, want %q — ownership must also resolve by leg POSITION, not completion order", owner.label, "city")
	}
}
