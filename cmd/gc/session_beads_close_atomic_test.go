package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// The controller's terminal closes (closeBead, closeFailedCreateBead) used to
// write their terminal metadata and the Close through store.Tx. That Tx is
// atomic on native Dolt and SQLite, but FileStore runs it as two separate
// writes, so a writer landing between them (a wake stamping state=awake) left
// the row status=closed with live-looking metadata. The tests below pin the
// fenced single-write close on every store that provides it, and the
// unchanged Tx on the stores that do not.

// controllerCloseRaceStore runs interfere once, right before the write that
// commits the close: before the atomic close on a store that provides it, and
// between the metadata write and the Close inside the Tx otherwise. It
// advertises the atomic close only when its backing truly provides it, so the
// controller takes the same arm it would on the bare store.
type controllerCloseRaceStore struct {
	beads.Store
	interfere func(id string) error
	// interfereInTx arms interfere inside the Tx. It is off for SQLite, whose
	// Tx holds the store's only write connection, so a writer inside it would
	// wait forever; that Tx is atomic anyway.
	interfereInTx bool
	atomicCalls   int
	txCalls       int
}

func (s *controllerCloseRaceStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	s.txCalls++
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		if !s.interfereInTx {
			return fn(tx)
		}
		return fn(&controllerCloseRaceTx{Tx: tx, interfere: s.interfere})
	})
}

func (s *controllerCloseRaceStore) CloseWithMetadataIfMatch(id string, revision int64, metadata map[string]string) (beads.Bead, error) {
	s.atomicCalls++
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	if s.interfere != nil {
		if err := s.interfere(id); err != nil {
			return beads.Bead{}, err
		}
	}
	return closer.CloseWithMetadataIfMatch(id, revision, metadata)
}

func (s *controllerCloseRaceStore) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	if _, ok := beads.AtomicConditionalCloserFor(s.Store); !ok {
		return nil, false
	}
	return s, true
}

type controllerCloseRaceTx struct {
	beads.Tx
	interfere func(id string) error
}

func (tx *controllerCloseRaceTx) Close(id string) error {
	if tx.interfere != nil {
		if err := tx.interfere(id); err != nil {
			return err
		}
	}
	return tx.Tx.Close(id)
}

// wakeStampOnce models a wake (or a controller acting on an older awake
// observation) stamping state=awake exactly once, through the raw backing.
func wakeStampOnce(backing beads.Store) func(string) error {
	fired := false
	return func(id string) error {
		if fired {
			return nil
		}
		fired = true
		return backing.SetMetadata(id, "state", string(session.StateAwake))
	}
}

// controllerCloseBackend is one real store kind. store is what the controller
// holds; backing is the raw store under any cache, where another writer lands.
type controllerCloseBackend struct {
	name string
	// atomic: the store provides the fenced terminal close.
	atomic bool
	// txSplits: the store's Tx runs its writes separately, which is where the
	// closed-but-awake window lived before the fix.
	txSplits bool
	open     func(t *testing.T) (store, backing beads.Store)
}

func controllerCloseBackends() []controllerCloseBackend {
	openFile := func(t *testing.T) beads.Store {
		t.Helper()
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		return store
	}
	openSQLite := func(t *testing.T) beads.Store {
		t.Helper()
		store, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		t.Cleanup(func() { _ = store.(*beads.SQLiteStore).CloseStore() })
		return store
	}
	return []controllerCloseBackend{
		{name: "AtomicCloseMemStore", atomic: true, txSplits: true, open: func(*testing.T) (beads.Store, beads.Store) {
			s := beads.NewAtomicCloseMemStore()
			return s, s
		}},
		{name: "FileStore", atomic: true, txSplits: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := openFile(t)
			return s, s
		}},
		{name: "SQLiteStore", atomic: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := openSQLite(t)
			return s, s
		}},
		{name: "CachingStore/FileStore", atomic: true, txSplits: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			backing := openFile(t)
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			return cache, backing
		}},
		// Plain MemStore stands in for the stores without the capability (the
		// exec store, a legacy sqlite layout); BdStore has its own test below.
		{name: "MemStore", txSplits: true, open: func(*testing.T) (beads.Store, beads.Store) {
			s := beads.NewMemStore()
			return s, s
		}},
	}
}

