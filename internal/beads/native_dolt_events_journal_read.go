package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"
)

// ErrEventsJournalUnsupported reports that a native store's storage backend
// does not implement the durable mutation journal's read role
// (journalops.Journal), so there is no journal to page.
var ErrEventsJournalUnsupported = errors.New("events journal unsupported by the storage backend")

// ErrEventsJournalUnavailable reports that a store which can carry a journal
// read has no native read leaf to carry it right now: a ProxiedStore that has
// stood its native leaf down reads through bd, which publishes no page read.
// A caller keeps the scope on its scan path while it sees this error.
var ErrEventsJournalUnavailable = errors.New("events journal unavailable: no native read leaf")

var _ journalops.Journal = (*NativeDoltStore)(nil)

// ReadEventsJournalPage pages the scope's durable mutation journal from the
// caller's exclusive checkpoint, on the terms journalops.Journal states.
//
// The read goes through withReadRetry, so it rides a managed-Dolt rebind the
// way every other native read does. A *journalops.TruncatedError, and the
// ErrEventsJournalUnsupported refusal of a backend that cannot journal, come
// back wrapped with %w: errors.As and errors.Is recover them. A caller
// cancellation cancels the read and is returned as the context error.
func (s *NativeDoltStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	var page journalops.Page
	err := s.withReadRetry(func(readCtx context.Context, storage beadslib.Storage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		journal, ok := storage.(journalops.Journal)
		if !ok {
			return fmt.Errorf("storage %T: %w", storage, ErrEventsJournalUnsupported)
		}
		pageCtx, cancel := context.WithCancel(readCtx)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		read, err := journal.ReadEventsJournalPage(pageCtx, since, limit)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		page = read
		return nil
	})
	if err != nil {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, err)
	}
	return page, nil
}

// EventsJournalActive reports whether the storage this store currently reads
// through was opened with the events journal activated — the answer
// openNativeDoltStorage resolved from the workspace's config (and
// BD_EVENTS_JOURNAL) when it opened that handle.
//
// The library exposes the activation switch but no getter, so the answer is
// recorded where the switch is thrown (nativeEventsJournalActivations). A store
// built directly over a storage value, which never passed that open, reports
// false. A closed store reports false.
func (s *NativeDoltStore) EventsJournalActive() bool {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false
	}
	defer release()
	return nativeEventsJournalActivations.active(storage)
}

// nativeEventsJournalActivations records which opened storage handles
// openNativeDoltStorage activated the journal on.
var nativeEventsJournalActivations = &nativeJournalActivationSet{}

// nativeJournalActivationSet is a set of storage handles keyed by identity.
//
// It holds a handle only while that handle is open: every record first drops
// the handles that report themselves closed (the library's stores implement
// IsClosed), so a process that reconnects or reopens repeatedly carries at
// most its live activated handles plus the ones closed since the last open.
// A handle whose dynamic type is not comparable cannot be a map key and is not
// recorded; every library backend is a pointer.
type nativeJournalActivationSet struct {
	mu      sync.Mutex
	handles map[beadslib.Storage]struct{}
}

func (a *nativeJournalActivationSet) record(storage beadslib.Storage) {
	if !nativeStorageComparable(storage) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for handle := range a.handles {
		if closer, ok := handle.(interface{ IsClosed() bool }); ok && closer.IsClosed() {
			delete(a.handles, handle)
		}
	}
	if a.handles == nil {
		a.handles = make(map[beadslib.Storage]struct{})
	}
	a.handles[storage] = struct{}{}
}

func (a *nativeJournalActivationSet) active(storage beadslib.Storage) bool {
	if !nativeStorageComparable(storage) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.handles[storage]
	return ok
}

func nativeStorageComparable(storage beadslib.Storage) bool {
	return storage != nil && reflect.TypeOf(storage).Comparable()
}
