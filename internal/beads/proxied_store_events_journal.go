package beads

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/journalops"
)

var _ journalops.Journal = (*ProxiedStore)(nil)

// ReadEventsJournalPage pages the journal through the native read leaf.
//
// Only the native leaf can answer: the bd leaf publishes no page read, so a
// demoted store refuses with ErrEventsJournalUnavailable and its caller keeps
// the scope on the scan path. A read that fails with a proxied verdict stands
// the native leaf down, exactly as every other native read does.
func (s *ProxiedStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	native := s.nativeLeaf()
	if native == nil {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d: proxied store is demoted: %w", since, ErrEventsJournalUnavailable)
	}
	page, err := native.ReadEventsJournalPage(ctx, since, limit)
	return page, s.classifyReadError(err)
}

// EventsJournalActive reports the native read leaf's activation. A demoted
// store reports false: it has no journal read to offer.
func (s *ProxiedStore) EventsJournalActive() bool {
	native := s.nativeLeaf()
	return native != nil && native.EventsJournalActive()
}
