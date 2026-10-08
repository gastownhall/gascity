package main

import (
	"context"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// stepsTestNow is the steps tests' pinned lane clock.
var stepsTestNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// casMemStore is a MemStore stamped so its conditional writer resolves, with
// a hook before each CAS and a count of its blind metadata writes.
type casMemStore struct {
	*beads.MemStore
	beforeCAS   func()
	blindWrites int
}

func newCASMemStore(t *testing.T) *casMemStore {
	t.Helper()
	s := &casMemStore{MemStore: beads.NewMemStore()}
	if err := beads.StampOpenedStore(s.MemStore, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	return s
}

func (s *casMemStore) UpdateIfMatch(id string, rev int64, o beads.UpdateOpts) error {
	if hook := s.beforeCAS; hook != nil {
		hook()
	}
	return s.MemStore.UpdateIfMatch(id, rev, o)
}

func (s *casMemStore) SetMetadataBatch(id string, kv map[string]string) error {
	s.blindWrites++
	return s.MemStore.SetMetadataBatch(id, kv)
}

func (s *casMemStore) Update(id string, o beads.UpdateOpts) error {
	s.blindWrites++
	return s.MemStore.Update(id, o)
}

// newStepsTestLane is a lane over env at stepsTestNow with the waits step,
// and a count of its planner wakes.
func newStepsTestLane(env externalReadsEnv) (*externalReadsLane, *atomic.Int64) {
	lane, wakes := newTestBackstopLane(env)
	lane.now = func() time.Time { return stepsTestNow }
	lane.addWaitsStep()
	return lane, wakes
}

// createWaitTestSession creates a session row on store with meta, and a deps
// wait on it watching depID in state.
func createWaitTestSession(t *testing.T, store beads.Store, depID, state string, meta map[string]string) beads.Bead {
	t.Helper()
	md := map[string]string{"session_name": "worker", "agent_name": "worker", "continuation_epoch": "1"}
	maps.Copy(md, meta)
	sess, err := store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: md})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if depID == "" {
		return sess
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sess.ID},
		Metadata: map[string]string{
			"session_id": sess.ID, "session_name": "worker", "kind": "deps", "state": state,
			"dep_ids": depID, "dep_mode": "all", "registered_epoch": "1", "delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait: %v", err)
	}
	return sess
}

func waitsStepEnv(t *testing.T, store beads.Store) externalReadsEnv {
	cfg := &config.City{}
	cfg.Daemon.NudgeDispatcher = "supervisor" // no nudge poller process in a test
	return externalReadsEnv{CityPath: t.TempDir(), Cfg: cfg, CityStore: store, Nudges: beads.NudgesStore{Store: store}}
}

// Kills: wait-ready never waking, a blind wait-hold clear from the step, and
// a dependency index that misses a pending wait. The waits step marks a deps
// wait whose dependency closed ready and publishes its session in the
// ready-wait set with one planner wake, which gather reads into the World the
// allocation takes as ReadyWaits; it clears an expired wait's hold by CAS
// only; and it indexes the open dependency of the wait it left pending.
func TestWaitsStepPublishesReadyWaits(t *testing.T) {
	store, _ := newWaitClearOracle(t)
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep: %v", err)
	}
	sess := createWaitTestSession(t, store.MemStore, dep.ID, waitStatePending, nil)
	open, err := store.Create(beads.Bead{Title: "open dep"})
	if err != nil {
		t.Fatalf("create open dep: %v", err)
	}
	createWaitTestSession(t, store.MemStore, open.ID, waitStatePending, nil)
	env := waitsStepEnv(t, store)
	lane, wakes := newStepsTestLane(env)

	lane.waits(context.Background(), env)
	if got := lane.readyWaitSet(); !got[sess.ID] || len(got) != 1 {
		t.Fatalf("ready waits = %v, want only %s", got, sess.ID)
	}
	for _, line := range store.log {
		if !strings.HasPrefix(line, "cas ") {
			t.Errorf("the step wrote a held session blind: %s", line)
		}
	}
	if len(store.log) != 3 {
		t.Errorf("held-session writes = %q, want one clear per held session", store.log)
	}
	if deps := *lane.waitDeps.Load(); !deps[open.ID] || deps[dep.ID] {
		t.Errorf("wait dependency index = %v, want %s and not the closed %s", deps, open.ID, dep.ID)
	}
	if wakes.Load() != 1 {
		t.Fatalf("planner wakes = %d, want 1 for the new ready set", wakes.Load())
	}
	lane.waits(context.Background(), env)
	if wakes.Load() != 1 {
		t.Errorf("planner wakes = %d after an unchanged run, want still 1", wakes.Load())
	}

	f := newGatherFixture(t)
	f.env.ReadyWaits = lane.readyWaitSet
	if w := f.gather(t); !w.ReadyWaits[sess.ID] {
		t.Fatalf("World.ReadyWaits = %v, want %s", w.ReadyWaits, sess.ID)
	}
}

