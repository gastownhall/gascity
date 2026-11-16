package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Every delete the backing commits must announce bead.deleted for the id. The
// run projection drops a row on that event alone and replays the retained log
// on every restart, so a delete that announces nothing leaves the row listed
// forever (gastownhall/gascity#5934). When no snapshot of the row is at hand —
// it was never cached here, or the pre-delete read failed — the event carries
// an id-only tombstone.

// deletedNotes records the bead.deleted notifications a cache sends.
type deletedNotes struct {
	notes []cacheWriteNotification
}

func (n *deletedNotes) record(eventType, beadID string, payload json.RawMessage) {
	if eventType == "bead.deleted" {
		n.notes = append(n.notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	}
}

// payloadFor returns the decoded bead.deleted payload announced for id,
// failing unless exactly one was announced and it names id.
func (n *deletedNotes) payloadFor(t *testing.T, id string) Bead {
	t.Helper()
	var found []Bead
	for _, note := range n.notes {
		if note.beadID != id {
			continue
		}
		b, ok := DecodeBeadEventPayload(note.payload)
		if !ok {
			t.Fatalf("bead.deleted payload for %s does not decode: %s", id, note.payload)
		}
		if b.ID != id {
			t.Fatalf("bead.deleted for %s carries payload id %q", id, b.ID)
		}
		found = append(found, b)
	}
	if len(found) != 1 {
		t.Fatalf("bead.deleted notifications for %s = %d, want exactly 1; all: %+v", id, len(found), n.notes)
	}
	return found[0]
}

func newDeleteEventsCache(t *testing.T, backing Store) (*CachingStore, *deletedNotes) {
	t.Helper()
	notes := &deletedNotes{}
	cache := NewCachingStoreForTest(backing, notes.record)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, notes
}

// mustCreateCold creates a bead behind the cache's back, so the cache never
// holds its row: a row another process wrote, or one the cache evicted.
func mustCreateCold(t *testing.T, backing Store, b Bead) Bead {
	t.Helper()
	created, err := backing.Create(b)
	if err != nil {
		t.Fatalf("backing Create(%+v): %v", b, err)
	}
	return created
}

func TestCachingStoreDeleteAnnouncesDeleteWhenThePreDeleteReadFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// warm creates the row through the cache, so the cache holds it.
		warm bool
		// closed closes the row in the backing before the delete.
		closed bool
		// arm makes the pre-delete read of the row fail or miss.
		arm       func(*casBackingStore)
		wantTitle string
	}{
		{
			name:      "transient read failure on an uncached row announces an id-only tombstone",
			arm:       func(s *casBackingStore) { s.failNextGet = true },
			wantTitle: "",
		},
		{
			name:      "transient read failure on a cached row announces the cached snapshot",
			warm:      true,
			arm:       func(s *casBackingStore) { s.failNextGet = true },
			wantTitle: "doomed",
		},
		{
			name:      "a read that hides the closed row it then deletes announces an id-only tombstone",
			closed:    true,
			arm:       func(s *casBackingStore) { s.hideClosedFromGet = true },
			wantTitle: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, notes := newDeleteEventsCache(t, backing)
			var b Bead
			if tt.warm {
				b = mustBatchCreate(t, cache, Bead{Title: "doomed"})
			} else {
				b = mustCreateCold(t, backing, Bead{Title: "doomed"})
			}
			if tt.closed {
				if err := backing.Close(b.ID); err != nil {
					t.Fatalf("backing Close: %v", err)
				}
			}
			tt.arm(backing)

			if err := cache.Delete(b.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			if _, err := backing.Store.Get(b.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("backing Get after Delete = %v, want ErrNotFound (the delete committed)", err)
			}
			if got := notes.payloadFor(t, b.ID); got.Title != tt.wantTitle {
				t.Fatalf("bead.deleted payload title = %q, want %q", got.Title, tt.wantTitle)
			}
		})
	}
}

