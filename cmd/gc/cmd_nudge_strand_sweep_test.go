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
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// TestPruneStrandedQueuedNudgesDrainTimeRaceIsDeadLettered pins half (a2) of the
// sc-875uie loud-fail guard end to end: a managed nudge queued to a live/asleep
// session that THEN drains before consuming it is dead-lettered by the maintenance
// sweep and becomes VISIBLE in `gc nudge status` (Dead: 1), instead of lingering in
// Pending until its retired fence stops matching and status reads 0/0/0 with zero
// trace. The send-time guard (half a) cannot catch this race — the target was
// perfectly resumable when the nudge was queued; only a later observation over the
// now-terminal session lifecycle can. The same observation while the target is
// still asleep must NOT sweep the nudge (no false positive on a resumable target).
func TestPruneStrandedQueuedNudgesDrainTimeRaceIsDeadLettered(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Asleep (suspended) — a genuinely resumable target. A managed wake queued here
	// is legitimate: the session would consume it on resume. This is the pre-race
	// state; the drain happens AFTER the nudge is already queued.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	prevManaged := nudgeCityUsesManagedReconciler
	prevPoke := nudgePokeController
	prevObserve := nudgeObserveTarget
	pokes := 0
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(string) error {
		pokes++
		return nil
	}
	nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
		return worker.LiveObservation{Running: false}, nil
	}
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler = prevManaged
		nudgePokeController = prevPoke
		nudgeObserveTarget = prevObserve
	})

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}

	// Queue the managed wake to the asleep session — the normal, successful path.
	var stdout, stderr bytes.Buffer
	if code := deliverSessionNudgeWithWorker(target, store, fake, "resume: mayor ruling", nudgeDeliveryImmediate, false, &stdout, &stderr); code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d (%q), want 0 (queued to a resumable session)", code, stderr.String())
	}
	if pokes != 1 {
		t.Fatalf("pokes = %d, want 1 (managed wake enqueued + controller poked)", pokes)
	}

	// Observation #1, target still ASLEEP: the sweep runs but must NOT touch a
	// resumable target — the nudge stays Pending.
	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget (asleep): %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("asleep pending/inFlight/dead = %d/%d/%d, want 1/0/0 (no false sweep of a resumable target)", len(pending), len(inFlight), len(dead))
	}
	if pending[0].SessionID != info.ID {
		t.Fatalf("queued nudge SessionID = %q, want %q (item must carry its target session for the sweep to classify it)", pending[0].SessionID, info.ID)
	}

	// The race: the target DRAINS after the nudge was queued. A drained session
	// projects to BaseStateDrained — the reconciler spawns a fresh worker rather
	// than resuming it, so nothing will ever consume the queued wake.
	if err := store.SetMetadata(info.ID, "state", "drained"); err != nil {
		t.Fatalf("SetMetadata(state=drained): %v", err)
	}

	// Observation #2, target now DRAINED: the sweep dead-letters the orphan. This
	// is the fix — status now shows a real Dead trace instead of 0/0/0.
	pending, inFlight, dead, err = listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget (drained): %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
		t.Fatalf("drained pending/inFlight/dead = %d/%d/%d, want 0/0/1 (queued-then-drained nudge dead-lettered, not evaporated)", len(pending), len(inFlight), len(dead))
	}
	if !strings.Contains(dead[0].LastError, "drained") {
		t.Fatalf("dead LastError = %q, want it to name the terminal session state (drained)", dead[0].LastError)
	}
	if dead[0].DeadAt.IsZero() {
		t.Fatalf("dead nudge DeadAt is zero, want it stamped at sweep time")
	}

	// Idempotent: a subsequent observation does not re-sweep or duplicate the Dead
	// entry — the item is already terminal.
	pending, inFlight, dead, err = listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget (re-observe): %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
		t.Fatalf("re-observe pending/inFlight/dead = %d/%d/%d, want 0/0/1 (sweep is idempotent)", len(pending), len(inFlight), len(dead))
	}
}
