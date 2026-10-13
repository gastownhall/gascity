package session

import (
	"context"
	"errors"
	"testing"
	"time"

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

// A runtime already gone has no window to fence: kill runs alone and its
// result is returned as it is. An unconfigured session's row is left as it
// was; a configured named session's row is synced asleep/killed, as `gc
// session kill` does, so the kill supersedes its pending wake.
func TestKillFencedRuntimeAlreadyGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named bool
	}{
		{"unconfigured session", false},
		{"configured named session", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			mgr, store, info := newKillFencedSession(t, fake, fake)
			meta := map[string]string{"state": string(StateAsleep), "wake_request": "explicit", "wake_requested_at": time.Now().UTC().Format(time.RFC3339)}
			if tc.named {
				meta["configured_named_session"], meta["configured_named_identity"] = "true", "chat"
			}
			if err := store.SetMetadataBatch(info.ID, meta); err != nil {
				t.Fatal(err)
			}
			_ = fake.Stop(info.SessionName)
			calls := len(fake.SnapshotCalls())
			want := errors.New("not active")
			got := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, func(context.Context, Actor) error { return want })
			if !errors.Is(got, want) {
				t.Fatalf("KillFenced = %v, want kill's own error", got)
			}
			for _, c := range fake.SnapshotCalls()[calls:] {
				if c.Method == "Stop" {
					t.Errorf("a runtime already gone was stopped again")
				}
			}
			after, _ := store.Get(info.ID)
			if after.Metadata["state_reason"] == KillPendingReason {
				t.Errorf("a fence was written for a runtime already gone: %v", after.Metadata)
			}
			if !tc.named {
				if after.Metadata["wake_request"] != "explicit" || after.Metadata["sleep_reason"] != "" {
					t.Errorf("unconfigured row rewritten: %v", after.Metadata)
				}
				return
			}
			if after.Metadata["sleep_reason"] != string(SleepReasonKilled) || after.Metadata["wake_request"] != "" {
				t.Errorf("named row = %v, want asleep/killed with the pending wake cleared", after.Metadata)
			}
		})
	}
}

// The flock fallback after a lease failure other than busy is retryable when
// the flock itself is busy: ErrSessionStarting, as every operator lease wait,
// and the kill does not run. The lease failure here is a lent lease already
// released; the flock is held by another stopper.
func TestKillFencedBusyFlockFallbackIsRetryable(t *testing.T) {
	fake := runtime.NewFake()
	mgr, _, info := newKillFencedSession(t, fake, fake)
	by := testActor(mgr, ActorOperator)
	stale, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: by.City.Path(), Name: info.SessionName})
	if err != nil {
		t.Fatal(err)
	}
	stale.Release()
	held, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: by.City.Path(), Name: info.SessionName})
	if err != nil {
		t.Fatalf("holding the flock: %v", err)
	}
	defer held.Release()
	by.Lease = stale
	killed := false
	err = mgr.KillFenced(context.Background(), by, info.ID, func(context.Context, Actor) error { killed = true; return nil })
	if !errors.Is(err, ErrSessionStarting) {
		t.Fatalf("KillFenced = %v, want ErrSessionStarting (retryable)", err)
	}
	if killed || !fake.IsRunning(info.SessionName) {
		t.Error("the kill ran without the flock")
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

// startWhileLeasedStore models a controller start that finishes while the
// kill waits for the runtime lease. The row starts asleep with no runtime
// and another holder's unexpired lease record. The kill's first lease
// attempt reads the row (the second read of it, after the kill's own); at
// that read the start lands (the runtime runs, the row reads active) and the
// holder's record goes, so the attempt takes the lease.
type startWhileLeasedStore struct {
	beads.Store
	id    string
	reads int
	start func()
}

func (s *startWhileLeasedStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		if s.reads++; s.reads == 2 && s.start != nil {
			s.start()
		}
	}
	return s.Store.Get(id)
}

// Kills deciding the dead path before the lease (#7709 review): a start that
// finished while the kill waited for the lease must be killed under the
// fence, not on the unfenced dead path (which a pass reads as a crash).
func TestKillFencedDecidesUnderTheLease(t *testing.T) {
	fake := runtime.NewFake()
	_, store, info := newKillFencedSession(t, fake, fake)
	remote := MetadataPatch{
		RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseEpochKey: "4", RuntimeLeaseTTLKey: "240",
		RuntimeLeaseExpiresKey: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), RuntimeLeaseFlockKey: "another-boot/1/2/0123",
	}
	meta := map[string]string{"state": string(StateAsleep), "configured_named_session": "true", "configured_named_identity": "chat"}
	for k, v := range remote {
		meta[k] = v
	}
	if err := store.SetMetadataBatch(info.ID, meta); err != nil {
		t.Fatal(err)
	}
	_ = fake.Stop(info.SessionName)

	racing := &startWhileLeasedStore{Store: store, id: info.ID}
	racing.start = func() {
		if err := fake.Start(context.Background(), info.SessionName, runtime.Config{Command: "claude"}); err != nil {
			t.Fatal(err)
		}
		started := map[string]string{"state": string(StateActive)}
		for k := range remote {
			started[k] = ""
		}
		if err := store.SetMetadataBatch(info.ID, started); err != nil {
			t.Fatal(err)
		}
	}
	mgr := newTestManager(t, racing, fake)
	var atStop map[string]string
	kill := func(ctx context.Context, by Actor) error {
		b, _ := store.Get(info.ID)
		atStop = b.Metadata
		return mgr.Kill(ctx, by, info.ID)
	}
	if err := mgr.KillFenced(context.Background(), testActor(mgr, ActorOperator), info.ID, kill); err != nil {
		t.Fatalf("KillFenced: %v", err)
	}
	if racing.reads < 2 {
		t.Fatalf("premise: the start never landed while the kill waited for the lease (%d reads)", racing.reads)
	}
	if atStop["state_reason"] != KillPendingReason {
		t.Errorf("row at the stop = %v, want the kill fence: the started runtime was killed on the unfenced dead path", atStop)
	}
	if fake.IsRunning(info.SessionName) {
		t.Error("the started runtime survived the kill")
	}
	b, _ := store.Get(info.ID)
	if b.Metadata["state"] != string(StateAsleep) || b.Metadata["sleep_reason"] != string(SleepReasonKilled) || b.Metadata["state_reason"] != "" {
		t.Errorf("row after the kill = %v, want asleep/killed with the fence lifted", b.Metadata)
	}
}
