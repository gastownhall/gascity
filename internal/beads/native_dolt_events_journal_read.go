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

// ErrEventsJournalInactive reports that the scope's writes are not being
// journaled, so a page read would answer "caught up" while mutations went
// unrecorded. bd serve refuses its events endpoint for the same reason.
var ErrEventsJournalInactive = errors.New("events journal is not activated for this scope")

var _ journalops.Journal = (*NativeDoltStore)(nil)

// ReadEventsJournalPage pages the scope's durable mutation journal from the
// caller's exclusive checkpoint, on the terms journalops.Journal states.
//
// It refuses with ErrEventsJournalInactive when the handle it reads through
// was not opened with the journal activated (EventsJournalActive), and with
// ErrEventsJournalUnsupported when the backend cannot journal at all.
//
// The read goes through the read-retry chain, so it rides a managed-Dolt
// rebind the way every other native read does, and the caller's ctx bounds
// that whole chain: a cancellation ends a reconnect in progress and is
// returned as the context error. A *journalops.TruncatedError comes back
// wrapped with %w, so errors.As recovers it.
func (s *NativeDoltStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return s.readEventsJournalPage(ctx, since, limit, true)
}

// readEventsJournalPage is ReadEventsJournalPage with the activation gate
// optional, for a wrapper (ProxiedStore) whose writes go through another
// leaf and which therefore answers the activation question itself.
func (s *NativeDoltStore) readEventsJournalPage(ctx context.Context, since int64, limit int, requireActive bool) (journalops.Page, error) {
	var page journalops.Page
	err := s.withReadRetryContext(ctx, func(readCtx context.Context, storage beadslib.Storage) error {
		journal, ok := storage.(journalops.Journal)
		if !ok {
			return fmt.Errorf("storage %T: %w", storage, ErrEventsJournalUnsupported)
		}
		if requireActive && !nativeEventsJournalActivations.active(storage) {
			return ErrEventsJournalInactive
		}
		read, err := journal.ReadEventsJournalPage(readCtx, since, limit)
		if err != nil {
			return err
		}
		page = read
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller gave up; whatever the chain was doing is not a
			// verdict about the endpoint.
			return journalops.Page{}, ctxErr
		}
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
// It exists because the library exposes the activation setter and no getter,
// and openNativeDoltStorage's callers (the open paths and the reconnect hook)
// hand storage around without room for a flag beside it. It holds a handle
// only while that handle is open: every record and every lookup drops the
// handles that report themselves closed (the library's stores implement
// IsClosed). A handle whose dynamic type is not comparable cannot be a map key
// and is not recorded; every library backend is a pointer.
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
	a.sweepLocked()
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
	a.sweepLocked()
	_, ok := a.handles[storage]
	return ok
}

// sweepLocked drops the handles that report themselves closed.
func (a *nativeJournalActivationSet) sweepLocked() {
	for handle := range a.handles {
		if closer, ok := handle.(interface{ IsClosed() bool }); ok && closer.IsClosed() {
			delete(a.handles, handle)
		}
	}
}

func nativeStorageComparable(storage beadslib.Storage) bool {
	return storage != nil && reflect.TypeOf(storage).Comparable()
}