// decidedSessionInfo is the Info a caller would decide a close of id on: the
// row as store reads it now, or a bare Info for a row it cannot read.
func decidedSessionInfo(store beads.Store, id string) session.Info {
	b, err := store.Get(id)
	if err != nil {
		return session.Info{ID: id}
	}
	return sessionInfoFromBead(b)
}

func createControllerCloseSession(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "worker-1",
			"state":                string(session.StateCreating),
			"pending_create_claim": "true",
			"sleep_intent":         "idle",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// controllerClosePaths are the two controller close helpers, each with the
// terminal metadata it must leave on the row.
func controllerClosePaths(now time.Time) []struct {
	name  string
	close func(store beads.Store, id string, stderr *bytes.Buffer) bool
	want  map[string]string
} {
	return []struct {
		name  string
		close func(store beads.Store, id string, stderr *bytes.Buffer) bool
		want  map[string]string
	}{
		{
			name: "closeBead",
			close: func(store beads.Store, id string, stderr *bytes.Buffer) bool {
				return closeBead(store, testSeatWork("", servingCity("worker"), store, nil), decidedSessionInfo(store, id), "dead-runtime", now, stderr)
			},
			want: session.ClosePatch(now, "dead-runtime"),
		},
		{
			name: "closeFailedCreateBead",
			close: func(store beads.Store, id string, stderr *bytes.Buffer) bool {
				return closeFailedCreateBead(sessionFrontDoor(store), decidedSessionInfo(store, id), now, stderr)
			},
			want: failedCreateClosePatch(now),
		},
	}
}

// TestControllerClosesRefuseAWakeAfterTheCallersDecision is the regression for
// a close premise taken from the close's own first read. The reconciler
// decides to close a session from its tick snapshot, and a wake stamps
// state=awake before closeBead reads the row. The close must be judged against
// the caller's decision, not against that later read: on every store with the
// atomic close it writes nothing, reports false, and skips the release
// cascade, and the next pass decides again from the woken row.
func TestControllerClosesRefuseAWakeAfterTheCallersDecision(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, path := range []struct {
		name  string
		close func(store beads.Store, decided session.Info, stderr *bytes.Buffer) bool
	}{
		{name: "closeBead", close: func(store beads.Store, decided session.Info, stderr *bytes.Buffer) bool {
			return closeBead(store, testSeatWork("", servingCity("worker"), store, nil), decided, "dead-runtime", now, stderr)
		}},
		{name: "closeFailedCreateBead", close: func(store beads.Store, decided session.Info, stderr *bytes.Buffer) bool {
			return closeFailedCreateBead(sessionFrontDoor(store), decided, now, stderr)
		}},
	} {
		for _, backend := range controllerCloseBackends() {
			if !backend.atomic {
				continue
			}
			t.Run(path.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := createControllerCloseSession(t, store)
				work, err := store.Create(beads.Bead{Title: "task", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}, Assignee: created.ID})
				if err != nil {
					t.Fatalf("Create work: %v", err)
				}
				decided := decidedSessionInfo(store, created.ID)
				if err := wakeStampOnce(backing)(created.ID); err != nil {
					t.Fatalf("stamping the wake: %v", err)
				}

				var stderr bytes.Buffer
				if path.close(store, decided, &stderr) {
					t.Fatalf("%s closed a row woken after the decision", path.name)
				}
				if !strings.Contains(stderr.String(), session.ErrSessionCloseSuperseded.Error()) {
					t.Fatalf("stderr = %q, want the superseded close reported", stderr.String())
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != "open" || got.Metadata["state"] != string(session.StateAwake) || got.Metadata["close_reason"] != "" {
					t.Fatalf("row = status %q state %q close_reason %q, want the woken row untouched", got.Status, got.Metadata["state"], got.Metadata["close_reason"])
				}
				heldWork, err := store.Get(work.ID)
				if err != nil {
					t.Fatalf("Get work: %v", err)
				}
				if heldWork.Assignee != created.ID {
					t.Fatalf("work assignee = %q, want %q: a refused close must not run the release cascade", heldWork.Assignee, created.ID)
				}
			})
		}
	}
}

