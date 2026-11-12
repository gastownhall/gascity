package session

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// healWriteStore is a session store with no conditional writer, the shape of
// a city whose beads.conditional_writes is unset or off: there
// ApplyPatchIfLifecycleUnchanged checks the row and then writes unfenced.
// atRead and atWrite run at the Get and SetMetadataBatch boundaries, before
// the read or the write reaches the backing.
type healWriteStore struct {
	beads.Store
	atRead  func(id string)
	atWrite func(id string, kvs map[string]string)
}

func (s *healWriteStore) Get(id string) (beads.Bead, error) {
	if s.atRead != nil {
		s.atRead(id)
	}
	return s.Store.Get(id)
}

func (s *healWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.atWrite != nil {
		s.atWrite(id, kvs)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// The reconciler's advisory heal checks the row it decided on and then
// writes. Without conditional writes nothing fences that write, so it must
// check and write inside the row's mutation lock, where every operator verb
// (Manager.Suspend, Close, Wake) writes.
func TestApplyPatchIfLifecycleUnchangedHoldsMutationLockAcrossReadAndWrite(t *testing.T) {
	b := sessionBeadFixture("heal-fence", "open", map[string]string{"state": "active"})
	store := &healWriteStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)}
	held := func(id string) {
		t.Helper()
		sessionMutationLocksMu.Lock()
		entry := sessionMutationLocks[id]
		sessionMutationLocksMu.Unlock()
		if entry == nil {
			t.Fatalf("session %s: the heal read or wrote outside the row's mutation lock", id)
		}
		if !sessionMutationLockEntryHeld(entry) {
			t.Fatalf("session %s: the heal read or wrote outside the row's mutation lock", id)
		}
	}
	store.atRead = held
	store.atWrite = func(id string, _ map[string]string) { held(id) }
	front := NewStore(beads.SessionStore{Store: store})

	applied, err := front.ApplyPatchIfLifecycleUnchanged(infoFromPersistedBead(b), MetadataPatch{"state": string(StateAwake)})
	if err != nil || !applied {
		t.Fatalf("heal: applied=%v err=%v, want applied", applied, err)
	}
}

// ga-axgaemw: an operator's suspend that lands between the heal's check and
// its write must survive. The heal decided "awake" on a row that read active
// with a live runtime; the suspend then wrote state=suspended and stopped
// the runtime; the heal's write landing last left state=awake over the
// suspend's hold, which the API reads back as asleep (the runtime is gone)
// and the next tick's heal makes durable.
func TestApplyPatchIfLifecycleUnchangedKeepsAConcurrentOperatorSuspend(t *testing.T) {
	b := sessionBeadFixture("heal-vs-suspend", "open", map[string]string{"state": "active"})
	atWrite := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store := &healWriteStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)}
	store.atWrite = func(_ string, kvs map[string]string) {
		if kvs["state"] != string(StateAwake) {
			return
		}
		once.Do(func() { close(atWrite) })
		<-release
	}
	front := NewStore(beads.SessionStore{Store: store})

	// The tick's snapshot: active, with a live runtime, so the heal is awake.
	snapshot := infoFromPersistedBead(b)
	healDone := make(chan error, 1)
	go func() {
		_, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
		healDone <- err
	}()
	<-atWrite // the heal checked the row and is at its write

	// The operator's suspend, written as Manager.Suspend writes it: under the
	// row's mutation lock.
	suspendDone := make(chan error, 1)
	go func() {
		suspendDone <- WithSessionMutationLock(b.ID, func() error {
			_, err := front.OperatorSuspend(b.ID, time.Now())
			return err
		})
	}()
	// Let the suspend either land, or queue behind a heal that holds the lock.
	var suspendErr error
	suspendFinished := false
	deadline := time.Now().Add(10 * time.Second)
	for refs, _ := sessionMutationLockRefs(b.ID); !suspendFinished && refs < 2; refs, _ = sessionMutationLockRefs(b.ID) {
		select {
		case suspendErr = <-suspendDone:
			suspendFinished = true
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the suspend neither landed nor queued on the row's mutation lock")
		}
	}
	close(release)
	if err := <-healDone; err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !suspendFinished {
		suspendErr = <-suspendDone
	}
	if suspendErr != nil {
		t.Fatalf("suspend: %v", suspendErr)
	}

	got, err := front.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MetadataState != string(StateSuspended) {
		t.Fatalf("state after the heal and the suspend = %q, want %q: a heal decided before the suspend landed after it", got.MetadataState, StateSuspended)
	}
}

// A heal must not wait out an operator verb holding the row: Manager.Suspend
// holds the lock through its runtime teardown, and a provider stop can hang.
// The heal is level-triggered, so it skips, writes nothing, and the next tick
// recomputes it from a snapshot that includes the operator's write.
func TestApplyPatchIfLifecycleUnchangedSkipsWhileAnOperatorHoldsTheRow(t *testing.T) {
	b := sessionBeadFixture("heal-skip", "open", map[string]string{"state": "active"})
	writes := 0
	store := &healWriteStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)}
	store.atWrite = func(string, map[string]string) { writes++ }
	front := NewStore(beads.SessionStore{Store: store})
	snapshot := infoFromPersistedBead(b)

	var applied bool
	var healErr error
	if err := WithSessionMutationLock(b.ID, func() error {
		done := make(chan struct{})
		go func() {
			defer close(done)
			applied, healErr = front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the heal waited behind an operator's hold of the row; it must skip")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if healErr != nil || applied || writes != 0 {
		t.Fatalf("heal while the row is held: applied=%v err=%v writes=%d, want a skip that writes nothing", applied, healErr, writes)
	}
	got, err := front.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MetadataState != "active" {
		t.Fatalf("state = %q, want active (untouched)", got.MetadataState)
	}
}
