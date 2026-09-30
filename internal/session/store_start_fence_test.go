package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type startFenceStore struct {
	beads.Store
	check         func(string)
	reads, writes int
}

func (s *startFenceStore) Get(id string) (beads.Bead, error) {
	s.check(id)
	s.reads++
	return s.Store.Get(id)
}

func (s *startFenceStore) SetMetadataBatch(id string, patch map[string]string) error {
	s.check(id)
	s.writes++
	return s.Store.SetMetadataBatch(id, patch)
}

// Inspect the actual shared mutex at each boundary. This deterministically
// rejects both an unlocked preflight read and an unlocked write after a check.
func TestStartAndRollbackHoldMutationLockAcrossReadAndWrite(t *testing.T) {
	b := sessionBeadFixture("start-fence", "open", map[string]string{
		"state": "creating", "pending_create_claim": "true", "generation": "2", "instance_token": "token-2",
	})
	store := &startFenceStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)}
	store.check = func(id string) {
		t.Helper()
		sessionMutationLocksMu.Lock()
		entry := sessionMutationLocks[id]
		sessionMutationLocksMu.Unlock()
		if entry == nil {
			t.Fatal("session mutation lock not acquired")
		}
		if entry.mu.TryLock() {
			entry.mu.Unlock()
			t.Fatal("session mutation lock released before the operation")
		}
	}
	front := NewStore(beads.SessionStore{Store: store})
	expected := Info{ID: b.ID, MetadataState: "creating", PendingCreateClaim: true, Generation: "2", InstanceToken: "token-2"}
	if applied, err := front.WithPendingCreateRollback(expected, func() error {
		store.check(b.ID)
		return nil
	}); err != nil || !applied {
		t.Fatalf("rollback: applied=%v err=%v", applied, err)
	}
	if applied, err := front.CommitStartedIfCurrent(expected, MetadataPatch{"state": "active", "pending_create_claim": ""}); err != nil || !applied {
		t.Fatalf("completion: applied=%v err=%v", applied, err)
	}
	if store.reads != 2 || store.writes != 1 {
		t.Fatalf("reads=%d writes=%d, want 2 and 1", store.reads, store.writes)
	}
}
