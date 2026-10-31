package readywork

// Tests for how the reader reads its legs: concurrently, under a bound, and
// merged by leg POSITION. First-leg-wins dedupe and the fail-loud rule are
// stated in terms of position, which is exactly what a naive concurrent reader
// that collected results off a completion-ordered channel would break while
// still passing every sequential test, so these tests force legs to finish out
// of order.
//
// Overlap is measured inside testing/synctest bubbles: synctest.Wait returns
// only once every goroutine is parked, so "how many legs are in flight" is a
// count, not a timing. Completion order is forced with channels; no test here
// sleeps or bounds wall-clock time (TESTING.md: wait for facts).

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/fanout/fanouttest"
)

// testLeg is a leg with the fields a test cares about.
func testLeg(label string, store beads.Store) Leg {
	return Leg{Label: label, Store: store}
}

// mustCreate creates a bead and fails the test on error.
func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}

// readyGatedStore parks every read on probe before delegating, so a synctest
// bubble can count how many legs are being read at once.
type readyGatedStore struct {
	beads.Store
	probe *fanouttest.Probe
}

func (s readyGatedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.probe.Call(0)
	return s.Store.Ready(query...)
}

func (s readyGatedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.probe.Call(0)
	return s.Store.List(query)
}

// completionOrder lets a test force one operation to finish strictly after
// another without timing either: the operation holding waitFor blocks until
// the one holding signal has finished.
type completionOrder struct {
	finished chan struct{}
	once     sync.Once
}

func newCompletionOrder() *completionOrder {
	return &completionOrder{finished: make(chan struct{})}
}

func (c *completionOrder) markFinished() { c.once.Do(func() { close(c.finished) }) }

// readyOrderedStore finishes its reads after the operation it waits for and
// then signals its own completion, optionally failing. Either order field may
// be nil.
type readyOrderedStore struct {
	beads.Store
	waitFor *completionOrder
	signal  *completionOrder
	err     error
}

func (s readyOrderedStore) read(fn func() ([]beads.Bead, error)) ([]beads.Bead, error) {
	if s.waitFor != nil {
		<-s.waitFor.finished
	}
	if s.signal != nil {
		defer s.signal.markFinished()
	}
	if s.err != nil {
		return nil, s.err
	}
	return fn()
}

func (s readyOrderedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.read(func() ([]beads.Bead, error) { return s.Store.Ready(query...) })
}

func (s readyOrderedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return s.read(func() ([]beads.Bead, error) { return s.Store.List(query) })
}

// gatedLegs builds n legs over empty stores, every read parked on probe.
func gatedLegs(n int, probe *fanouttest.Probe) []Leg {
	legs := make([]Leg, n)
	for i := range legs {
		legs[i] = testLeg(fmt.Sprintf("leg-%d", i), readyGatedStore{Store: beads.NewMemStore(), probe: probe})
	}
	return legs
}

// TestFederateReadyBeadsReadsEveryLegAtOnce pins the read half of the latency
// fix: with fewer legs than the fan-out bound, every leg is being read at the
// same time, so a federated read costs the slowest leg rather than the sum.
func TestFederateReadyBeadsReadsEveryLegAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const legCount = 5
		probe := fanouttest.NewProbe()
		defer probe.Open()

		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, err = federate(context.Background(), gatedLegs(legCount, probe), readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier}))
		}()
		synctest.Wait()
		if got := probe.Running(); got != legCount {
			t.Fatalf("%d of %d legs were being read at once, want all of them: a leg-after-leg reader makes one gc ready cost the sum of every store's latency", got, legCount)
		}
		probe.Open()
		<-done
		if err != nil {
			t.Fatalf("federate: %v", err)
		}
	})
}

// TestFederateReadyBeadsBoundsTheLegFanOut pins the bound: a city with more
// legs than LegFanOut never has more than that many reads in flight.
func TestFederateReadyBeadsBoundsTheLegFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		legCount := LegFanOut + 3
		probe := fanouttest.NewProbe()
		defer probe.Open()

		var rows []beads.Bead
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			rows, _, err = federate(context.Background(), gatedLegs(legCount, probe), readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier}))
		}()
		synctest.Wait()
		if got := probe.Running(); got != int64(LegFanOut) {
			t.Fatalf("%d legs being read at once over a %d-leg city, want exactly the bound %d", got, legCount, LegFanOut)
		}
		probe.Open()
		<-done
		if err != nil || len(rows) != 0 {
			t.Fatalf("federateReadyBeads = %v, %v; want an empty answer from empty legs", rows, err)
		}
		if got := len(probe.SortedVisits()); got != legCount {
			t.Fatalf("%d of %d legs were read, want every leg exactly once", got, legCount)
		}
	})
}

