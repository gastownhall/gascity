package main

import (
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestUnknownStateRefusal: the controller's State refuses the API wake and
// reset of an open row in a state it skips, naming the raw state and the close
// that releases it, and refuses no row it manages, no closed row (whatever
// close code its state carries) and no interrupted close the reconciler
// finishes. Kills an API wake or reset that queues what the controller never
// applies, and a refusal that sends an operator to close an already-closed row
// or one the reconciler is about to restore.
func TestUnknownStateRefusal(t *testing.T) {
	cs := &controllerState{}
	for _, state := range []string{"future-state", "archived"} {
		got := cs.UnknownStateRefusal(sessionpkg.Info{ID: "gc-1", MetadataState: state})
		if !strings.Contains(got, `"`+state+`"`) || !strings.Contains(got, "gc session close gc-1") {
			t.Fatalf("UnknownStateRefusal(%q) = %q, want the state and `gc session close gc-1` named", state, got)
		}
	}
	for _, info := range []sessionpkg.Info{
		{ID: "gc-1", MetadataState: ""},
		{ID: "gc-1", MetadataState: "awake"},
		{ID: "gc-1", MetadataState: "asleep"},
		{ID: "gc-1", MetadataState: "gc_swept", Closed: true},
		{ID: "gc-1", MetadataState: "dead-runtime"},
	} {
		if got := cs.UnknownStateRefusal(info); got != "" {
			t.Fatalf("UnknownStateRefusal(state=%q closed=%v) = %q, want none", info.MetadataState, info.Closed, got)
		}
	}
}
