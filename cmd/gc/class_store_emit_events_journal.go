package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/steveyegge/beads/journalops"
)

// ReadEventsJournalPage forwards the journal read to the inner store. It is a
// read and emits nothing.
func (s *emittingClassStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return beads.ReadEventsJournalPageThrough(ctx, s.Store, since, limit)
}

// EventsJournalActive forwards the inner store's journal activation.
func (s *emittingClassStore) EventsJournalActive() bool {
	return beads.EventsJournalActiveThrough(s.Store)
}
