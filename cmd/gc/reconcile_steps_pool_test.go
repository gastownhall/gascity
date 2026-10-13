package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
	row := routerSessionBead("s1", map[string]string{"session_name": "s-s1", "state": "active", "template": "worker", "generation": "3"})
	f := newGatherFixture(t, row)
	// A live seat: a dormant one, or one whose runtime is gone, spends its
	// request (mc-uinl0).
	clk := &clock.Fake{Time: gatherNow}
	obs := NewObservationCache(clk, time.Minute, "e1")
	obs.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "s-s1")), nil)
	f.env.Observations = func() *ObservationCache { return obs }
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

// The orphan-release step runs the stranded-work detector: a claim no lane
// serves on a closed session raises session.stranded_claims.
func TestOrphanReleaseDetectsStrandedClaims(t *testing.T) {
	store := beads.NewMemStore()
	seat, err := store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker-dead", "template": "worker", "state": "drained"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(seat.ID); err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{Title: "stranded", Assignee: "worker-dead", Metadata: map[string]string{"gc.routed_to": "nowhere", beadmeta.StrandedMetadataKey: "true", beadmeta.StrandedSessionMetadataKey: seat.ID, beadmeta.StrandedAssigneeMetadataKey: "worker-dead"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := &allocSummary{At: stepsTestNow, AssignedWork: []beads.Bead{mustGetTestBead(t, store, work.ID)}, AssignedStoreRefs: []string{""}}
	rec := events.NewFake()
	env := externalReadsEnv{Cfg: orphanCfg(), CityStore: store, WorkLegs: testWorkLegs("", orphanCfg(), store, nil)}
	lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, nil)
	lane.stranded = &strandedClaimAlerts{rec: rec, scanned: stepsTestNow.Add(-time.Hour)}
	lane.orphanRelease(context.Background(), env)
	if evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 || evts[0].Subject != seat.ID {
		t.Fatalf("session.stranded_claims = %+v, want one for %s", evts, seat.ID)
	}
}

// Kills v2's claim backstop leaving a wedged slot on its seat (mc-zndi7.80):
// at the cap, with the slot quiet and its own trigger still ready routed work
// in the last allocation and open on a live read through the work legs, the
// nudges step posts the slot's execution-stalled request to
// the planner (A16 turns it into a drain no lens cancels) once, at the row's
// generation; a trigger no longer in the allocation posts nothing.
func TestNudgeStepsDrainAWedgedSlot(t *testing.T) {
	for _, routed := range []bool{true, false} {
		env, store := nudgeStepsFixture(t)
		store = beads.NewMemStoreFrom(0, []beads.Bead{mustGetTestBead(t, store, "session-bead-a"), {ID: "work-a", Title: "work-a", Type: "task", Status: "open"}}, nil)
		env.CityStore = store
		env.WorkLegs = workLegsFromCensus(t.TempDir(), env.Cfg, cityWorkLegOf(beads.WorkStore{Store: store}), nil)
		env.SP.(*runtime.Fake).SetActivity("session-a", stepsTestNow.Add(-time.Hour)) // quiet
		if err := store.SetMetadataBatch("session-bead-a", map[string]string{
			idleClaimNudgeTriggerKey: "work-a", idleClaimNudgeCountKey: strconv.Itoa(idleClaimNudgeMaxAttempts),
			idleClaimNudgeAtKey: stepsTestNow.Add(-time.Hour).Format(time.RFC3339), idleClaimNudgeTokenKey: "tok-7", "generation": "7", "instance_token": "tok-7",
		}); err != nil {
			t.Fatal(err)
		}
		sum := &allocSummary{At: stepsTestNow}
		if routed {
			sum.ReadyRouted, sum.ReadyRoutedRefs = []beads.Bead{{ID: "work-a", Status: "open"}}, []string{""}
		}
		var posted []executionStalledRequest
		lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, func(r executionStalledRequest) { posted = append(posted, r) })
		lane.nudges(context.Background(), env)
		lane.nudges(context.Background(), env)
		want := 0
		if routed {
			want = 1
		}
		if len(posted) != want || (want == 1 && (posted[0].ID != "session-bead-a" || posted[0].Generation != "7")) {
			t.Fatalf("routed %v: posted %+v, want %d request(s) for session-bead-a at generation 7", routed, posted, want)
		}
	}
}

// Kills v2's claim backstop sending nothing to a rig-scoped pool that
// configures no nudge (mc-zndi7.80, the production-dead case): the nudges
// step delivers the default claim nudge after the grace.
func TestNudgeStepsNudgeARigPoolWithoutNudge(t *testing.T) {
	sess := beads.Bead{ID: "gcg-session-2", Status: "open", Type: "session", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "beads--claude-max-gw-2-pool", "pool_managed": "true", "template": "beads/claude-max-gw", "agent_name": "beads/claude-max-gw-2",
		testTriggerBeadIDKey: "gcg-step", idleClaimNudgeTriggerKey: "gcg-step", idleClaimNudgeCountKey: "0",
		idleClaimNudgeAtKey: stepsTestNow.Add(-idleClaimNudgeGrace - time.Second).Format(time.RFC3339),
	}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{sess}, nil)
	sp := runningIdleClaimFake(t, "beads--claude-max-gw-2-pool")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "c"},
		Rigs:      []config.Rig{{Name: "beads", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "claude-max-gw", Dir: "beads", MaxActiveSessions: intPtr(100)}},
	}
	env := externalReadsEnv{Cfg: cfg, CityStore: store, SP: sp}
	sum := &allocSummary{At: stepsTestNow, ReadyRouted: []beads.Bead{{ID: "gcg-step", Status: "open"}}, ReadyRoutedRefs: []string{""}}
	lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, nil)
	lane.nudges(context.Background(), env)
	if got := sp.CountCalls("Nudge", "beads--claude-max-gw-2-pool"); got != 1 {
		t.Fatalf("nudges = %d, want the default claim nudge delivered", got)
	}
}