// TestReadStartsNoLegReadOnceItsContextEnds pins cancellation at the leg
// fan-out: with every slot held by a parked read and one leg still waiting for
// a slot, ending the context means that leg is never read, and the answer is
// the context's error rather than rows.
func TestReadStartsNoLegReadOnceItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		legCount := LegFanOut + 1
		probe := fanouttest.NewProbe()
		defer probe.Open()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var rows []ReadyBead
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			rows, err = ReadContext(ctx, gatedLegs(legCount, probe), Query{})
		}()
		synctest.Wait()
		if got := probe.Running(); got != int64(LegFanOut) {
			t.Fatalf("%d legs being read at once, want the bound %d with one leg waiting for a slot", got, LegFanOut)
		}
		cancel()
		probe.Open()
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadContext = %v, %v; want the context's error once it ended", rows, err)
		}
		if got := len(probe.SortedVisits()); got != LegFanOut {
			t.Fatalf("%d of %d legs were read, want only the %d already in flight when the context ended", got, legCount, LegFanOut)
		}
	})
}

// TestFederateListBeadsWithOwnerReadsEveryLegAtOnce is the same property for
// the crash-recovery arm, which restates the merge loop rather than sharing it.
func TestFederateListBeadsWithOwnerReadsEveryLegAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const legCount = 4
		probe := fanouttest.NewProbe()
		defer probe.Open()

		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, err = federate(context.Background(), gatedLegs(legCount, probe), listRead(beads.ListQuery{Status: StatusInProgress, TierMode: beads.FederatedReadTier}))
		}()
		synctest.Wait()
		if got := probe.Running(); got != legCount {
			t.Fatalf("%d of %d legs were being listed at once, want all of them", got, legCount)
		}
		probe.Open()
		<-done
		if err != nil {
			t.Fatalf("federate: %v", err)
		}
	})
}

// seedCoResidentPair puts one bead in the work store and a forced copy of the
// same id in the class store, titled after the leg that holds it: the steady
// state of a migrated city, where the migration preserves ids.
func seedCoResidentPair(t *testing.T) (work, graph beads.Store, id string) {
	t.Helper()
	work, graph = splittest.NewSplitStores(t)
	workCopy := mustCreate(t, work, beads.Bead{Title: "work leg row", Type: "task"})
	forced, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class store %T cannot model the migration's forced foreign-id copy", graph)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: workCopy.ID, Title: "graph leg row", Type: "task"}); err != nil {
		t.Fatalf("copy %s into the class store: %v", workCopy.ID, err)
	}
	return work, graph, workCopy.ID
}

// TestFederateReadyBeadsDedupeIsByLegPositionNotCompletion is
// TestReadyDedupeIsFirstLegWins's concurrent twin: the first leg is forced to
// answer LAST, and its row must still win.
func TestFederateReadyBeadsDedupeIsByLegPositionNotCompletion(t *testing.T) {
	work, graph, id := seedCoResidentPair(t)
	synctest.Test(t, func(t *testing.T) {
		graphFirst := newCompletionOrder()
		defer graphFirst.markFinished()
		legs := []Leg{
			testLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
			testLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
		}
		rows, _, err := federate(context.Background(), legs, readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier}))
		if err != nil {
			t.Fatalf("federate: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != id || rows[0].Title != "work leg row" {
			t.Fatalf("merged = %v, want only the first leg's row for %s even though the second leg answered first", rows, id)
		}
	})
}

// TestFederateListBeadsWithOwnerResolvesOwnershipByLegPosition pins the same
// rule for the merged rows AND the ownership map the blocked_by enrichment
// reads through.
func TestFederateListBeadsWithOwnerResolvesOwnershipByLegPosition(t *testing.T) {
	work, graph, id := seedCoResidentPair(t)
	inProgress, assignee := StatusInProgress, "worker-1"
	for _, store := range []beads.Store{work, graph} {
		if err := store.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		graphFirst := newCompletionOrder()
		defer graphFirst.markFinished()
		legs := []Leg{
			testLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
			testLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
		}
		rows, owners, err := federate(context.Background(), legs, listRead(beads.ListQuery{Status: StatusInProgress, TierMode: beads.FederatedReadTier}))
		if err != nil {
			t.Fatalf("federate: %v", err)
		}
		if len(rows) != 1 || rows[0].Title != "work leg row" {
			t.Fatalf("merged = %v, want only the first leg's row", rows)
		}
		if owner := legs[owners[id]].Label; owner != "city" {
			t.Fatalf("owner of %s = %q, want %q: ownership resolves by leg position, not by which leg answered first", id, owner, "city")
		}
	})
}

