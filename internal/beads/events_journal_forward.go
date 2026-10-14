package beads

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/journalops"
)

// maxJournalUnwrap bounds ActiveEventsJournal's walk down Backing chains. Real
// chains are two or three deep; the bound only keeps a store whose Backing
// cycles from looping forever.
const maxJournalUnwrap = 16

// ActiveEventsJournal finds the journal read a consumer can tail through
// store, and reports false when there is none it can trust to be complete.
//
// The outermost store in the Backing chain that carries journalops.Journal
// answers: it is returned when its EventsJournalActive reports true, and
// nothing is returned otherwise. Discovery never unwraps past a store that
// carries the journal, because a wrapper's answer can differ from its
// backing's — a ProxiedStore's Backing is its native read leaf, whose own
// activation is not the scope's. A store that does not carry the journal (a
// CachingStore holds none of its own) is unwrapped through Backing.
//
// The walk ends, with false, at a wrapper that has neither. gc's bead-policy
// layer, which every store open applies outermost, is one: discovery through
// it misses a journal that is active beneath it. A consumer runs discovery on
// the store below that layer — the engine, or a CachingStore or gc's emitting
// wrapper over it.
//
// A caller that gets false keeps the scope on its scan path. A caller that
// gets a journal can still be refused by a later read
// (ErrEventsJournalUnavailable after a demotion or a leaf change,
// ErrEventsJournalInactive after the scope turned the journal off — noticed
// by the next guard tick on a proxied store, and on a direct native one
// within nativeJournalActivationRecheck or once a native open in flight
// ends), and handles that the same way.
func ActiveEventsJournal(store Store) (journalops.Journal, bool) {
	for range maxJournalUnwrap {
		if store == nil {
			return nil, false
		}
		if journal, ok := store.(journalops.Journal); ok {
			if !EventsJournalActiveThrough(store) {
				return nil, false
			}
			return journal, true
		}
		wrapper, ok := store.(interface{ Backing() Store })
		if !ok {
			return nil, false
		}
		store = wrapper.Backing()
	}
	return nil, false
}

// ReadEventsJournalPageThrough pages inner's journal, refusing with
// ErrEventsJournalUnsupported when inner has no journal read. It is the
// forward a wrapping store's ReadEventsJournalPage delegates to.
func ReadEventsJournalPageThrough(ctx context.Context, inner Store, since int64, limit int) (journalops.Page, error) {
	journal, ok := inner.(journalops.Journal)
	if !ok {
		return journalops.Page{}, fmt.Errorf("reading events journal after seq %d through %T: %w", since, inner, ErrEventsJournalUnsupported)
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
