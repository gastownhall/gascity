package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newDrainingStopHandle builds a handle over a session bead parked in
// state=draining with a live runtime, which is the seat class both stop intents
// disagree about.
func newDrainingStopHandle(t *testing.T) (*SessionHandle, *runtime.Fake, string, *beads.MemStore, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Draining",
		Command:  "true",
		WorkDir:  t.TempDir(),
		Provider: "legacy-provider",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := bead.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	if err := store.SetMetadata(info.ID, "state", string(sessionpkg.StateActive)); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if err := manager.BeginDrain(info.ID, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{ID: info.ID, Template: "worker", Command: "true", Provider: "legacy-provider"},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle, sp, sessName, store, info.ID
}

// The draining latitude belongs to the city stop/restart SWEEP and must not be
// reachable from a targeted operator path.
//
// Keying the intent on the METHOD Stop leaked it: Stop has targeted callers —
// `gc session suspend <id>`'s controller-down fallback, idle auto-suspend, and
// the legacy API mux — so a draining seat could be torn down with none of the
// gates the reconciler's escalation insists on for the same class of kill (no
// assigned-work probe, no instance-token fence, no confirm-dead), and the CLI
// would print "suspended... Resume with: gc session wake" while the bead stayed
// draining and wake could not restore it.
func TestSessionHandleStopKeepsOperatorIntentOnDrainingSeat(t *testing.T) {
	handle, sp, sessName, store, id := newDrainingStopHandle(t)

	err := handle.Stop(context.Background())
	if !errors.Is(err, sessionpkg.ErrIllegalTransition) {
		t.Fatalf("Stop(draining) = %v, want ErrIllegalTransition — the operator seam must not carry shutdown latitude", err)
	}
	if sp.CountCalls("Stop", sessName) != 0 {
		t.Errorf("operator Stop tore down the runtime %q anyway", sessName)
	}
	if !sp.IsRunning(sessName) {
		t.Errorf("runtime %q was killed by a rejected operator stop", sessName)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if got := bead.Metadata["state"]; got != string(sessionpkg.StateDraining) {
		t.Errorf("state = %q, want draining", got)
	}
}

// The control: the sweep's own seam does carry the latitude, so `gc stop` and
// `gc restart` stop skipping draining seats and leaving them alive holding their
// pool slot names.
func TestSessionHandleStopForShutdownTearsDownDrainingSeat(t *testing.T) {
	handle, sp, sessName, store, id := newDrainingStopHandle(t)

	if err := handle.StopForShutdown(context.Background()); err != nil {
		t.Fatalf("StopForShutdown(draining) = %v, want nil", err)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime %q still running after the shutdown sweep", sessName)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	// The early return writes no state: a drain is not a suspension.
	if got := bead.Metadata["state"]; got != string(sessionpkg.StateDraining) {
		t.Errorf("state = %q, want draining (the drain must survive the stop)", got)
	}
}