// TestFederateReadyBeadsReportsTheFirstFailingLegByPosition pins error
// determinism: when two legs fail, the error names the one a sequential reader
// would have stopped at, even though the later leg failed first. And a failing
// leg still fails the whole answer — concurrency does not turn the fail-loud
// rule into a partial array.
func TestFederateReadyBeadsReportsTheFirstFailingLegByPosition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		laterFailsFirst := newCompletionOrder()
		defer laterFailsFirst.markFinished()
		healthy := beads.NewMemStore()
		mustCreate(t, healthy, beads.Bead{Title: "healthy work", Type: "task"})
		legs := []Leg{
			testLeg("city", healthy),
			testLeg("rig alpha", readyOrderedStore{Store: beads.NewMemStore(), waitFor: laterFailsFirst, err: errors.New("alpha is locked")}),
			testLeg("rig beta", readyOrderedStore{Store: beads.NewMemStore(), signal: laterFailsFirst, err: errors.New("beta is locked")}),
		}
		rows, _, err := federate(context.Background(), legs, readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier}))
		if err == nil {
			t.Fatalf("two dead legs produced an answer of %d rows; a short array is indistinguishable from \"no work\"", len(rows))
		}
		if !strings.Contains(err.Error(), "rig alpha store") || !strings.Contains(err.Error(), "alpha is locked") {
			t.Fatalf("error = %v, want the first-POSITION failing leg (rig alpha) named", err)
		}
	})
}

// snapshotStore is a leg whose read cache holds a stale snapshot: the shape of
// a CachingStore whose last reconcile predates writes an agent's own `bd` made.
// Its own reads and its Cached handle answer from the snapshot, the embedded
// store; only its Live handle reads the backing store.
type snapshotStore struct {
	beads.Store
	backing beads.Store
}

func (s snapshotStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: beads.HandlesFor(s.Store).Cached,
		Live:   beads.HandlesFor(s.backing).Live,
		Writer: s.backing,
	}
}

// TestReadAnswersEveryStatusFromTheLiveHandle pins the package's
// every-read-is-live rule on every arm: the ready default, each --status list,
// and the in_progress rows' blocked_by enrichment. Every row, and the one
// dependency edge, is written after the leg's cache took its snapshot, so an
// arm that read the cache, through the Cached handle or through the store's
// own reads, would answer short; each arm must answer exactly as the backing
// store.
func TestReadAnswersEveryStatusFromTheLiveHandle(t *testing.T) {
	backing := beads.NewMemStore()
	ids := map[string]string{}
	for _, status := range KnownStatuses() {
		ids[status] = mustCreate(t, backing, beads.Bead{Title: status + " work", Type: "task"}).ID
	}
	for _, status := range []string{StatusInProgress, StatusBlocked} {
		if err := backing.Update(ids[status], beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set %s: %v", status, err)
		}
	}
	if err := backing.Close(ids[StatusClosed]); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := backing.DepAdd(ids[StatusInProgress], ids[StatusOpen], "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}
	stale := []Leg{testLeg("city", snapshotStore{Store: beads.NewMemStore(), backing: backing})}
	live := []Leg{testLeg("city", backing)}
	for _, status := range append([]string{""}, KnownStatuses()...) {
		q := Query{Status: status}
		want, err := Read(live, q)
		if err != nil {
			t.Fatalf("--status=%q over the backing store: %v", status, err)
		}
		if len(want) == 0 {
			t.Fatalf("--status=%q: the backing store answers no rows; the fixture must give every arm a row", status)
		}
		if status == StatusInProgress && len(want[0].BlockedBy) == 0 {
			t.Fatal("--status=in_progress: the backing store's row carries no blocked_by; the fixture's edge must give the enrichment a blocker to read")
		}
		got, err := Read(stale, q)
		if err != nil {
			t.Fatalf("--status=%q: %v", status, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("--status=%q over a leg whose cache is stale answered %+v, want the backing store's %+v", status, got, want)
		}
	}
}
