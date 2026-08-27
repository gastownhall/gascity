package beads_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func openAtomicCloseFileStore(t *testing.T) (*beads.FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	return store, path
}

func atomicCloserFor(t *testing.T, store beads.Store) beads.AtomicConditionalCloser {
	t.Helper()
	closer, ok := beads.AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatalf("store %T does not expose atomic terminal close", store)
	}
	return closer
}

// TestFileStoreAtomicCloseFusesMetadataAndClose pins the capability contract:
// one call, one fresh revision, merged metadata, and a returned bead that does
// not alias stored state.
func TestFileStoreAtomicCloseFusesMetadataAndClose(t *testing.T) {
	store, _ := openAtomicCloseFileStore(t)
	created, err := store.Create(beads.Bead{Title: "atomic close", Metadata: map[string]string{"keep": "me"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closer := atomicCloserFor(t, store)

	closed, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Revision != created.Revision+1 {
		t.Fatalf("closed bead = status=%q revision=%d, want closed at one fresh revision past %d", closed.Status, closed.Revision, created.Revision)
	}
	if closed.Metadata["state"] != "drained" || closed.Metadata["keep"] != "me" {
		t.Fatalf("closed metadata = %#v, want the merge of the sibling key and the terminal key", closed.Metadata)
	}

	closed.Metadata["state"] = "corrupted caller copy"
	fresh, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Metadata["state"] != "drained" {
		t.Fatalf("returned bead aliases store metadata: %#v", fresh)
	}
}

// TestFileStoreAtomicClosePersistsToDisk is the reason FileStore needs its own
// override rather than the promoted MemStore method: a fresh handle reading
// straight from disk must see the terminal row. A missing save cannot fake it.
func TestFileStoreAtomicClosePersistsToDisk(t *testing.T) {
	store, path := openAtomicCloseFileStore(t)
	created, err := store.Create(beads.Bead{Title: "durable atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := atomicCloserFor(t, store).CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"}); err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}

	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	row, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatalf("Get from a fresh handle: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != "drained" {
		t.Fatalf("persisted row = status=%q state=%q, want closed/drained", row.Status, row.Metadata["state"])
	}
	if row.Revision != created.Revision+1 {
		t.Fatalf("persisted revision = %d, want %d (the fence token must survive the save/reload cycle)", row.Revision, created.Revision+1)
	}
}

// TestFileStoreAtomicCloseStaleFenceLeavesFileUntouched pins the all-or-nothing
// half of the contract Store.Close's retry arm depends on: a losing fence
// mutates neither memory nor the file, so a caller rereads a whole row rather
// than a half-written one.
func TestFileStoreAtomicCloseStaleFenceLeavesFileUntouched(t *testing.T) {
	store, path := openAtomicCloseFileStore(t)
	created, err := store.Create(beads.Bead{Title: "stale fence"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(created.ID, "state", "awake"); err != nil {
		t.Fatalf("advance the row past the observed revision: %v", err)
	}
	before, err := fsys.OSFS{}.ReadFile(path)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}

	_, err = atomicCloserFor(t, store).CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	var pfe *beads.PreconditionFailedError
	if !errors.As(err, &pfe) {
		t.Fatalf("stale close error = %v, want *PreconditionFailedError", err)
	}
	if pfe.Expected != created.Revision {
		t.Fatalf("PreconditionFailedError.Expected = %d, want the caller's own token %d", pfe.Expected, created.Revision)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pfe.Current != row.Revision {
		t.Fatalf("PreconditionFailedError.Current = %d, want the live revision %d", pfe.Current, row.Revision)
	}
	if row.Status != "open" || row.Metadata["state"] != "awake" {
		t.Fatalf("losing fence mutated the row: status=%q state=%q, want open/awake", row.Status, row.Metadata["state"])
	}
	after, err := fsys.OSFS{}.ReadFile(path)
	if err != nil {
		t.Fatalf("reread store file: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("losing fence rewrote the store file; a refused conditional write must not save")
	}
}

// TestFileStoreAtomicCloseHasExactlyOneSameRevisionWinner is the revision
// contract's concurrency clause at the file store: two closers holding the same
// observed revision produce one winner and one typed loser, and the durable row
// carries the winner's terminal write.
func TestFileStoreAtomicCloseHasExactlyOneSameRevisionWinner(t *testing.T) {
	store, _ := openAtomicCloseFileStore(t)
	created, err := store.Create(beads.Bead{Title: "racing atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closer := atomicCloserFor(t, store)

	type result struct {
		bead beads.Bead
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, winner := range []string{"first", "second"} {
		go func() {
			ready.Done()
			<-start
			bead, closeErr := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{
				"state": "drained", "winner": winner,
			})
			results <- result{bead: bead, err: closeErr}
		}()
	}
	ready.Wait()
	close(start)

	wins, losses := 0, 0
	for range 2 {
		got := <-results
		if got.err == nil {
			wins++
			if got.bead.Status != "closed" || got.bead.Metadata["state"] != "drained" {
				t.Fatalf("winner returned a nonterminal row: %#v", got.bead)
			}
			continue
		}
		if !beads.IsPreconditionFailed(got.err) {
			t.Fatalf("losing close error = %v, want a precondition failure", got.err)
		}
		losses++
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("same-revision close winners/losses = %d/%d, want 1/1", wins, losses)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get final row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != "drained" || row.Metadata["winner"] == "" || row.Revision != created.Revision+1 {
		t.Fatalf("final row = %#v, want one atomic terminal winner", row)
	}
}

// TestFileStoreAtomicCloseRollsBackAFailedFlush proves the override wraps the
// delegate the same way FileStore's other conditional writes do: when the flush
// fails, the in-memory mutation is undone so memory and file stay in sync.
func TestFileStoreAtomicCloseRollsBackAFailedFlush(t *testing.T) {
	f := fsys.NewFake()
	store, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "flush failure"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")

	if _, err := atomicCloserFor(t, store).CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"}); err == nil {
		t.Fatal("atomic close reported success while the flush failed")
	} else if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("flush failure error = %v, want the underlying disk error", err)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != "open" || row.Metadata["state"] != "" || row.Revision != created.Revision {
		t.Fatalf("in-memory row after a failed flush = %#v, want the pre-close row rolled back", row)
	}
}

// TestFileStoreAtomicCloseDisabledToggleReturnsUnsupported extends the
// disable-toggle contract to the new method: the toggle is a runtime refusal,
// not interface-stripping, so the store still claims the capability and returns
// the typed unsupported error.
func TestFileStoreAtomicCloseDisabledToggleReturnsUnsupported(t *testing.T) {
	store, _ := openAtomicCloseFileStore(t)
	created, err := store.Create(beads.Bead{Title: "disabled"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.DisableConditionalWrites = true

	closer := atomicCloserFor(t, store)
	if _, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"}); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("atomic close on a disabled store = %v, want ErrConditionalWriteUnsupported", err)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != "open" || row.Revision != created.Revision {
		t.Fatalf("refused atomic close mutated the row: %#v", row)
	}
}

// TestFileBackedCityResolvesAtomicCloseThroughItsRealComposition walks the
// store stack a GC_BEADS=file city actually builds — the factory open for
// provider "file", the CachingStore the API and controller wrap it in, and the
// typed session-class view — and proves the atomic terminal closer resolves at
// every layer, closing through the cache without losing its notification. This
// is the capability the cmd/gc close-path gates
// (session_start_reconcile.go's "drain acknowledgement atomic terminal closer
// is unavailable" refusals) require; until FileStore gained it those gates
// parked or fell back to legacy on every file-backed city.
func TestFileBackedCityResolvesAtomicCloseThroughItsRealComposition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: filepath.Dir(path),
		CityPath:  filepath.Dir(path),
		Provider:  "file",
		OpenFileStore: func() (beads.Store, error) {
			return beads.OpenFileStore(fsys.OSFS{}, path)
		},
	})
	if err != nil {
		t.Fatalf("open file-backed city store: %v", err)
	}
	if _, ok := beads.AtomicConditionalCloserFor(opened.Store); !ok {
		t.Fatal("factory-opened file store does not expose atomic terminal close")
	}
	if _, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: opened.Store}); !ok {
		t.Fatal("session-class view of the file store does not expose atomic terminal close")
	}

	var notifications []string
	cache := beads.NewCachingStoreForTest(opened.Store, func(eventType, _ string, _ json.RawMessage) {
		notifications = append(notifications, eventType)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	created, err := cache.Create(beads.Bead{Title: "file city terminal close"})
	if err != nil {
		t.Fatalf("Create through the cache: %v", err)
	}
	notifications = nil

	closer, ok := beads.AtomicConditionalCloserFor(cache)
	if !ok {
		t.Fatal("cache(file store) does not expose atomic terminal close")
	}
	closed, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch through the cache: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("closed bead = %#v, want a terminal row", closed)
	}
	if len(notifications) != 1 || notifications[0] != "bead.closed" {
		t.Fatalf("notifications = %v, want the cache-preserved bead.closed", notifications)
	}
}

// TestMemStoreStaysNonCapableForAtomicClose guards the deliberate asymmetry:
// FileStore gaining the capability must not promote it to plain MemStore, whose
// non-capability keeps the non-atomic Close arm exercisable.
func TestMemStoreStaysNonCapableForAtomicClose(t *testing.T) {
	if _, ok := beads.AtomicConditionalCloserFor(beads.NewMemStore()); ok {
		t.Fatal("plain MemStore exposes atomic terminal close; the non-atomic arm is no longer testable")
	}
}
