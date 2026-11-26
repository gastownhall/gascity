package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// fakeEventWatcher replays a fixed list of events, then reports end-of-stream.
type fakeEventWatcher struct {
	evts []events.Event
}

func (f *fakeEventWatcher) Next() (events.Event, error) {
	if len(f.evts) == 0 {
		return events.Event{}, errFakeWatcherDone
	}
	evt := f.evts[0]
	f.evts = f.evts[1:]
	return evt, nil
}

func (f *fakeEventWatcher) Close() error { return nil }

var errFakeWatcherDone = &fakeWatcherDoneError{}

type fakeWatcherDoneError struct{}

func (*fakeWatcherDoneError) Error() string { return "fake watcher done" }

func closedBeadEvent(t *testing.T, b beads.Bead) events.Event {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	return events.Event{Type: events.BeadClosed, Payload: payload}
}

// TestScopeCloseWatcherRecordsOnlyClosedScopeMembers covers gastownhall/gascity#9507:
// the serve loop must learn which closed beads are scope members so it can
// reconcile their scopes. Closes of non-scoped beads, and non-close events, are
// not recorded.
func TestScopeCloseWatcherRecordsOnlyClosedScopeMembers(t *testing.T) {
	t.Parallel()

	member := beads.Bead{ID: "wf.body.a", Status: "closed", Metadata: map[string]string{
		beadmeta.ScopeRefMetadataKey: "body",
	}}
	plain := beads.Bead{ID: "wf.plain", Status: "closed", Metadata: map[string]string{}}
	watcher := &scopeCloseWatcher{Watcher: &fakeEventWatcher{evts: []events.Event{
		closedBeadEvent(t, plain),
		{Type: events.BeadUpdated},
		closedBeadEvent(t, member),
	}}}

	for i := 0; i < 3; i++ {
		if _, err := watcher.Next(); err != nil && !errors.Is(err, errFakeWatcherDone) {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	got := watcher.takeClosedScopeMembers()
	if len(got) != 1 || got[0] != "wf.body.a" {
		t.Fatalf("takeClosedScopeMembers = %v, want [wf.body.a]", got)
	}
	if again := watcher.takeClosedScopeMembers(); len(again) != 0 {
		t.Fatalf("second take = %v, want empty (the record is cleared)", again)
	}
}
