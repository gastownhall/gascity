package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// newPoolStepsTestLane is a lane over env at stepsTestNow with the pool
// steps over the summary sum hands them and the stalled inbox.
func newPoolStepsTestLane(env externalReadsEnv, sum func() *allocSummary, stalled func(executionStalledRequest)) (*externalReadsLane, *atomic.Int64) {
	lane, wakes := newTestBackstopLane(env)
	lane.now = func() time.Time { return stepsTestNow }
	lane.addPoolSteps(sum, stalled)
	return lane, wakes
}

// nudgeStepsFixture is a running idle pool slot whose trigger bead work-a is
// still open, on a store the nudges step reads its rows from.
func nudgeStepsFixture(t *testing.T) (externalReadsEnv, beads.Store) {
	t.Helper()
	sess := idleClaimPoolSession()
	sess.Labels = []string{sessionBeadLabel}
	store := beads.NewMemStoreFrom(0, []beads.Bead{sess}, nil)
	return externalReadsEnv{Cfg: idleClaimTestCfg(), CityStore: store, SP: runningIdleClaimFake(t, "session-a")}, store
}

// Kills: nudging from stale or empty demand. The claim backstop starts its
// grace on a slot whose trigger the last allocation holds, as assigned or
// as ready routed work, and on nothing when there is no allocation yet or
// the last one is older than a recording stays fresh.
func TestNudgeStepsUseLastAllocation(t *testing.T) {
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	for _, tc := range []struct {
		name string
		sum  *allocSummary
		want string
	}{
		{name: "assigned", sum: &allocSummary{At: stepsTestNow, AssignedWork: work}, want: "work-a"},
		{name: "ready routed", sum: &allocSummary{At: stepsTestNow, ReadyRouted: work, ReadyRoutedRefs: []string{""}}, want: "work-a"},
		{name: "no allocation"},
		{name: "stale", sum: &allocSummary{At: stepsTestNow.Add(-externalReadsFreshPatrols*backstopTestInterval - time.Second), AssignedWork: work}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, store := nudgeStepsFixture(t)
			lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return tc.sum }, nil)
			lane.nudges(context.Background(), env)
			if got := mustGetTestBead(t, store, "session-bead-a").Metadata[idleClaimNudgeTriggerKey]; got != tc.want {
				t.Errorf("idle-claim marker = %q, want %q", got, tc.want)
			}
		})
	}
}

// orphanSummary is a summary over one in_progress pool bead whose assignee
// no open session holds.
func orphanSummary(t *testing.T, store beads.Store) func() *allocSummary {
	t.Helper()
	work, err := store.Create(beads.Bead{Title: "orphaned", Assignee: "worker-dead", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	work = mustGetTestBead(t, store, work.ID)
	return func() *allocSummary {
		b := work
		b.Metadata, b.Labels = maps.Clone(work.Metadata), slices.Clone(work.Labels)
		return &allocSummary{At: stepsTestNow, AssignedWork: []beads.Bead{b}, AssignedStoreRefs: []string{""}}
	}
}

func orphanCfg() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
}

// Kills: releasing a live holder's work from an incomplete snapshot (INC-016).
// A partial or stale summary releases nothing; a complete one releases the
// orphan and records bead.dead_assignee_reopened.
func TestOrphanReleaseSkipsIncompleteSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*allocSummary)
		release bool
	}{
		{name: "partial", edit: func(s *allocSummary) { s.Partial = true }},
		{name: "stale", edit: func(s *allocSummary) { s.At = s.At.Add(-time.Hour) }},
		{name: "complete", edit: func(*allocSummary) {}, release: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			base := orphanSummary(t, store)
			sum := base()
			tc.edit(sum)
			rec := events.NewFake()
			env := externalReadsEnv{Cfg: orphanCfg(), CityStore: store}
			lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, nil)
			lane.events = rec
			lane.orphanRelease(context.Background(), env)
			got := mustGetTestBead(t, store, sum.AssignedWork[0].ID)
			evts, _ := rec.List(events.Filter{Type: events.BeadDeadAssigneeReopened})
			if released := got.Status == "open" && got.Assignee == ""; released != tc.release || (len(evts) == 1) != tc.release {
				t.Errorf("status=%q assignee=%q events=%d, want released=%t", got.Status, got.Assignee, len(evts), tc.release)
			}
		})
	}
}

