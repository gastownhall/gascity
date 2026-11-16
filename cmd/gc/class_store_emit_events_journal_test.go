package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/steveyegge/beads/journalops"
)

// The journal read survives the emitting wrap as a read. Over the SQLite
// engine, which has no journal, the claim answers as the engine's absence
// would: the typed refusal and no activation
// (TestEmittingClassStoreOverClaimsArePinnedPerEngine).
func TestEmittingClassStoreForwardsTheEventsJournalRead(t *testing.T) {
	wrapped := splitClassRoutes(openBindingEngineForTest(t)).withCLIEmission(t.TempDir()).stores[coordclass.ClassGraph]
	journal, ok := wrapped.(journalops.Journal)
	if !ok {
		t.Fatalf("%T does not carry journalops.Journal", wrapped)
	}
	if _, err := journal.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, beads.ErrEventsJournalUnsupported) {
		t.Fatalf("error = %v, want beads.ErrEventsJournalUnsupported", err)
	}
	if beads.EventsJournalActiveThrough(wrapped) {
		t.Fatal("a wrapper over a store with no journal reports one active")
	}
}
