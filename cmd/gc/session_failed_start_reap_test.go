package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestOneFailedStartDoesNotLoseTheSession is mc-ydyvj (found in #7565's max
// review): an operator's (claimless) session whose restart fails once with an
// ordinary provider error must not be left creating with its in-flight marker
// cleared, where the next tick's stuck-creating reap (which runs before the
// heal) ages it from CreatedAt and closes it as stale-session. A failed start
// undoes its PreWake, as a refused one does: the row is open and asleep, its
// wake failure accrued, and the next pass relaunches it.
func TestOneFailedStartDoesNotLoseTheSession(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.clk.Time = time.Now().UTC().Add(2 * time.Minute) // the row was created 2m ago
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	row := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&row, map[string]string{"session_origin": "manual", "state": "asleep"})
	env.sp.StartErrors["worker"] = errors.New("provider boom")
	if woken := env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)}); woken != 0 {
		t.Fatalf("the failing start woke %d, want 0", woken)
	}
	failed := mustGetBead(t, env.store, row.ID).Metadata
	if failed["state"] != "asleep" || failed["last_woke_at"] != "" || failed["wake_attempts"] != "1" {
		t.Fatalf("row after a failed start = %v, want PreWake undone (asleep, no marker) and the wake failure accrued", failed)
	}
	env.clk.Advance(5 * time.Second)
	var log bytes.Buffer
	if n := reapStaleSessionBeads(env.city, env.store, testSeatWork(env.city, env.cfg, env.store, nil), env.sp, env.dt, nil, env.clk, &log); n != 0 {
		t.Fatalf("the next tick's reap closed %d row(s) after one failed start: %s", n, log.String())
	}
	delete(env.sp.StartErrors, "worker")
	if woken := env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)}); woken != 1 || !env.sp.IsRunning("worker") {
		t.Fatalf("the next pass woke %d, running %v; want the session relaunched\nstderr %s", woken, env.sp.IsRunning("worker"), env.stderr.String())
	}
}

// TestFailedRelaunchOfAnAbandonedStartSurvivesTheReap (#7570 review): a
// restart a crashed controller abandoned (creating, a fresh marker, no
// holder) is relaunched (#7565), and the relaunch fails once with an
// ordinary provider error. PreWake found the row creating, so restoring it
// would leave it creating with no marker; the attempt's marker stays, and the
// next tick's reap, which runs before the heal, keeps the row.
func TestFailedRelaunchOfAnAbandonedStartSurvivesTheReap(t *testing.T) {
	env, row := abandonedRestart(t)
	env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
	m := mustGetBead(t, env.store, row.ID).Metadata
	if env.sp.CountCalls("Start", "worker") != 1 || m["generation"] != "3" || m["last_woke_at"] == "" || m["wake_attempts"] != "1" {
		t.Fatalf("row after the failed relaunch = %v (starts %d), want the relaunch's marker kept and its wake failure accrued", m, env.sp.CountCalls("Start", "worker"))
	}
	env.clk.Advance(5 * time.Second)
	var log bytes.Buffer
	if n := reapStaleSessionBeads(env.city, env.store, testSeatWork(env.city, env.cfg, env.store, nil), env.sp, env.dt, nil, env.clk, &log); n != 0 {
		t.Fatalf("a failed relaunch of an abandoned start lost the session: %s", log.String())
	}
}

// TestAbandonedStartCrashLoopQuarantines (#7570 review): an abandoned start
// whose every relaunch fails is never reaped, and its wake failures
// quarantine it by the fifth provider start.
func TestAbandonedStartCrashLoopQuarantines(t *testing.T) {
	env, row := abandonedRestart(t)
	var trail []string
	for tick := 0; tick < 12; tick++ {
		var log bytes.Buffer
		reapStaleSessionBeads(env.city, env.store, testSeatWork(env.city, env.cfg, env.store, nil), env.sp, env.dt, nil, env.clk, &log)
		if mustGetBead(t, env.store, row.ID).Status == "closed" {
			t.Fatalf("reaped during the relaunch crash loop at tick %d: %v %s", tick, trail, log.String())
		}
		env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
		m := mustGetBead(t, env.store, row.ID).Metadata
		trail = append(trail, "wa="+m["wake_attempts"]+",st="+m["state"]+",q="+m["quarantined_until"])
		env.clk.Advance(10 * time.Second)
	}
	m := mustGetBead(t, env.store, row.ID).Metadata
	if starts := env.sp.CountCalls("Start", "worker"); m["quarantined_until"] == "" || starts > 5 {
		t.Fatalf("quarantined_until=%q after %d provider starts, want quarantine by 5: %v", m["quarantined_until"], starts, trail)
	}
}

// abandonedRestart is an operator's row a crashed controller left between
// PreWake and Start (creating, a fresh marker, no holder), created 2m ago,
// whose every start fails with an ordinary provider error.
func abandonedRestart(t *testing.T) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	now := time.Now().UTC().Add(2 * time.Minute)
	env := newReconcilerTestEnv(t)
	env.clk.Time = now
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	row := env.createSessionBead("worker", "worker")
	woke := now.Add(-10 * time.Second).Format(time.RFC3339)
	env.setSessionMetadata(&row, map[string]string{
		"state": "creating", "session_origin": "manual", "last_woke_at": woke, "pending_create_started_at": woke,
		"generation": "2", "instance_token": "T1",
	})
	env.sp.StartErrors["worker"] = errors.New("provider boom")
	return env, row
}
