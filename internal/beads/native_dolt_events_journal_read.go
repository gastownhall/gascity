package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"
)

// ErrEventsJournalUnsupported reports that a native store's storage backend
// does not implement the durable mutation journal's read role
// (journalops.Journal), so there is no journal to page.
var ErrEventsJournalUnsupported = errors.New("events journal unsupported by the storage backend")

// ErrEventsJournalUnavailable reports that a store which can carry a journal
// read has no usable native read leaf to carry it right now: a ProxiedStore
// that has stood its native leaf down reads through bd, which publishes no
// page read, and one that dropped the leaf a read started on — standing it
// down or re-pinning it — refuses that read rather than answer for a closed
// leaf. A caller keeps the scope on its scan path while it sees this error.
var ErrEventsJournalUnavailable = errors.New("events journal unavailable: no usable native read leaf")

// ErrEventsJournalInactive reports that the scope's writes are not being
// journaled, so a page read would answer "caught up" while mutations went
// unrecorded. bd serve refuses its events endpoint for the same reason.
//
// The refusal is not permanent: a scope can switch its journal back on, and
// reads then succeed again. Writes made while it was off have no rows, so a
// tail that saw this refusal resumes past a gap, not where it stopped
// (ledgerfeed.Tailer.Lapse). A switch is noticed only when activation is
// re-resolved — on a direct native store within nativeJournalActivationRecheck,
// or once a native open in flight elsewhere in the process ends; on a proxied
// one at the next guard tick (proxiedGuardInterval) — so a lapse shorter than
// that is never refused and leaves no trace: a consumer that must not miss a
// write still reconciles in full now and then.
var ErrEventsJournalInactive = errors.New("events journal is not activated for this scope")

var _ journalops.Journal = (*NativeDoltStore)(nil)

// ReadEventsJournalPage pages the scope's durable mutation journal from the
// caller's exclusive checkpoint, on the terms journalops.Journal states.
//
// It refuses with ErrEventsJournalInactive when the handle it reads through
// was not opened with the journal activated, or its scope has since turned
// the journal off (EventsJournalActive), and with
// ErrEventsJournalUnsupported when the backend cannot journal at all.
//
// The read goes through the read-retry chain, so it rides a managed-Dolt
// rebind the way every other native read does, and the caller's ctx bounds
// that whole chain: a cancellation ends a reconnect in progress and is
// returned as the context error — unless the handle had already refused the
// read as inactive or unsupported, which answers for the scope rather than
// the call and is returned instead. A *journalops.TruncatedError comes back
// bare, as the journalops contract asks; every other error is wrapped.
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
			// verdict about the endpoint. A refusal the handle had already
			// answered is one about the scope, so it outlasts the cancel. It
			// is wrapped afresh: the chain carries a canceled read's error
			// in a budget verdict, which would stand a proxied leaf down.
			for _, refusal := range []error{ErrEventsJournalInactive, ErrEventsJournalUnsupported} {
				if errors.Is(err, refusal) {
					return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, refusal)
				}
			}
			return journalops.Page{}, ctxErr
		}
		var truncated *journalops.TruncatedError
		if errors.As(err, &truncated) {
			// The journalops contract: hand the typed window back bare, so a
			// type assertion recovers it as well as errors.As does.
			return journalops.Page{}, truncated
		}
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, err)
	}
	return page, nil
}

// EventsJournalActive reports whether the storage this store currently reads
// through can be tailed: it was opened with the events journal activated
// (openNativeDoltStorage resolved the workspace's config and
// BD_EVENTS_JOURNAL when it opened that handle), AND the scope still resolves
// activation on.
//
// The second half matters because bd's CLI resolves activation on every
// invocation: a scope that turns the journal off stops journaling agents'
// writes at once while this handle, opened earlier, keeps journaling gc's
// own. The store re-checks the scope's resolution at most every
// nativeJournalActivationRecheck, so it stops answering "caught up" over
// unrecorded writes within that interval — or, when a native open elsewhere in
// the process holds the environment at the re-check, once that open ends: the
// re-check keeps the last answer rather than wait for it under the store's
// read lock. A scope that turns the journal on after open does not make the
// handle tailable: its own writes are not journaled until it reopens.
//
// The library exposes the activation switch but no getter, so the open-time
// answer is recorded where the switch is thrown (nativeEventsJournalActivations).
// A store built directly over a storage value, which never passed that open,
// reports false. A closed store reports false.
func (s *NativeDoltStore) EventsJournalActive() bool {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false
	}
	defer release()
	return nativeEventsJournalActivations.active(storage)
}

