package session

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file is store_sqlite_terminal_close_test.go's harness transferred to a
// REAL beads.FileStore — the store every GC_BEADS=file city runs, and the store
// the integration journeys run. Until the atomic terminal-close capability
// reached it, Store.Close took the historical ClosePatch-then-Close fallback
// there, which is the ga-f7v2ft.78.6 strand class: a stale controller write
// lands between the two commands and leaves a closed row carrying a nonterminal
// lifecycle state.
//
// staleAwakeBeforeCloseBacking (store_sqlite_terminal_close_test.go) is the
// deterministic stand-in for that writer and is reused verbatim: it stamps
// state=awake exactly once immediately before an unfenced Close reaches the
// backing store, and forwards the backing store's atomic terminal-close
// capability so the arm Close takes is decided by what FileStore can really do.

// openFileSessionRow opens a flock-backed FileStore the way a real file-backed
// city does (cmd/gc openScopeLocalFileStore) and seeds one open session row.
func openFileSessionRow(t *testing.T, metadata map[string]string) (beads.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	store.SetLocker(beads.NewFileFlock(path + ".lock"))
	bead, err := store.Create(beads.Bead{
		Title:    "probe",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return store, bead.ID
}

// contendedCloseBacking injects a genuine competing writer immediately before
// chosen fenced close attempts, so Close's bounded reread-and-retry arm runs
// against a real FileStore instead of a capability double. interfereBefore
// receives the 1-based attempt number; returning true advances the row's
// revision first, which is exactly what a stale controller cycle does to the
// fence.
type contendedCloseBacking struct {
	beads.Store
	interfereBefore func(attempt int) bool
	attempts        int
}

func (s *contendedCloseBacking) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return nil, false
	}
	return &contendedCloser{backing: s, closer: closer}, true
}

type contendedCloser struct {
	backing *contendedCloseBacking
	closer  beads.AtomicConditionalCloser
}

func (c *contendedCloser) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	c.backing.attempts++
	if c.backing.interfereBefore != nil && c.backing.interfereBefore(c.backing.attempts) {
		if err := c.backing.SetMetadata(id, "state", string(StateAwake)); err != nil {
			return beads.Bead{}, err
		}
	}
	return c.closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// TestFileStoreAdvertisesAtomicTerminalClose pins the capability discovery the
// fix turns on: Store.Close only leaves the split fallback when
// AtomicConditionalCloserFor resolves a closer through the typed class wrapper.
func TestFileStoreAdvertisesAtomicTerminalClose(t *testing.T) {
	backing, _ := openFileSessionRow(t, map[string]string{"state": string(StateAwake)})
	if _, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: backing}); !ok {
		t.Fatal("AtomicConditionalCloserFor(SessionStore{FileStore}) = unavailable; the split close sequence stays live on a file-backed city")
	}
}

// TestCloseOverFileStoreCannotStrandAwakeStateOnAClosedRow is the ga-f7v2ft.78.6
// regression at the file store. A stale controller write lands in the window
// between ClosePatch and Close; the durable row must never come to rest closed
// with a nonterminal lifecycle state, and the terminal write must be one fenced
// command rather than a metadata write followed by a separate close.
func TestCloseOverFileStoreCannotStrandAwakeStateOnAClosedRow(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	stale := &staleAwakeBeforeCloseBacking{Store: backing}
	front := NewStore(beads.SessionStore{Store: stale})

	closed, err := front.Close(id, "drained", time.Date(2026, 8, 8, 10, 17, 35, 0, time.UTC))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close reported not-closed for an open suspended session")
	}

	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read durable row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("durable row = status=%q state=%q; want closed/%s (closed+awake is the incident signature)",
			row.Status, row.Metadata["state"], StateDrained)
	}
	if stale.plainCloses != 0 {
		t.Fatalf("terminal close fell back to %d unfenced Close calls, want 0", stale.plainCloses)
	}
}

// TestCloseOverFileStoreYieldsToAWinningCloser proves the fused write is fenced:
// when another actor closes the row first, the terminal close reports
// already-closed instead of blindly re-closing.
func TestCloseOverFileStoreYieldsToAWinningCloser(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	stale := &staleAwakeBeforeCloseBacking{Store: backing}
	front := NewStore(beads.SessionStore{Store: stale})

	if err := backing.Close(id); err != nil {
		t.Fatalf("pre-close the row: %v", err)
	}

	closed, err := front.Close(id, "drained", time.Now())
	if err != nil {
		t.Fatalf("Close on an already-closed row: %v", err)
	}
	if closed {
		t.Fatal("Close reported closed for a row another actor had already closed")
	}
	if stale.plainCloses != 0 {
		t.Fatalf("already-closed row took %d Close calls, want 0", stale.plainCloses)
	}
}

