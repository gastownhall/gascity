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
func (s *ProxiedStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	native := s.nativeLeaf()
	if native == nil {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store is demoted: %w", since, ErrEventsJournalUnavailable)
	}
	if !s.journalActive {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: %w", since, ErrEventsJournalInactive)
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
// the store resolves it the same way, once, when it is constructed.
func (s *ProxiedStore) EventsJournalActive() bool {
	return s.journalActive && s.nativeLeaf() != nil
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
