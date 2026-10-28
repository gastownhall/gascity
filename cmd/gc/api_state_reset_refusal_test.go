package main

import (
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestResetRefusal: the controller's State refuses the API reset of a row in a
// state it skips, naming the raw state and the close that releases it, and
// refuses no row it manages. Kills an API reset that queues a fresh restart
// the controller never applies.
func TestResetRefusal(t *testing.T) {
	cs := &controllerState{}
	for _, state := range []string{"future-state", "archived"} {
		got := cs.ResetRefusal(sessionpkg.Info{ID: "gc-1", MetadataState: state})
		if !strings.Contains(got, `"`+state+`"`) || !strings.Contains(got, "gc session close gc-1") {
			t.Fatalf("ResetRefusal(%q) = %q, want the state and `gc session close gc-1` named", state, got)
		}
	}
	for _, state := range []string{"", "awake", "asleep"} {
		if got := cs.ResetRefusal(sessionpkg.Info{ID: "gc-1", MetadataState: state}); got != "" {
			t.Fatalf("ResetRefusal(%q) = %q, want none", state, got)
		}
	}
}
