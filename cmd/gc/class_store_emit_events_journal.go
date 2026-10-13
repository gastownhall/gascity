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
//
// Neither journal method unwraps: the emitter carries a journal only while the
// store directly inside it does, which holds because gc places it directly on
// an engine (withMainStoreEmission, withCLIEmission). beads.ActiveEventsJournal
// stops at the emitter, but cannot reach it through the bead-policy layer
// rebuilt over it, which has no Backing; a consumer runs discovery beneath that
// layer (unwrapBeadPolicyStore).
func (s *emittingClassStore) EventsJournalActive() bool {
	return beads.EventsJournalActiveThrough(s.Store)
}