// Kills: a blind or widened wait-hold clear (CONTRACT v5.5 R1 wait-hold owner,
// C8 ruling). The clear writes only by CAS, only once no wait is left, and
// clears sleep_intent and sleep_reason only while they still read wait-hold;
// a wait registered between its read and its CAS makes it re-decide and
// write nothing; a store with no conditional writer is refused.
func TestWaitHoldClearFenced(t *testing.T) {
	held := map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold"}
	for _, tc := range []struct {
		name        string
		meta        map[string]string
		pendingWait bool
		race        bool
		want        map[string]string
	}{
		{name: "idle", meta: held, want: map[string]string{"wait_hold": "", "sleep_intent": "", "sleep_reason": ""}},
		{name: "a wait remains", meta: held, pendingWait: true, want: held},
		{
			name: "operator hold kept", meta: map[string]string{"wait_hold": "true", "sleep_intent": "user-hold", "sleep_reason": "user-hold"},
			want: map[string]string{"wait_hold": "", "sleep_intent": "user-hold", "sleep_reason": "user-hold"},
		},
		{
			name: "not yet asleep", meta: map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"},
			want: map[string]string{"wait_hold": "", "sleep_intent": "", "sleep_reason": ""},
		},
		{name: "wait registered before the CAS", meta: held, race: true, want: held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newCASMemStore(t)
			dep := ""
			if tc.pendingWait {
				dep = "dep-open"
			}
			sess := createWaitTestSession(t, store, dep, waitStatePending, tc.meta)
			if tc.race {
				store.beforeCAS = func() {
					store.beforeCAS = nil
					if _, err := store.Create(beads.Bead{
						Type: waitBeadType, Labels: []string{waitBeadLabel, "session:" + sess.ID},
						Metadata: map[string]string{"session_id": sess.ID, "state": waitStatePending},
					}); err != nil {
						t.Fatalf("racing wait: %v", err)
					}
					if err := store.SetMetadata(sess.ID, "wait_hold", "true"); err != nil {
						t.Fatalf("racing hold: %v", err)
					}
				}
			}
			if err := clearSessionWaitHoldFenced(sessionFrontDoor(store), sess.ID); err != nil {
				t.Fatalf("clear: %v", err)
			}
			got := mustGetTestBead(t, store, sess.ID)
			for k, v := range tc.want {
				if got.Metadata[k] != v {
					t.Errorf("%s = %q, want %q", k, got.Metadata[k], v)
				}
			}
			if store.blindWrites != 0 {
				t.Errorf("blind writes = %d, want 0", store.blindWrites)
			}
		})
	}

	plain := beads.NewMemStore()
	sess := createWaitTestSession(t, plain, "", "", held)
	if err := clearSessionWaitHoldFenced(sessionFrontDoor(plain), sess.ID); err == nil {
		t.Fatal("clear on a store with no conditional writer: want a refusal")
	}
	if got := mustGetTestBead(t, plain, sess.ID); got.Metadata["wait_hold"] != "true" {
		t.Errorf("unfenced store: wait_hold = %q, want untouched", got.Metadata["wait_hold"])
	}
}

// waitWriteLog is a CAS-capable MemStore that logs every metadata write to
// the rows in watch, so a run's session-row writes compare exactly.
type waitWriteLog struct {
	*beads.MemStore
	watch map[string]bool
	log   []string
}

func (s *waitWriteLog) add(kind, id string, kv map[string]string) {
	if !s.watch[id] {
		return
	}
	keys := slices.Sorted(maps.Keys(kv))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+kv[k])
	}
	s.log = append(s.log, kind+" "+id+" "+strings.Join(parts, ","))
}

func (s *waitWriteLog) SetMetadataBatch(id string, kv map[string]string) error {
	s.add("batch", id, kv)
	return s.MemStore.SetMetadataBatch(id, kv)
}

func (s *waitWriteLog) Update(id string, o beads.UpdateOpts) error {
	s.add("update", id, o.Metadata)
	return s.MemStore.Update(id, o)
}

func (s *waitWriteLog) UpdateIfMatch(id string, rev int64, o beads.UpdateOpts) error {
	s.add("cas", id, o.Metadata)
	return s.MemStore.UpdateIfMatch(id, rev, o)
}

