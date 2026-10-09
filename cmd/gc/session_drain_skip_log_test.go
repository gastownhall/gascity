package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A seat that legitimately holds a drain skip (live assigned work, a partial
// store query) re-reaches the same skip every reconcile tick. The skip line
// must print when the skip starts or changes, not on every tick; a tick that
// sees the session without the skip re-arms it so the next episode prints.
func TestDrainSkipLineLogsOnTransition(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	env := newReconcilerTestEnv()
	// "worker" is neither desired nor configured-named, so a live runtime
	// falls to the orphan drain, which a partial store query skips.
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	// Past the INC-003 wake grace, so the healthy pass below drains rather than
	// deferring a freshly woken orphan.
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-wakeUndesiredGrace - time.Minute).UTC().Format(time.RFC3339),
	})

	tick := func(sessions []beads.Bead, partial bool) {
		t.Helper()
		reconcileSessionBeads(
			context.Background(), sessions, env.desiredState, map[string]bool{}, env.cfg, env.sp, env.store,
			nil, nil, nil, env.dt, nil, partial, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		)
		env.clk.Advance(30 * time.Second)
	}
	const partialLine = "Skipping drain for 'worker': store query partial (transient failure)"
	count := func() int { return strings.Count(env.stdout.String(), partialLine) }

	for range 3 {
		tick([]beads.Bead{session}, true)
	}
	if got := count(); got != 1 {
		t.Fatalf("standing skip printed %d times over 3 ticks, want 1:\n%s", got, env.stdout.String())
	}

	// A pass whose feed omits the session (the control-dispatcher tick
	// reconciles a filtered feed) is not evidence the skip ended.
	tick(nil, true)
	tick([]beads.Bead{session}, true)
	if got := count(); got != 1 {
		t.Fatalf("skip reprinted after a pass that did not see the session: %d prints:\n%s", got, env.stdout.String())
	}

	// The skip reason changing (the store recovers but the seat holds live
	// work) is a transition and prints once.
	task := createInProgressTaskWithWorkDir(t, env.store, session.ID, t.TempDir())
	tick([]beads.Bead{session}, false)
	tick([]beads.Bead{session}, false)
	if got := strings.Count(env.stdout.String(), "Skipping drain for 'worker': live assigned work found"); got != 1 {
		t.Fatalf("live-work skip printed %d times over 2 ticks, want 1:\n%s", got, env.stdout.String())
	}
	if err := env.store.Close(task.ID); err != nil {
		t.Fatalf("Close(task): %v", err)
	}

	// A pass that sees the session without the skip ends the episode (here
	// the store recovers and the orphan drain proceeds); a later skip is a
	// new episode and prints again.
	tick([]beads.Bead{session}, false)
	if !strings.Contains(env.stdout.String(), "Draining session 'worker'") {
		t.Fatalf("healthy pass did not drain the orphan; test premise broken:\n%s", env.stdout.String())
	}
	tick([]beads.Bead{session}, true)
	if got := count(); got != 2 {
		t.Fatalf("skip after a clean pass printed %d times total, want 2:\n%s", got, env.stdout.String())
	}
}

func TestDrainSkipLineReprintsWhenTheReasonChanges(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	dt := newDrainTracker()
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer

	logDrainSkip(dt, &out, "s-1", "skip: partial", now)
	logDrainSkip(dt, &out, "s-1", "skip: partial", now)
	logDrainSkip(dt, &out, "s-1", "skip: live work", now)
	logDrainSkip(dt, &out, "s-2", "skip: partial", now)
	want := "skip: partial\nskip: live work\nskip: partial\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestDrainSkipLineEveryTickUnderGCDebug(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	dt := newDrainTracker()
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	for range 3 {
		logDrainSkip(dt, &out, "s-1", "skip: partial", now)
	}
	if got := strings.Count(out.String(), "skip: partial"); got != 3 {
		t.Fatalf("GC_DEBUG printed %d lines, want 3", got)
	}
}

func TestSweepDrainSkipsPrunesAbsentSessionsAfterRetention(t *testing.T) {
	dt := newDrainTracker()
	start := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	dt.noteDrainSkip("gone", "skip", start)
	dt.sweepDrainSkips(map[string]sessionpkg.Info{}, start) // consumes this pass's sighting

	dt.sweepDrainSkips(map[string]sessionpkg.Info{}, start.Add(drainSkipAbsentRetention))
	if dt.noteDrainSkip("gone", "skip", start.Add(drainSkipAbsentRetention)) {
		t.Fatal("mark for an absent session dropped before the retention elapsed")
	}
	dt.sweepDrainSkips(map[string]sessionpkg.Info{}, start.Add(drainSkipAbsentRetention))

	later := start.Add(3 * drainSkipAbsentRetention)
	dt.sweepDrainSkips(map[string]sessionpkg.Info{}, later)
	if n := len(dt.drainSkips); n != 0 {
		t.Fatalf("drainSkips holds %d marks after the retention elapsed, want 0", n)
	}
}

func TestDrainSkipNilTrackerAlwaysPrints(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	var dt *drainTracker
	var out bytes.Buffer
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	logDrainSkip(dt, &out, "s-1", "skip", now)
	logDrainSkip(dt, &out, "s-1", "skip", now)
	dt.sweepDrainSkips(nil, now)
	if got := strings.Count(out.String(), "skip"); got != 2 {
		t.Fatalf("nil tracker printed %d lines, want 2", got)
	}
}