// Kills: a step that reads a summary the next pass rewrites, or edits the
// one it loaded (S-14). Under -race, the nudges and orphan-release steps run
// while another goroutine reads every summary field, and the summary they
// were handed still equals a fresh build of it afterwards.
func TestNudgeStepsReadImmutableSummary(t *testing.T) {
	env, store := nudgeStepsFixture(t)
	env.Cfg.Agents = append(env.Cfg.Agents, orphanCfg().Agents...)
	build := orphanSummary(t, store)
	trigger := beads.Bead{ID: "work-a", Status: "open", Metadata: map[string]string{"k": "v"}}
	fresh := func() *allocSummary {
		s := build()
		s.AssignedWork = append(s.AssignedWork, trigger)
		s.AssignedStoreRefs = []string{"", ""}
		s.ReadyAssigned = map[storeScopedBeadKey]bool{{ID: "work-a"}: true}
		return s
	}
	sum := fresh()
	var published atomic.Pointer[allocSummary]
	published.Store(sum)
	lane, _ := newPoolStepsTestLane(env, published.Load, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := published.Load()
			for _, b := range s.AssignedWork {
				_ = fmt.Sprint(b.Status, b.Assignee, b.Metadata)
			}
			_ = fmt.Sprint(s.ReadyAssigned, s.AssignedStoreRefs, s.OpenSessions)
		}
	}()
	for range 3 {
		lane.nudges(context.Background(), env)
		lane.orphanRelease(context.Background(), env)
	}
	close(stop)
	wg.Wait()
	if want := fresh(); !reflect.DeepEqual(sum, want) {
		t.Fatalf("summary edited by a step:\n got %+v\nwant %+v", sum, want)
	}
}

// Kills: an execution-stalled drain request that never reaches the planner,
// or reaches it with a generation the row no longer holds. The step's
// request reads the row fresh, posts it with a dirty mark, and the next
// gather carries it in World.ExecutionStalled; a row the census dropped is
// forgotten, and a row that cannot be read posts nothing.
func TestExecutionStalledRequestReachesPlanner(t *testing.T) {
	row := routerSessionBead("s1", map[string]string{"session_name": "s-s1", "state": "asleep", "template": "worker", "generation": "3"})
	f := newGatherFixture(t, row)
	env := externalReadsEnv{CityStore: f.cache}
	lane, _ := newPoolStepsTestLane(env, nil, f.p.postExecutionStalled)
	request := lane.requestExecutionStalled(env)

	if err := request(beads.Bead{ID: "s1"}); err != nil {
		t.Fatalf("request: %v", err)
	}
	select {
	case <-f.p.dirty:
	default:
		t.Fatal("the request left the planner clean")
	}
	if err := request(beads.Bead{ID: "absent"}); err == nil {
		t.Error("request for an unreadable row: want an error")
	}
	f.p.postExecutionStalled(executionStalledRequest{ID: "gone", Generation: "1"})
	w := f.gather(t)
	want := map[string]executionStalledRequest{"s1": {ID: "s1", Generation: "3", At: stepsTestNow}}
	if !reflect.DeepEqual(w.ExecutionStalled, want) {
		t.Fatalf("World.ExecutionStalled = %v, want %v", w.ExecutionStalled, want)
	}
	if w = f.gather(t); !reflect.DeepEqual(w.ExecutionStalled, want) {
		t.Errorf("next pass: %v, want the request held for A16", w.ExecutionStalled)
	}
}

// Kills orphan release and the continuation and execution backstops running
// over an incomplete session census (INC-016): the summary a pass publishes
// carries its time and the census's open rows, and is partial when the work
// read or any census leg failed, a hard rig-leg failure included.
func TestAllocSummaryCarriesCensusSnapshot(t *testing.T) {
	ok := censusStore(censusSession("gc-1", map[string]string{"state": "active"}))
	partial := censusErrStore{censusStore(), &beads.PartialResultError{Op: "list", Err: errors.New("down")}}
	down := censusErrStore{censusStore(), errors.New("down")}
	for _, tc := range []struct {
		name         string
		legs         []classStoreCandidate
		storePartial bool
		want         bool
	}{
		{name: "complete", legs: censusLegs("class:sessions", ok)},
		{name: "work read partial", legs: censusLegs("class:sessions", ok), storePartial: true, want: true},
		{name: "rig leg partial", legs: censusLegs("class:sessions", ok, "rig:a", partial), want: true},
		{name: "rig leg down", legs: censusLegs("class:sessions", ok, "rig:a", down), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := readCensus(t, gatherNow, tc.legs)
			s := newAllocSummary(gatherNow, &World{Census: c, Demand: demandView{StorePartial: tc.storePartial}}, &allocDecision{})
			if s.Partial != tc.want || !s.At.Equal(gatherNow) || len(s.OpenSessions) != 1 || s.OpenSessions[0].ID != "gc-1" {
				t.Errorf("summary = partial %t at %v open %v, want partial %t at %v open [gc-1]", s.Partial, s.At, s.OpenSessions, tc.want, gatherNow)
			}
		})
	}
}