// TestFileCloseWithMetadataIfMatchRefusesAStaleFence pins the store-level
// contract Close's retry arm depends on: a losing fence changes nothing at all,
// so a caller can reread and retry without inspecting a half-written row.
func TestFileCloseWithMetadataIfMatchRefusesAStaleFence(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	closer, ok := beads.AtomicConditionalCloserFor(backing)
	if !ok {
		t.Fatal("FileStore does not expose atomic terminal close")
	}
	observed, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if err := backing.SetMetadata(id, "state", string(StateAwake)); err != nil {
		t.Fatalf("advance the row under the observed revision: %v", err)
	}

	if _, err := closer.CloseWithMetadataIfMatch(id, observed.Revision, map[string]string{"state": string(StateDrained)}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch at a stale revision = %v, want a precondition failure", err)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("reread row: %v", err)
	}
	if row.Status != "open" || row.Metadata["state"] != string(StateAwake) {
		t.Fatalf("losing fence mutated the row: status=%q state=%q, want open/%s", row.Status, row.Metadata["state"], StateAwake)
	}

	if _, err := closer.CloseWithMetadataIfMatch(id, row.Revision, map[string]string{"state": string(StateDrained)}); err != nil {
		t.Fatalf("CloseWithMetadataIfMatch at the current revision: %v", err)
	}
	row, err = backing.Get(id)
	if err != nil {
		t.Fatalf("reread closed row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("fused terminal write = status=%q state=%q, want closed/%s", row.Status, row.Metadata["state"], StateDrained)
	}
}

// TestCloseOverFileStoreRetriesAContendedFence is the first half of the
// behavior change this capability introduces on file-backed cities: a close
// that LOSES the fence to a concurrent writer must reread and retry, not
// surface an error. One interference, then the retry commits the terminal row.
func TestCloseOverFileStoreRetriesAContendedFence(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	contended := &contendedCloseBacking{Store: backing, interfereBefore: func(attempt int) bool { return attempt == 1 }}
	front := NewStore(beads.SessionStore{Store: contended})

	closed, err := front.Close(id, "drained", time.Now())
	if err != nil {
		t.Fatalf("contended Close: %v (a lost fence must retry, not error)", err)
	}
	if !closed {
		t.Fatal("contended Close reported not-closed for an open session")
	}
	if contended.attempts != 2 {
		t.Fatalf("fenced close attempts = %d, want 2 (one loss, one winning retry)", contended.attempts)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read durable row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("durable row after retry = status=%q state=%q, want closed/%s", row.Status, row.Metadata["state"], StateDrained)
	}
}

// TestCloseOverFileStoreBoundsRetriesUnderRelentlessInterference is the second
// half: retrying must not become a livelock. Under a writer that wins the fence
// before EVERY attempt, Close stops after terminalCloseMaxAttempts and surfaces
// a typed precondition failure rather than spinning or closing unfenced. The
// row is left untouched by the losing attempts.
func TestCloseOverFileStoreBoundsRetriesUnderRelentlessInterference(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	contended := &contendedCloseBacking{Store: backing, interfereBefore: func(int) bool { return true }}
	front := NewStore(beads.SessionStore{Store: contended})

	closed, err := front.Close(id, "drained", time.Now())
	if err == nil {
		t.Fatal("relentlessly contended Close succeeded; want a bounded refusal")
	}
	if closed {
		t.Fatal("failed Close reported the session closed")
	}
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("bounded refusal = %v, want a typed precondition failure the caller can classify", err)
	}
	if contended.attempts != terminalCloseMaxAttempts {
		t.Fatalf("fenced close attempts = %d, want exactly %d (bounded, no livelock)", contended.attempts, terminalCloseMaxAttempts)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read durable row: %v", err)
	}
	if row.Status != "open" || row.Metadata["state"] != string(StateAwake) {
		t.Fatalf("losing attempts mutated the row: status=%q state=%q, want open/%s", row.Status, row.Metadata["state"], StateAwake)
	}
}

// TestCloseOverFileStoreConcurrentClosesHaveExactlyOneWinner drives two real
// terminal closes at one file-backed row at once. Exactly one may report it did
// the closing; the loser must observe the winner's terminal row and report
// already-closed. Neither may error, and the durable row must be terminal.
func TestCloseOverFileStoreConcurrentClosesHaveExactlyOneWinner(t *testing.T) {
	backing, id := openFileSessionRow(t, map[string]string{"state": string(StateSuspended)})
	front := NewStore(beads.SessionStore{Store: backing})

	type outcome struct {
		closed bool
		err    error
	}
	outcomes := make(chan outcome, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			closed, err := front.Close(id, "drained", time.Now())
			outcomes <- outcome{closed: closed, err: err}
		}()
	}
	ready.Wait()
	close(start)

	winners := 0
	for range 2 {
		got := <-outcomes
		if got.err != nil {
			t.Fatalf("concurrent Close: %v", got.err)
		}
		if got.closed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent terminal closes reported %d winners, want exactly 1", winners)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read durable row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("durable row = status=%q state=%q, want closed/%s", row.Status, row.Metadata["state"], StateDrained)
	}
}
