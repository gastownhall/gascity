package beads

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/journalops"
)

// A store that wraps another forwards the journal read so a caller's
// journalops.Journal assertion survives the wrap, the way every other engine
// capability does. Forwarding is structural: the wrapper always carries the
// methods, and EventsJournalActive is the predicate that says whether a read
// can succeed.

var _ journalops.Journal = (*CachingStore)(nil)

// ReadEventsJournalPageThrough pages inner's journal, refusing with
// ErrEventsJournalUnsupported when inner has no journal read. It is the
// forward a wrapping store's ReadEventsJournalPage delegates to.
func ReadEventsJournalPageThrough(ctx context.Context, inner Store, since int64, limit int) (journalops.Page, error) {
	journal, ok := inner.(journalops.Journal)
	if !ok {
		return journalops.Page{}, fmt.Errorf("reading events journal through %T: %w", inner, ErrEventsJournalUnsupported)
	}
	return journal.ReadEventsJournalPage(ctx, since, limit)
}

// EventsJournalActiveThrough reports inner's journal activation, false when
// inner cannot report one. It is the forward a wrapping store's
// EventsJournalActive delegates to.
func EventsJournalActiveThrough(inner Store) bool {
	reporter, ok := inner.(interface{ EventsJournalActive() bool })
	return ok && reporter.EventsJournalActive()
}

// ReadEventsJournalPage pages the backing store's journal. The cache holds
// no journal of its own; the read always reaches the backing.
func (c *CachingStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return ReadEventsJournalPageThrough(ctx, c.backing, since, limit)
}

// EventsJournalActive reports the backing store's journal activation.
func (c *CachingStore) EventsJournalActive() bool {
	return EventsJournalActiveThrough(c.backing)
}