// nativeJournalActivationRecheck bounds how long a recorded handle trusts its
// scope's last activation answer before resolving it again. Resolving reads
// the scope's config.yaml, so a tailer polling several times a second does not
// re-read it on every page.
const nativeJournalActivationRecheck = 5 * time.Second

// nativeEventsJournalActivations records which opened storage handles
// openNativeDoltStorage activated the journal on, and for which scope.
var nativeEventsJournalActivations = &nativeJournalActivationSet{now: time.Now}

// nativeJournalActivationSet is a set of activated storage handles keyed by
// identity, each with the beads directory whose activation it follows.
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
	handles map[beadslib.Storage]*nativeJournalActivation
	// now is the clock the recheck interval is measured on.
	now func() time.Time
	// resolve re-resolves a scope's activation; nil resolves it as bd does
	// whenever no open holds the environment (resolveScope). A test swaps it
	// to land a close or a re-record in a resolution.
	resolve func(beadsDir string) bool
}

// nativeJournalActivation is one recorded handle's scope and the scope's last
// resolved activation.
type nativeJournalActivation struct {
	beadsDir  string
	enabled   bool
	checkedAt time.Time
}

func (a *nativeJournalActivationSet) record(storage beadslib.Storage, beadsDir string) {
	if !nativeStorageComparable(storage) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	if a.handles == nil {
		a.handles = make(map[beadslib.Storage]*nativeJournalActivation)
	}
	// The open resolved activation on the moment before recording.
	a.handles[storage] = &nativeJournalActivation{beadsDir: beadsDir, enabled: true, checkedAt: a.clock()}
}

// active reports whether storage was activated at open and its scope still
// resolves activation on, re-resolving the scope — outside mu, since it reads
// the scope's config — once the last answer is older than the recheck
// interval. A handle that closes, or is recorded anew, while its scope
// resolves reports false.
//
// Its callers hold the store's read lock, so the re-resolution never waits
// for nativeDoltOpenEnvMu: an open elsewhere in the process holds that for its
// whole window, and the store's close, and every reader queued behind that
// close, would wait as long. While an open holds it, active keeps the last
// answer and leaves checkedAt alone, so the next question resolves again.
func (a *nativeJournalActivationSet) active(storage beadslib.Storage) bool {
	if !nativeStorageComparable(storage) {
		return false
	}
	a.mu.Lock()
	a.sweepLocked()
	entry, ok := a.handles[storage]
	if !ok {
		a.mu.Unlock()
		return false
	}
	now := a.clock()
	if now.Sub(entry.checkedAt) < nativeJournalActivationRecheck {
		enabled := entry.enabled
		a.mu.Unlock()
		return enabled
	}
	beadsDir := entry.beadsDir
	a.mu.Unlock()

	enabled, resolved := a.resolveScope(beadsDir)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	if current, ok := a.handles[storage]; !ok || current != entry {
		// The handle closed, or was recorded anew, while its scope resolved:
		// the answer is about a handle that is gone.
		return false
	}
	if !resolved {
		return entry.enabled
	}
	entry.enabled, entry.checkedAt = enabled, now
	return enabled
}

// resolveScope re-resolves beadsDir's activation, reporting resolved false
// when an open holds the environment it reads.
func (a *nativeJournalActivationSet) resolveScope(beadsDir string) (enabled, resolved bool) {
	if a.resolve != nil {
		return a.resolve(beadsDir), true
	}
	return tryAmbientEventsJournalEnabledFor(beadsDir)
}

func (a *nativeJournalActivationSet) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
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
