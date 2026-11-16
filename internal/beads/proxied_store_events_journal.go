package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/journalops"
)

var _ journalops.Journal = (*ProxiedStore)(nil)

// ReadEventsJournalPage pages the journal through the native read leaf.
//
// Only the native leaf can answer: the bd leaf publishes no page read, so a
// demoted store refuses with ErrEventsJournalUnavailable and its caller keeps
// the scope on the scan path. A scope whose writes are not journaled refuses
// with ErrEventsJournalInactive. A read that fails with a proxied verdict
// stands the native leaf down, exactly as every other native read does.
//
// The refusals are ordered longest-lasting first — ErrStoreClosed, then
// ErrEventsJournalInactive, then the transient ErrEventsJournalUnavailable —
// so a caller that retries on the last never spins on a store that is closed
// or not journaling. Inactive means not tailable now: a scope can switch its
// journal back on, and recovering from the refusal is a gap
// (ErrEventsJournalInactive says why).
//
// One transient refusal comes ahead of inactive. Resolving the activation can
// wait out another native open's whole window, and a stand-down or re-pin
// inside that wait drops the leaf the read started from, so the leaf is read
// again once the activation is resolved. A leaf that changed refuses with
// ErrEventsJournalUnavailable whatever the activation said: that answer may be
// the dropped leaf's pin's, and the next call answers for the current one.
//
// A stand-down or re-pin can also drop the leaf while the read is in flight on
// it — one bd proxy restart both fails the read into a reconnect and stands
// the leaf down — and the dropped leaf, latched closed, answers ErrStoreClosed
// for a store that is still open. That read refuses with
// ErrEventsJournalUnavailable too. Closing the store latches it closed before
// it drops the leaf, so a read the close cuts short still answers
// ErrStoreClosed.
func (s *ProxiedStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	s.mu.RLock()
	closed, native := s.closed, s.native
	s.mu.RUnlock()
	// closed latches, so a store closed already resolves nothing and is
	// refused below.
	active := !closed && s.journalActive()
	s.mu.RLock()
	closed, current := s.closed, s.native
	s.mu.RUnlock()
	switch {
	case closed:
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store: %w", since, ErrStoreClosed)
	case current != native:
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store changed its native leaf while resolving journal activation: %w", since, ErrEventsJournalUnavailable)
	case !active:
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, ErrEventsJournalInactive)
	case current == nil:
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store is demoted: %w", since, ErrEventsJournalUnavailable)
	}
	page, err := current.readEventsJournalPage(ctx, since, limit, false)
	err = s.classifyReadError(err)
	if errors.Is(err, ErrStoreClosed) {
		s.mu.RLock()
		closed, dropped := s.closed, s.native != current
		s.mu.RUnlock()
		if !closed && dropped {
			// The leaf's error is kept as text: wrapped, the refusal would
			// still read as ErrStoreClosed.
			leafErr := strings.TrimPrefix(err.Error(), fmt.Sprintf("reading events journal after seq %d: ", since))
			return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store dropped its native leaf during the read (%s): %w", since, leafErr, ErrEventsJournalUnavailable)
		}
	}
	return page, err
}

// EventsJournalActive reports whether this scope's writes are journaled and a
// native leaf is serving to read them. A demoted store reports false.
//
// The answer is NOT the native leaf's own activation. That leaf is read-only
// and opened with the BD_ namespace withheld, so its activation reflects the
// workspace config alone. Every write goes through bd, which inherits this
// process's environment and resolves activation with BD_EVENTS_JOURNAL first;
// the store resolves it the same way.
//
// bd's CLI resolves activation on every invocation (only bd serve resolves it
// once, at listen), so the store's answer is a cache: resolved on the first
// journal question, and forgotten by every guard tick and re-pin. A store with
// no guard (a one-shot open) keeps its first answer for its short life.
func (s *ProxiedStore) EventsJournalActive() bool {
	return s.nativeLeaf() != nil && s.journalActive()
}

// proxiedJournalActivation is ProxiedStore's cached journal activation,
// guarded by the store's mu. epoch advances on every forget, so a resolution
// that raced a forget is not cached over it.
type proxiedJournalActivation struct {
	resolved bool
	active   bool
	epoch    uint64
}

// forget drops the cached answer. The caller holds the store's mu.
func (a *proxiedJournalActivation) forget() {
	a.resolved = false
	a.epoch++
}

// journalActive returns the cached activation, resolving it — outside mu,
// since it reads the scope's config — when nothing is cached. The resolution
// is not bounded by a reader's ctx yet (ga-3gfh0h): it waits out any native
// open's window that holds the environment it reads.
func (s *ProxiedStore) journalActive() bool {
	s.mu.RLock()
	cached, pin := s.journal, s.pin
	s.mu.RUnlock()
	if cached.resolved {
		return cached.active
	}
	active := proxiedJournalResolve(pin)
	s.mu.Lock()
	if s.journal.epoch == cached.epoch {
		s.journal.resolved, s.journal.active = true, active
	}
	s.mu.Unlock()
	return active
}

// forgetJournalActivation drops the cached activation so the next journal
// question re-resolves it.
func (s *ProxiedStore) forgetJournalActivation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.journal.forget()
}

// proxiedJournalResolve is the resolution journalActive runs; a test swaps it
// to land a forget in the middle of a resolution.
var proxiedJournalResolve = proxiedEventsJournalActive

// proxiedEventsJournalActive resolves the activation bd's writes to the
// pinned scope run with.
func proxiedEventsJournalActive(pin Pin) bool {
	root := pin.ScopeRoot()
	if root == "" {
		return false
	}
	return ambientEventsJournalEnabledFor(filepath.Join(root, ".beads"))
}
