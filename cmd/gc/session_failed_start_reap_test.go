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
	if n := reapStaleSessionBeads(env.city, env.store, env.sp, env.dt, nil, env.clk, &log); n != 0 {
		t.Fatalf("the next tick's reap closed %d row(s) after one failed start: %s", n, log.String())
	}
	delete(env.sp.StartErrors, "worker")
	if woken := env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)}); woken != 1 || !env.sp.IsRunning("worker") {
		t.Fatalf("the next pass woke %d, running %v; want the session relaunched\nstderr %s", woken, env.sp.IsRunning("worker"), env.stderr.String())
	}
}
