package session

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// reproSession creates a real session through the Manager so the reproduction
// exercises the same write path an operator's city does.
func reproSession(t *testing.T) (*Manager, beads.Store, string) {
	t.Helper()
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake())
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "polecat", Title: "repro", Command: "claude",
		WorkDir: "/tmp", Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return mgr, store, info.ID
}

// TestSuspendThenWakeIsSingleVoiced reproduces the defect end to end, exactly as
// the upstream repro steps describe it: suspend a session, ask for an explicit
// wake, read the bead back.
func TestSuspendThenWakeIsSingleVoiced(t *testing.T) {
	mgr, store, id := reproSession(t)

	if err := mgr.Suspend(id); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	suspended, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after suspend: %v", err)
	}
	if suspended.Metadata["state"] != string(StateSuspended) {
		t.Fatalf("precondition: state = %q, want suspended", suspended.Metadata["state"])
	}
	if suspended.Metadata["suspended_at"] == "" {
		t.Fatal("precondition: suspend did not stamp suspended_at")
	}

	if _, err := NewStore(beads.SessionStore{Store: store}).
		WakeSession(id, time.Now().UTC(), WakeOpts{}); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	woken, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after wake: %v", err)
	}
	md := woken.Metadata
	if md["state"] != string(StateAsleep) {
		t.Fatalf("precondition: state = %q, want asleep (the re-projection under test)", md["state"])
	}

	// (a) the stamp from the state it is no longer in must not survive.
	if md["suspended_at"] != "" {
		t.Errorf("suspended_at = %q, want cleared: the bead is asleep, not suspended", md["suspended_at"])
	}
	// (b) an asleep bead must carry the asleep vocabulary.
	if md["slept_at"] == "" {
		t.Errorf("slept_at is absent on an asleep bead: nothing stamps it on the suspended->asleep re-projection")
	}
	t.Logf("post-wake record: state=%q suspended_at=%q slept_at=%q sleep_reason=%q state_reason=%q wake_request=%q wake_requested_at=%q wake_attempts=%q",
		md["state"], md["suspended_at"], md["slept_at"], md["sleep_reason"],
		md["state_reason"], md["wake_request"], md["wake_requested_at"], md["wake_attempts"])
}

// TestAsleepWithoutSleptAtIsUnprunable is the operational consequence: the prune
// classifier keys the asleep lane on slept_at, and a bead with neither slept_at
// nor sleep_reason=drained is reported unprunable, so the session never leaves
// the lane by the ordinary cleanup path.
func TestAsleepWithoutSleptAtIsUnprunable(t *testing.T) {
	woken := sessionBeadFixture("s-woken", "open", map[string]string{
		"state":        string(StateAsleep),
		"suspended_at": waitStoreNow.Add(-8 * time.Minute).UTC().Format(time.RFC3339),
		"sleep_reason": "",
	})

	if _, ok := pruneStateTimestamp(woken, StateAsleep); !ok {
		t.Errorf("pruneStateTimestamp(asleep, no slept_at) reported unprunable — " +
			"the woken-from-suspended bead is invisible to PruneDetailed forever")
	}
}

// TestSleepPatchClearsSuspendedAt covers the other arrival at asleep: a session
// that was suspended and later slept normally keeps the stale suspended_at too.
func TestSleepPatchClearsSuspendedAt(t *testing.T) {
	patch := SleepPatch(waitStoreNow, string(SleepReasonIdle))
	if v, ok := patch["suspended_at"]; !ok || v != "" {
		t.Errorf("SleepPatch does not clear suspended_at (got %q, present=%v): "+
			"a session that is asleep is not suspended", v, ok)
	}
}

// TestSuspendClearsSleepVocabulary is the reverse direction: Manager.Suspend
// writes state+suspended_at but leaves a prior slept_at/sleep_reason behind.
func TestSuspendClearsSleepVocabulary(t *testing.T) {
	mgr, store, id := reproSession(t)

	if err := NewStore(beads.SessionStore{Store: store}).Sleep(id, string(SleepReasonIdle), time.Now().UTC()); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	slept, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after sleep: %v", err)
	}
	if slept.Metadata["slept_at"] == "" {
		t.Fatal("precondition: sleep did not stamp slept_at")
	}

	if err := mgr.Suspend(id); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	md, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after suspend: %v", err)
	}
	if v := md.Metadata["slept_at"]; v != "" {
		t.Errorf("slept_at = %q survived the transition to suspended", v)
	}
	if v := md.Metadata["sleep_reason"]; v != "" {
		t.Errorf("sleep_reason = %q survived the transition to suspended", v)
	}
}
