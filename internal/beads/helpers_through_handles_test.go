package beads

import (
	"context"
	"testing"
)

// countingCapStore is a store with the optional reads the generic helpers
// look for, counting each one it serves.
type countingCapStore struct {
	*MemStore
	batches, reports int
}

func (s *countingCapStore) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	s.batches++
	out := map[string]Bead{}
	var missing []string
	for _, id := range ids {
		b, err := s.Get(id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		out[id] = b
	}
	return out, missing, nil
}

func (s *countingCapStore) UpdateReporting(id string, opts UpdateOpts) (UpdatedRow, error) {
	s.reports++
	if err := s.Update(id, opts); err != nil {
		return UpdatedRow{}, err
	}
	b, err := s.Get(id)
	return UpdatedRow{Status: b.Status, Metadata: b.Metadata}, err
}

// TestGenericHelpersSeeThroughClassHandles pins that the generic read and
// write helpers a typed caller hands a class handle (typed-stores T5b) reach
// the store's own optional capability, exactly as they do on the raw store:
// GetMany and PrefetchExact its exact batch read, UpdateAndReadBack its
// reporting update, and DirectMembers and DirectMembersWithRoot the cache's
// own live read, which reads past the cache without refreshing it.
func TestGenericHelpersSeeThroughClassHandles(t *testing.T) {
	for name, wrap := range map[string]func(Store) Store{
		"the store":        func(s Store) Store { return s },
		"a graph handle":   func(s Store) Store { return GraphStore{s} },
		"a graph target":   func(s Store) Store { return GraphMolecule(GraphStore{s}) },
		"a session handle": func(s Store) Store { return SessionStore{s} },
	} {
		store := &countingCapStore{MemStore: NewMemStore()}
		a, _ := store.Create(Bead{Title: "a"})
		b, _ := store.Create(Bead{Title: "b"})

		if _, err := GetMany(wrap(store), []string{a.ID, b.ID}); err != nil || store.batches != 1 {
			t.Errorf("%s: GetMany made %d batch reads (%v), want 1", name, store.batches, err)
		}
		if _, err := PrefetchExact(wrap(store), []string{a.ID, b.ID}); err != nil || store.batches != 2 {
			t.Errorf("%s: PrefetchExact made %d batch reads in all (%v), want 2", name, store.batches, err)
		}
		status := "in_progress"
		if _, err := UpdateAndReadBack(wrap(store), a.ID, UpdateOpts{Status: &status}); err != nil || store.reports != 1 {
			t.Errorf("%s: UpdateAndReadBack made %d reporting updates (%v), want 1", name, store.reports, err)
		}

		backing := NewMemStore()
		root, _ := backing.Create(Bead{Title: "root"})
		member, _ := backing.Create(Bead{Title: "member", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := backing.Close(member.ID); err != nil {
			t.Fatal(err)
		}
		members, err := DirectMembers(wrap(cache), root.ID)
		if err != nil {
			t.Fatalf("%s: DirectMembers: %v", name, err)
		}
		for _, m := range members {
			if m.ID == member.ID && m.Status != "closed" {
				t.Errorf("%s: DirectMembers read member %s as %q, want the live closed status", name, m.ID, m.Status)
			}
		}
		// The live read goes to the backing store past the cache, as the
		// cache's own live handle does: it must not refresh the cached row
		// (CachingStore.List with Live set would install the closed status).
		if cached, err := cache.cachedGetOnly(member.ID); err != nil || cached.Status != "open" {
			t.Errorf("%s: after DirectMembers the cache holds member %s as %q (%v), want the primed open row untouched", name, member.ID, cached.Status, err)
		}
		if _, err := DirectMembersWithRoot(wrap(cache), root); err != nil {
			t.Fatalf("%s: DirectMembersWithRoot: %v", name, err)
		}
		if cached, err := cache.cachedGetOnly(member.ID); err != nil || cached.Status != "open" {
			t.Errorf("%s: after DirectMembersWithRoot the cache holds member %s as %q (%v), want the primed open row untouched", name, member.ID, cached.Status, err)
		}
	}
}
