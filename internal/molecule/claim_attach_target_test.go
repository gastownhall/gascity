package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// casSpyStore counts the metadata CAS calls its conditional writer serves.
type casSpyStore struct {
	*beads.MemStore
	cas int
}

func (s *casSpyStore) CompareAndSetMetadataKey(id, key, from, to string) (bool, error) {
	s.cas++
	return s.MemStore.CompareAndSetMetadataKey(id, key, from, to)
}

// TestClaimAttachCandidateUsesTheCASThroughATarget pins that the attach
// claim reaches the store's conditional writer through the molecule target
// (typed-stores T5a), rather than falling back to the racy read-check-set a
// target would leave it with if the writer were looked up on the target.
func TestClaimAttachCandidateUsesTheCASThroughATarget(t *testing.T) {
	store := &casSpyStore{MemStore: beads.NewMemStore()}
	root, err := store.Create(beads.Bead{Title: "root", Metadata: map[string]string{beadmeta.AttachFencePendingMetadataKey: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	won, err := claimAttachCandidate(beadstest.GraphMolecule(store), root.ID, "a", "b")
	if err != nil || !won {
		t.Fatalf("claimAttachCandidate = %v, %v; want true, nil", won, err)
	}
	if store.cas != 1 {
		t.Errorf("the claim made %d CAS calls, want 1: it fell back to read-check-set", store.cas)
	}
}