// A delete the backing refused removed nothing, so it announces nothing.
func TestCachingStoreDeleteThatFailsAnnouncesNothing(t *testing.T) {
	t.Parallel()
	cache, notes := newDeleteEventsCache(t, NewMemStore())

	if err := cache.Delete("gc-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
	if len(notes.notes) != 0 {
		t.Fatalf("bead.deleted notifications = %+v, want none for a delete that failed", notes.notes)
	}
}

func TestCachingStoreDeleteIfMatchAnnouncesDeleteWhenThePreDeleteReadFails(t *testing.T) {
	t.Parallel()
	backing := &casBackingStore{Store: NewMemStore()}
	cache, notes := newDeleteEventsCache(t, backing)
	b := mustCreateCold(t, backing, Bead{Title: "doomed"})
	backing.failNextGet = true

	if err := cache.DeleteIfMatch(b.ID, b.Revision); err != nil {
		t.Fatalf("DeleteIfMatch: %v", err)
	}

	if got := notes.payloadFor(t, b.ID); got.Title != "" {
		t.Fatalf("bead.deleted payload = %+v, want an id-only tombstone", got)
	}
}

func TestCachingStoreDeleteBatchAnnouncesUncachedIDs(t *testing.T) {
	t.Parallel()
	backing := &batchBackingSpy{MemStore: NewMemStore()}
	cache, notes := newDeleteEventsCache(t, backing)
	warm := mustBatchCreate(t, cache, Bead{Title: "warm", Type: "molecule"})
	cold := mustCreateCold(t, backing, Bead{Title: "cold", Type: "molecule"})

	if err := cache.DeleteBatch([]string{warm.ID, cold.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	if got := notes.payloadFor(t, warm.ID); got.Title != "warm" {
		t.Fatalf("bead.deleted payload for the cached row = %+v, want its cached snapshot", got)
	}
	// The batch path reads nothing per id, so the uncached row's event is the
	// bare id, not a backing read.
	if got := notes.payloadFor(t, cold.ID); got.Title != "" || got.Type != "" {
		t.Fatalf("bead.deleted payload for the uncached row = %+v, want an id-only tombstone", got)
	}
}

func TestCachingStoreDeleteBatchAnnouncesEachIDOnce(t *testing.T) {
	t.Parallel()
	backing := &batchBackingSpy{MemStore: NewMemStore()}
	cache, notes := newDeleteEventsCache(t, backing)
	warm := mustBatchCreate(t, cache, Bead{Title: "warm"})
	cold := mustCreateCold(t, backing, Bead{Title: "cold"})

	if err := cache.DeleteBatch([]string{warm.ID, cold.ID, "", warm.ID, cold.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	notes.payloadFor(t, warm.ID)
	notes.payloadFor(t, cold.ID)
	if len(notes.notes) != 2 {
		t.Fatalf("bead.deleted notifications = %+v, want one per distinct id", notes.notes)
	}
}

// refusingBatchBacking is a BatchDeleter whose batch delete removes nothing and
// fails with err.
type refusingBatchBacking struct {
	*MemStore
	err error
}

func (s *refusingBatchBacking) DeleteBatch([]string) error { return s.err }

var _ BatchDeleter = (*refusingBatchBacking)(nil)

// A batch the backing refused outright removed nothing, so it announces
// nothing, whether the refusal is a plain error, a BatchDeleteError that
// committed no ids, or the capability being absent.
func TestCachingStoreDeleteBatchThatTheBackingRefusesAnnouncesNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "plain error", err: errors.New("bd delete failed")},
		{name: "batch error with no committed ids", err: &BatchDeleteError{Err: errors.New("first chunk failed")}},
		{name: "batch delete unsupported", err: ErrBatchDeleteUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backing := &refusingBatchBacking{MemStore: NewMemStore(), err: tt.err}
			cache, notes := newDeleteEventsCache(t, backing)
			warm := mustBatchCreate(t, cache, Bead{Title: "warm"})
			cold := mustCreateCold(t, backing, Bead{Title: "cold"})

			if err := cache.DeleteBatch([]string{warm.ID, cold.ID}); !errors.Is(err, tt.err) {
				t.Fatalf("DeleteBatch = %v, want %v", err, tt.err)
			}

			if len(notes.notes) != 0 {
				t.Fatalf("bead.deleted notifications = %+v, want none for a refused batch", notes.notes)
			}
			if _, err := cache.Get(warm.ID); err != nil {
				t.Fatalf("Get(cached row) after a refused batch = %v, want it still served", err)
			}
		})
	}
}

// getCountingBatchBacking counts the point reads a cache makes of its backing.
type getCountingBatchBacking struct {
	*batchBackingSpy
	gets int
}

func (s *getCountingBatchBacking) Get(id string) (Bead, error) {
	s.gets++
	return s.batchBackingSpy.Get(id)
}

// The controller applies every recorded bead.* event back to its caches, so a
// cache meets the echo of each delete it announced. Its tombstone already
// answers for the row, so the echo must not read the backing: on a bd backing
// that read is a subprocess (two for a wisp) per deleted id, on the one
// goroutine that applies events.
func TestCachingStoreEchoOfItsOwnDeleteReadsNothing(t *testing.T) {
	t.Parallel()
	backing := &getCountingBatchBacking{batchBackingSpy: &batchBackingSpy{MemStore: NewMemStore()}}
	cache, notes := newDeleteEventsCache(t, backing)
	warm := mustBatchCreate(t, cache, Bead{Title: "warm"})
	cold := mustCreateCold(t, backing, Bead{Title: "cold"})
	if err := cache.DeleteBatch([]string{warm.ID, cold.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(notes.notes) != 2 {
		t.Fatalf("bead.deleted notifications = %+v, want one per id", notes.notes)
	}

	before := backing.gets
	for _, note := range notes.notes {
		cache.ApplyEventSnapshot(note.eventType, note.payload)
	}

	if read := backing.gets - before; read != 0 {
		t.Fatalf("echoing %d bead.deleted events read the backing %d times, want 0", len(notes.notes), read)
	}
	for _, id := range []string{warm.ID, cold.ID} {
		if _, err := cache.Get(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%s) after the echo = %v, want ErrNotFound", id, err)
		}
	}
}

// A chunk-committing backend that fails partway still durably removed its
// committed ids, cached or not; each announces bead.deleted, and the id it
// never removed announces nothing.
func TestCachingStoreDeleteBatchPartialFailureAnnouncesUncachedCommittedIDs(t *testing.T) {
	t.Parallel()
	backing := &partialBatchBackingSpy{MemStore: NewMemStore(), commit: 2}
	cache, notes := newDeleteEventsCache(t, backing)
	warm := mustBatchCreate(t, cache, Bead{Title: "warm"})
	cold := mustCreateCold(t, backing, Bead{Title: "cold"})
	pending := mustCreateCold(t, backing, Bead{Title: "pending"})

	if err := cache.DeleteBatch([]string{warm.ID, cold.ID, pending.ID}); err == nil {
		t.Fatal("DeleteBatch: want the partial backing failure")
	}

	if got := notes.payloadFor(t, warm.ID); got.Title != "warm" {
		t.Fatalf("bead.deleted payload for the cached committed row = %+v, want its cached snapshot", got)
	}
	if got := notes.payloadFor(t, cold.ID); got.Title != "" {
		t.Fatalf("bead.deleted payload for the uncached committed row = %+v, want an id-only tombstone", got)
	}
	if len(notes.notes) != 2 {
		t.Fatalf("bead.deleted notifications = %+v, want only the two committed ids", notes.notes)
	}
}