// TestControllerClosesKeepAConcurrentWakeOutOfTheClosedRow is the regression.
// A wake stamps state=awake in the window before the close's terminal write.
// Before the atomic close, on a store whose Tx splits its writes (FileStore, a
// cache over it), the stamp landed between the metadata write and the Close,
// and the row came to rest status=closed state=awake. With the atomic close
// the stamp won the fence, but the retry re-read only the revision and closed
// the woken session anyway. The wake moved the facts the close was decided
// on, so every store with the atomic close must now leave the row open and
// awake, write no terminal metadata, and skip the release cascade; the next
// pass decides again. A store without the capability keeps the controller's
// single Tx.
func TestControllerClosesKeepAConcurrentWakeOutOfTheClosedRow(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, path := range controllerClosePaths(now) {
		for _, backend := range controllerCloseBackends() {
			t.Run(path.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := createControllerCloseSession(t, store)
				race := &controllerCloseRaceStore{Store: store, interfereInTx: backend.txSplits}
				if _, ok := beads.AtomicConditionalCloserFor(race); ok != backend.atomic {
					t.Fatalf("AtomicConditionalCloserFor(%s) = %v, want %v", backend.name, ok, backend.atomic)
				}
				if !backend.atomic {
					var stderr bytes.Buffer
					if !path.close(race, created.ID, &stderr) {
						t.Fatalf("%s returned false for an open session: %s", path.name, stderr.String())
					}
					got, err := backing.Get(created.ID)
					if err != nil {
						t.Fatalf("Get: %v", err)
					}
					if got.Status != "closed" {
						t.Fatalf("status = %q, want closed", got.Status)
					}
					for key, want := range path.want {
						if got.Metadata[key] != want {
							t.Errorf("metadata[%q] = %q, want %q", key, got.Metadata[key], want)
						}
					}
					if race.atomicCalls != 0 || race.txCalls != 1 {
						t.Fatalf("atomic/tx calls = %d/%d, want 0/1 (the controller's Tx, unchanged)", race.atomicCalls, race.txCalls)
					}
					return
				}
				work, err := store.Create(beads.Bead{Title: "task", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}, Assignee: created.ID})
				if err != nil {
					t.Fatalf("Create work: %v", err)
				}
				race.interfere = wakeStampOnce(backing)

				var stderr bytes.Buffer
				if path.close(race, created.ID, &stderr) {
					t.Fatalf("%s closed a session a wake won the fence for", path.name)
				}
				if !strings.Contains(stderr.String(), session.ErrSessionCloseSuperseded.Error()) {
					t.Fatalf("stderr = %q, want the superseded close reported", stderr.String())
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != "open" || got.Metadata["state"] != string(session.StateAwake) || got.Metadata["close_reason"] != "" {
					t.Fatalf("row = status %q state %q close_reason %q, want the wake's open/awake row untouched", got.Status, got.Metadata["state"], got.Metadata["close_reason"])
				}
				if race.atomicCalls != 1 || race.txCalls != 0 {
					t.Fatalf("atomic/tx calls = %d/%d, want 1/0 (one lost fence, no retry, no split write)", race.atomicCalls, race.txCalls)
				}
				heldWork, err := backing.Get(work.ID)
				if err != nil {
					t.Fatalf("Get work: %v", err)
				}
				if heldWork.Assignee != created.ID {
					t.Fatalf("work assignee = %q, want %q: a refused close must not run the release cascade", heldWork.Assignee, created.ID)
				}
			})
		}
	}
}

