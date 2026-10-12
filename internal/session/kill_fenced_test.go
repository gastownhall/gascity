package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// stopErrGoneProvider stops the runtime but reports an error, as a provider
// whose teardown raced the process's own exit does.
type stopErrGoneProvider struct{ *runtime.Fake }

func (p stopErrGoneProvider) Stop(name string) error {
	_ = p.Fake.Stop(name)
	return errors.New("stop raced the exit")
}

func newKillFencedSession(t *testing.T, sp runtime.Provider, fake *runtime.Fake) (*Manager, beads.Store, Info) {
	t.Helper()
	store := beads.NewMemStore()
	creator := newTestManager(t, store, fake)
	info, err := creator.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return newTestManager(t, store, sp), store, info
}

// A kill whose stop reports an error but leaves no runtime has done what it
// is for: KillFenced succeeds and leaves the row asleep/killed, the fence
// lifted, rather than restoring a row that claims a runtime.
func TestKillFencedStopErrorWithRuntimeGoneSucceeds(t *testing.T) {
	fake := runtime.NewFake()
	mgr, store, info := newKillFencedSession(t, stopErrGoneProvider{fake}, fake)
	kill := func(ctx context.Context, by Actor) error { return mgr.Kill(ctx, by, info.ID) }
	if err := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, kill); err != nil {
		t.Fatalf("KillFenced = %v, want nil: the runtime is gone", err)
	}
	b, _ := store.Get(info.ID)
	if b.Metadata["state"] != string(StateAsleep) || b.Metadata["sleep_reason"] != string(SleepReasonKilled) || b.Metadata["state_reason"] != "" {
		t.Errorf("row = %v, want asleep/killed with the fence lifted", b.Metadata)
	}
}

// A runtime already gone has no window to fence: kill runs alone, writes
// nothing, and its result is returned as it is.
func TestKillFencedRuntimeAlreadyGoneRunsKillAlone(t *testing.T) {
	fake := runtime.NewFake()
	mgr, store, info := newKillFencedSession(t, fake, fake)
	if err := store.SetMetadataBatch(info.ID, map[string]string{"state": string(StateAsleep)}); err != nil {
		t.Fatal(err)
	}
	_ = fake.Stop(info.SessionName)
	before, _ := store.Get(info.ID)
	want := errors.New("not active")
	got := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, func(context.Context, Actor) error { return want })
	if !errors.Is(got, want) {
		t.Fatalf("KillFenced = %v, want kill's own error", got)
	}
	after, _ := store.Get(info.ID)
	if after.Revision != before.Revision {
		t.Errorf("row rewritten for a runtime already gone: %v", after.Metadata)
	}
}

// The kill runs under the lease the fence was written under: kill's actor
// carries it, so the stop does not wait on a lease its own caller holds.
func TestKillFencedLendsItsLeaseToTheKill(t *testing.T) {
	fake := runtime.NewFake()
	mgr, _, info := newKillFencedSession(t, fake, fake)
	var lent *RuntimeLease
	err := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, func(ctx context.Context, by Actor) error {
		lent = by.Lease
		return mgr.Kill(ctx, by, info.ID)
	})
	if err != nil {
		t.Fatalf("KillFenced: %v", err)
	}
	if lent == nil {
		t.Fatal("kill ran without the fence's lease")
	}
}

// fenceRefusingStore refuses the kill fence's write (it hides conditional
// writes, so the fence takes the plain batch write) and passes the rest.
type fenceRefusingStore struct{ beads.Store }

func (s fenceRefusingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if kvs["state_reason"] == KillPendingReason {
		return errors.New("store unavailable")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// A fence that cannot be written does not hold the kill hostage: the runtime
// is stopped, and the row is synced asleep/killed after it.
func TestKillFencedWithoutAFenceStillSyncsAsleep(t *testing.T) {
	fake := runtime.NewFake()
	_, store, info := newKillFencedSession(t, fake, fake)
	mgr := newTestManager(t, fenceRefusingStore{store}, fake)
	kill := func(ctx context.Context, by Actor) error { return mgr.Kill(ctx, by, info.ID) }
	if err := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, kill); err != nil {
		t.Fatalf("KillFenced: %v", err)
	}
	if fake.IsRunning(info.SessionName) {
		t.Fatal("runtime still running")
	}
	b, _ := store.Get(info.ID)
	if b.Metadata["state"] != string(StateAsleep) || b.Metadata["sleep_reason"] != string(SleepReasonKilled) {
		t.Errorf("row = %v, want the post-kill asleep/killed sync", b.Metadata)
	}
}