// newWaitClearOracle builds three held sessions, one per terminal wait path
// that clears the hold: a stale continuation epoch, an expired wait and a
// missing dependency. The store's conditional writer resolves, so a legacy
// run that took the fenced clear would show.
func newWaitClearOracle(t *testing.T) (*waitWriteLog, []string) {
	t.Helper()
	s := &waitWriteLog{MemStore: beads.NewMemStore(), watch: map[string]bool{}}
	if err := beads.StampOpenedStore(s.MemStore, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	held := map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold"}
	var ids []string
	for _, wait := range []map[string]string{
		{"kind": "deps", "registered_epoch": "0", "dep_ids": "x"},
		{"kind": "deps", "registered_epoch": "1", "dep_ids": "x", "expires_at": stepsTestNow.Add(-time.Minute).Format(time.RFC3339)},
		{"kind": "deps", "registered_epoch": "1", "dep_ids": "missing-dep"},
	} {
		sess := createWaitTestSession(t, s.MemStore, "", "", held)
		md := map[string]string{"session_id": sess.ID, "session_name": "worker", "state": waitStatePending, "dep_mode": "all", "delivery_attempt": "1"}
		maps.Copy(md, wait)
		if _, err := s.Create(beads.Bead{Type: waitBeadType, Labels: []string{waitBeadLabel, "session:" + sess.ID}, Metadata: md}); err != nil {
			t.Fatalf("create wait: %v", err)
		}
		s.watch[sess.ID] = true
		ids = append(ids, sess.ID)
	}
	return s, ids
}

// Kills: the v2 seam changing legacy (C8 ruling A). Legacy's entry point
// keeps clearSessionWaitHoldIfIdle on every clear path: one blind batch of
// exactly wait_hold, sleep_intent and sleep_reason per session, as before
// the seam, even on a store that could fence. The v2 hooks write the same
// keys by CAS and nothing blind.
func TestPrepareWaitWakeStateLegacyClearOracle(t *testing.T) {
	deps := func(s beads.Store) waitDependencyReader {
		return waitDependencyReaderFunc(func(id string) (beads.Bead, error) { return loadWaitDependencyBead("", s, id) })
	}
	legacy, ids := newWaitClearOracle(t)
	if _, err := prepareWaitWakeStateWithSnapshot(sessionFrontDoor(legacy), deps(legacy), beads.NudgesStore{Store: legacy}, stepsTestNow, nil); err != nil {
		t.Fatalf("legacy prepare: %v", err)
	}
	var want []string
	for _, id := range ids {
		want = append(want, "batch "+id+" sleep_intent=,sleep_reason=,wait_hold=")
	}
	sort.Strings(legacy.log)
	if !slices.Equal(legacy.log, want) {
		t.Fatalf("legacy session-row writes:\n got %q\nwant %q", legacy.log, want)
	}

	v2, ids := newWaitClearOracle(t)
	if _, err := prepareWaitWakeStateHooked(sessionFrontDoor(v2), deps(v2), beads.NudgesStore{Store: v2}, stepsTestNow, nil, waitWakeHooks{clearHold: clearSessionWaitHoldFenced}); err != nil {
		t.Fatalf("v2 prepare: %v", err)
	}
	want = want[:0]
	for _, id := range ids {
		want = append(want, "cas "+id+" sleep_intent=,sleep_reason=,wait_hold=")
	}
	sort.Strings(v2.log)
	if !slices.Equal(v2.log, want) {
		t.Fatalf("v2 session-row writes:\n got %q\nwant %q", v2.log, want)
	}
}

// Kills GUAR-010 misses: a dependency close that waits a patrol for the
// waits step, or that re-reads the legs. Closing a pending wait's dependency
// runs the waits step alone, with no read and no publish; an update to it,
// or a close of anything else, runs nothing.
func TestDependencyCloseTriggersStepsOnlyRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		lane.addWaitsStep()
		runs := map[string]*atomic.Int64{}
		for _, step := range lane.steps {
			n := &atomic.Int64{}
			runs[step.name] = n
			step.run = func(context.Context, externalReadsEnv) { n.Add(1) }
		}
		lane.waitDeps.Store(&map[string]bool{"dep-1": true})
		p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
		wake := &controllerWake{planner: p, waitDepClosed: lane.dependencyClosed}
		startBackstopLaneInBubble(t, lane)
		seq := backstopSeq(lane)
		advanceBackstop(externalReadsMinGap + time.Second)

		event := func(typ, id string) events.Event {
			payload, err := beads.EncodeBeadEventPayload(beads.Bead{ID: id})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			return events.Event{Type: typ, Subject: id, Payload: payload}
		}
		wake.OnBeadEvent(event(events.BeadUpdated, "dep-1"), false)
		wake.OnBeadEvent(event(events.BeadClosed, "other"), false)
		synctest.Wait()
		if runs["waits"].Load() != 1 {
			t.Fatalf("waits runs = %d after unrelated events, want only the first pass's", runs["waits"].Load())
		}
		wake.OnBeadEvent(event(events.BeadClosed, "dep-1"), false)
		synctest.Wait()
		if runs["waits"].Load() != 2 {
			t.Fatalf("waits runs = %d after the dependency closed, want 2", runs["waits"].Load())
		}
		if runs["demand-repairs"].Load() != 1 || backstopSeq(lane) != seq {
			t.Errorf("steps-only run: repairs=%d seq %d→%d, want no other step, read or publish", runs["demand-repairs"].Load(), seq, backstopSeq(lane))
		}
	})
}