// TestControllerClosesUnderRealConcurrentWritersHaveOneWinner races real
// goroutines: several controller closes of the same row and a writer that
// keeps stamping state=awake on it while it is open. The writer is fenced, as
// the session front door's writers are, so it can only write an open row. On
// every store with the atomic close the row must end closed with its terminal
// metadata, and exactly one close must report the close, because only that
// one runs the release cascade.
func TestControllerClosesUnderRealConcurrentWritersHaveOneWinner(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, path := range controllerClosePaths(now) {
		for _, backend := range controllerCloseBackends() {
			if !backend.atomic {
				continue
			}
			t.Run(path.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := createControllerCloseSession(t, store)
				writer, ok := backing.(beads.ConditionalWriter)
				if !ok {
					t.Fatalf("%T is not a ConditionalWriter", backing)
				}

				const closers = 6
				wins := make([]bool, closers)
				start := make(chan struct{})
				stopWriter := make(chan struct{})
				var writers, closing sync.WaitGroup
				var writerErr error
				writers.Add(1)
				go func() {
					defer writers.Done()
					<-start
					for {
						select {
						case <-stopWriter:
							return
						default:
						}
						cur, err := backing.Get(created.ID)
						if err != nil {
							writerErr = err
							return
						}
						if cur.Status == "closed" {
							return
						}
						err = writer.UpdateIfMatch(created.ID, cur.Revision, beads.UpdateOpts{
							Metadata: map[string]string{"state": string(session.StateAwake)},
						})
						var precondition *beads.PreconditionFailedError
						if err != nil && !errors.As(err, &precondition) {
							writerErr = err
							return
						}
					}
				}()
				for i := range closers {
					closing.Add(1)
					go func() {
						defer closing.Done()
						<-start
						var stderr bytes.Buffer
						wins[i] = path.close(store, created.ID, &stderr)
					}()
				}
				close(start)
				closing.Wait()
				close(stopWriter)
				writers.Wait()
				if writerErr != nil {
					t.Fatalf("writer: %v", writerErr)
				}

				winners := 0
				for _, won := range wins {
					if won {
						winners++
					}
				}
				// A writer that keeps winning the fence can exhaust every
				// closer's bounded retries. The row is then still open, and
				// the next controller pass closes it.
				if winners == 0 {
					var stderr bytes.Buffer
					if !path.close(store, created.ID, &stderr) {
						t.Fatalf("follow-up close after the writer stopped failed: %s", stderr.String())
					}
					winners = 1
				}
				if winners != 1 && path.name == "closeBead" {
					t.Fatalf("closeBead reported %d winners, want exactly 1", winners)
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != "closed" {
					t.Fatalf("status = %q, want closed", got.Status)
				}
				for key, want := range path.want {
					if got.Metadata[key] != want {
						t.Errorf("metadata[%q] = %q, want %q", key, got.Metadata[key], want)
					}
				}
			})
		}
	}
}

// TestCloseFailedCreateBeadLeavesAnAlreadyClosedRowRecordAlone pins the
// failed-create path on a store with the atomic close: a row another closer
// already retired keeps that closer's record, and the helper still reports the
// row closed, as its callers expect.
func TestCloseFailedCreateBeadLeavesAnAlreadyClosedRowRecordAlone(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	created := createControllerCloseSession(t, store)
	var stderr bytes.Buffer
	if !closeBead(store, testSeatWork("", nil, store, nil), decidedSessionInfo(store, created.ID), "orphaned", now, &stderr) {
		t.Fatalf("closeBead: %s", stderr.String())
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !closeFailedCreateBead(sessionFrontDoor(store), decidedSessionInfo(store, created.ID), now.Add(time.Minute), &stderr) {
		t.Fatalf("closeFailedCreateBead on a closed row returned false: %s", stderr.String())
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Revision != before.Revision || after.Metadata["state"] != "orphaned" ||
		after.Metadata["close_reason"] != session.CanonicalCloseReason("orphaned") {
		t.Fatalf("closed row rewritten: rev %d -> %d, state %q, close_reason %q", before.Revision, after.Revision, after.Metadata["state"], after.Metadata["close_reason"])
	}
}

// TestControllerClosesYieldToAKillFenceThatLandsInTheWindow covers #6749's
// kill fence. The controller decides to close on an older read (the corpse
// pass, for one, stops the runtime first), and a `gc session kill` can fence
// the row before the close writes. The fence wins the revision fence, and the
// close must then yield instead of re-reading the fence and closing over it:
// the row stays open under the kill, and the work it holds is not released.
func TestControllerClosesYieldToAKillFenceThatLandsInTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, path := range controllerClosePaths(now) {
		t.Run(path.name, func(t *testing.T) {
			store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
			if err != nil {
				t.Fatalf("OpenFileStore: %v", err)
			}
			created := createControllerCloseSession(t, store)
			work, err := store.Create(beads.Bead{Title: "task", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}, Assignee: created.ID})
			if err != nil {
				t.Fatalf("Create work: %v", err)
			}
			race := &controllerCloseRaceStore{Store: store, interfereInTx: true}
			fired := false
			race.interfere = func(id string) error {
				if fired {
					return nil
				}
				fired = true
				return store.SetMetadataBatch(id, map[string]string(session.KillPendingPatch(now)))
			}

			var stderr bytes.Buffer
			if path.close(race, created.ID, &stderr) {
				t.Fatalf("%s closed a row under a fresh kill fence", path.name)
			}
			if !strings.Contains(stderr.String(), session.ErrSessionKillPending.Error()) {
				t.Fatalf("stderr = %q, want the kill-pending yield reported", stderr.String())
			}
			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["state_reason"] != session.KillPendingReason {
				t.Fatalf("row = status %q state %q state_reason %q, want it left open under the kill fence", got.Status, got.Metadata["state"], got.Metadata["state_reason"])
			}
			if race.atomicCalls != 1 || race.txCalls != 0 {
				t.Fatalf("atomic/tx calls = %d/%d, want 1/0 (lost fence, then the yield)", race.atomicCalls, race.txCalls)
			}
			heldWork, err := store.Get(work.ID)
			if err != nil {
				t.Fatalf("Get work: %v", err)
			}
			if heldWork.Assignee != created.ID {
				t.Fatalf("work assignee = %q, want %q: a yielded close must not run the release cascade", heldWork.Assignee, created.ID)
			}
		})
	}
}

