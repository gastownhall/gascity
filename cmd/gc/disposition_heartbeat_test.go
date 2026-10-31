package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// operatorSuspendFront is an operator's suspend of id through store's front
// door.
func operatorSuspendFront(store beads.Store, id string) error {
	_, err := sessionFrontDoor(store).OperatorSuspend(id, time.Now())
	return err
}

// TestHeartbeatOnABareWaitRowKeepsItAlive is the max review's H1: a heartbeat on a live row that still carries a bare wait_hold (a woken
// waiter whose drain completion cleared sleep_intent; CONTRACT D8 7(a)).
// Before R7b1 the heartbeat wrote held_until alone and SESS-617's keep-alive
// held the session up (#3994).
func TestHeartbeatOnABareWaitRowKeepsItAlive(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"wait_hold": "true"})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	if code := doRuntimeHeartbeat(env.store, 45*time.Minute, "worker", "worker", false, &so, &se); code != 0 {
		t.Fatalf("heartbeat exit %d: %s", code, se.String())
	}
	row, _ := env.store.Get(session.ID)
	t.Logf("after the heartbeat: sleep_intent=%q wait_hold=%q held_until=%q", row.Metadata["sleep_intent"], row.Metadata["wait_hold"], row.Metadata["held_until"])
	it := newFakeIdleTracker()
	it.idle["worker"] = true
	runTick := func() {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		reconcileSessionBeadsAtPath(
			context.Background(), env.city, []beads.Bead{got}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
			env.cfg, env.sp, env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
			it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
			withAsyncDrainAckStopTracker(env.stops),
		)
	}
	runTick()
	if ds := env.dt.get(session.ID); ds != nil {
		t.Logf("tick 1 began a drain: reason=%q", ds.reason)
	}
	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Minute)
	runTick()
	runTick()
	row, _ = env.store.Get(session.ID)
	t.Logf("after the drain deadline: running=%v state=%q sleep_reason=%q", env.sp.IsRunning("worker"), row.Metadata["state"], row.Metadata["sleep_reason"])
	if !env.sp.IsRunning("worker") || env.dt.get(session.ID) != nil {
		t.Errorf("the heartbeat-held live session was drained")
	}
}

type updateHookStore struct {
	beads.Store
	hook func()
	done bool
}

func (s *updateHookStore) Update(id string, opts beads.UpdateOpts) error {
	if !s.done && s.hook != nil {
		s.done = true
		s.hook()
	}
	return s.Store.Update(id, opts)
}

// TestHeartbeatRacingASuspendWithoutCASKeepsIt is the max review's H3: on a store with no conditional writes (maintainer-city, D-I), an
// operator suspend lands between the heartbeat's read and its write.
func TestHeartbeatRacingASuspendWithoutCASKeepsIt(t *testing.T) {
	const sessionName, beadID = "testpack__beater", "bead-789"
	mem := beads.NewMemStoreFrom(0, []beads.Bead{makeSessionBead(beadID, sessionName)}, nil)
	if err := mem.SetMetadata(beadID, "state", "active"); err != nil {
		t.Fatal(err)
	}
	w := &updateHookStore{Store: mem}
	w.hook = func() {
		if err := operatorSuspendFront(mem, beadID); err != nil {
			t.Fatal(err)
		}
	}
	var so, se bytes.Buffer
	code := doRuntimeHeartbeat(w, time.Hour, sessionName, sessionName, false, &so, &se)
	b, _ := mem.Get(beadID)
	t.Logf("exit %d state=%q sleep_intent=%q held_until=%q suspended_at=%q (hook fired: %v)", code, b.Metadata["state"], b.Metadata["sleep_intent"], b.Metadata["held_until"], b.Metadata["suspended_at"], w.done)
	if b.Metadata["sleep_intent"] != "user-hold" {
		t.Errorf("the heartbeat's blind write erased the operator's suspend")
	}
}
