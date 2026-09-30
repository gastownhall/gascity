package main

// Tests for the concurrency fix to federateBeadLegs and
// federateListBeadsWithOwner (ga-ntr7gn). Before this fix both ran a plain
// sequential for loop over the legs, turning a per-leg subprocess-spawn cost
// (bd, ~750-800ms baseline) into a SUM rather than a MAX across up to 9 legs
// on a real split city — measured 14266-15716ms wall-clock for `gc ready`
// there. See ready_federation.go's federateBeadLegs doc comment.
//
// cmd_ready_test.go already pins the sequential contracts these tests
// extend to the concurrent case: TestReadyDedupeIsFirstLegWins (first LEG
// wins) and TestReadyFailsLoudWhenALegErrors (any leg error aborts the whole
// federation). Both contracts are stated in terms of leg POSITION, which is
// exactly the property a naive "collect off a completion-ordered channel"
// concurrent implementation would silently violate while still passing every
// existing test, since none of them stagger per-leg latency.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// readyDelayedStore sleeps before every read, so a test can drive a leg with
// a controllable latency and assert federateBeadLegs's behavior — wall-clock
// cost, or dedupe/error selection under staggered completion — rather than
// only its row content.
type readyDelayedStore struct {
	beads.Store
	delay time.Duration
}

func (s readyDelayedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	time.Sleep(s.delay)
	return s.Store.Ready(query...)
}

func (s readyDelayedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	time.Sleep(s.delay)
	return s.Store.List(query)
}

// TestFederateBeadLegsRunsLegsConcurrently is the wall-clock regression test
// for ga-ntr7gn: reading N legs must cost about as much as the SLOWEST leg,
// not the sum of every leg.
func TestFederateBeadLegsRunsLegsConcurrently(t *testing.T) {
	const perLeg = 100 * time.Millisecond
	const legCount = 5
	legs := make([]readyLeg, legCount)
	for i := range legs {
		legs[i] = readyTestLeg(fmt.Sprintf("leg-%d", i), readyDelayedStore{
			Store: splittest.NewWorkStore(t, fmt.Sprintf("w%d", i)),
			delay: perLeg,
		})
	}

	start := time.Now()
	if _, err := federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return store.Ready()
	}); err != nil {
		t.Fatalf("federateBeadLegs: %v", err)
	}
	elapsed := time.Since(start)

	// A sequential reader costs legCount*perLeg (500ms). A concurrent one
	// costs about one perLeg (100ms). The budget sits well under the
	// sequential floor while leaving generous headroom over the concurrent
	// floor for scheduler jitter, so this fails on a regression to sequential
	// reads without flaking on a loaded box.
	if budget := legCount * perLeg / 2; elapsed >= budget {
		t.Fatalf("federateBeadLegs took %v to read %d legs at %v delay each; want well under %v (the sequential floor is %v) — legs did not run concurrently", elapsed, legCount, perLeg, budget, legCount*perLeg)
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

	// The FIRST leg (city/work) is the SLOW one; the SECOND leg (graph)
	// answers first. Position-wins must still resolve to the work leg's row.
	legs := []readyLeg{
		readyTestLeg("city", readyDelayedStore{Store: work, delay: 80 * time.Millisecond}),
		readyTestLeg("graph", graph),
	}

	rows, err := federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return store.Ready()
	})
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

	legs := []readyLeg{
		readyTestLeg("city", readyDelayedStore{Store: work, delay: 80 * time.Millisecond}),
		readyTestLeg("graph", graph),
	}

	rows, owners, err := federateListBeadsWithOwner(legs, beads.ListQuery{Status: readyStatusInProgress})
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