// bdCloseLedger is a one-row fake bd CLI: enough of show, update, close and
// list for a controller close to run end to end against a real BdStore.
type bdCloseLedger struct {
	mu        sync.Mutex
	bead      beads.Bead
	calls     []string
	closeArgs [][]string
}

func (l *bdCloseLedger) run(_, name string, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	l.calls = append(l.calls, args[0])
	switch args[0] {
	case "show":
		return l.render(true)
	case "list":
		return []byte(`[]`), nil
	case "update":
		return l.update(args[1:])
	case "close":
		l.closeArgs = append(l.closeArgs, append([]string(nil), args...))
		l.bead.Status = "closed"
		return []byte(`{}`), nil
	default:
		return nil, fmt.Errorf("unexpected bd args: %v", args)
	}
}

func (l *bdCloseLedger) update(args []string) ([]byte, error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", l.bead.ID:
		case "--status":
			i++
			l.bead.Status = args[i]
		case "--set-metadata":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			l.bead.Metadata[key] = value
		case "--title", "--type", "--priority", "--description", "--assignee", "--parent", "--add-label", "--remove-label":
			i++
		default:
			return nil, fmt.Errorf("bd update: unsupported arg %q in %v", args[i], args)
		}
	}
	return []byte(`{}`), nil
}

func (l *bdCloseLedger) render(list bool) ([]byte, error) {
	item := map[string]any{
		"id":         l.bead.ID,
		"title":      l.bead.Title,
		"status":     l.bead.Status,
		"issue_type": l.bead.Type,
		"labels":     l.bead.Labels,
		"created_at": nowForBDJSONTest().Format(time.RFC3339),
		"metadata":   l.bead.Metadata,
	}
	var v any = item
	if list {
		v = []any{item}
	}
	return json.Marshal(v)
}

// TestControllerClosesOnBdStoreKeepTheStagedTx covers the bd-backed city. The
// bd CLI has no fenced terminal close, so the controller must keep its staged
// Tx there: the terminal metadata lands, and `bd close` carries the canonical
// close reason (required under validation.on-close=error).
func TestControllerClosesOnBdStoreKeepTheStagedTx(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, path := range controllerClosePaths(now) {
		t.Run(path.name, func(t *testing.T) {
			ledger := &bdCloseLedger{bead: beads.Bead{
				ID:     "mc-1",
				Title:  "worker",
				Status: "open",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":         "worker-1",
					"state":                string(session.StateCreating),
					"pending_create_claim": "true",
				},
			}}
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "mc")
			if _, ok := beads.AtomicConditionalCloserFor(store); ok {
				t.Fatal("AtomicConditionalCloserFor(BdStore) = true, want false: bd has no fenced terminal close")
			}
			var stderr bytes.Buffer
			if !path.close(store, "mc-1", &stderr) {
				t.Fatalf("%s returned false: %s (bd calls %v)", path.name, stderr.String(), ledger.calls)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.closeArgs) != 1 {
				t.Fatalf("bd close calls = %v, want exactly one", ledger.closeArgs)
			}
			if got, want := testFlagValue(ledger.closeArgs[0], "--reason"), path.want["close_reason"]; got != want {
				t.Fatalf("bd close --reason = %q, want %q", got, want)
			}
			if ledger.bead.Status != "closed" {
				t.Fatalf("status = %q, want closed", ledger.bead.Status)
			}
			for key, want := range path.want {
				if ledger.bead.Metadata[key] != want {
					t.Errorf("metadata[%q] = %q, want %q", key, ledger.bead.Metadata[key], want)
				}
			}
		})
	}
}
