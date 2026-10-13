package beads

import (
	"context"
	"fmt"
	"path/filepath"

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
// The refusals are ordered permanent-first — ErrStoreClosed, then
// ErrEventsJournalInactive, then the transient ErrEventsJournalUnavailable —
// so a caller that retries on the last never spins on a store that is closed
// or never journals.
func (s *ProxiedStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	s.mu.RLock()
	closed, native := s.closed, s.native
	s.mu.RUnlock()
	if closed {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store: %w", since, ErrStoreClosed)
	}
	if !s.journalActive() {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, ErrEventsJournalInactive)
	}
	if native == nil {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store is demoted: %w", since, ErrEventsJournalUnavailable)
	}
	page, err := native.readEventsJournalPage(ctx, since, limit, false)
	return page, s.classifyReadError(err)
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
// since it reads the scope's config — when nothing is cached.
func (s *ProxiedStore) journalActive() bool {
	s.mu.RLock()
	cached, pin := s.journal, s.pin
	s.mu.RUnlock()
	if cached.resolved {
		return cached.active
	}
	active := proxiedEventsJournalActive(pin)
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

// proxiedEventsJournalActive resolves the activation bd's writes to the
// pinned scope run with.
func proxiedEventsJournalActive(pin Pin) bool {
	root := pin.ScopeRoot()
	if root == "" {
		return false
	}
	return nativeEventsJournalEnabledFor(filepath.Join(root, ".beads"))
}
