package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestFilterRunBeads pins the projection-boundary filter policy: engineering
// types and gc.kind=run roots are kept, while gc:-labeled control beads and
// non-run bookkeeping types (message/session) are dropped. This is the analog of
// the frontend runBeadFilter (summary.ts) that the live tailer applies
// before the pure summary/detail builders.
func TestFilterRunBeads(t *testing.T) {
	in := []beads.Bead{
		{ID: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
		{ID: "task", Type: "task"},
		{ID: "bug", Type: "bug"},
		{ID: "ctl", Type: "task", Labels: []string{"gc:control"}},
		{ID: "msg", Type: "message", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		{ID: "sess", Type: "session"},
		{ID: "run-labeled", Type: "molecule", Labels: []string{"gc:workflow"}, Metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
	}

	got := FilterRunBeads(in)

	gotIDs := map[string]bool{}
	for _, b := range got {
		gotIDs[b.ID] = true
	}
	want := map[string]bool{"root": true, "task": true, "bug": true}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("FilterRunBeads dropped %q; it should be kept", id)
		}
	}
	for _, id := range []string{"ctl", "msg", "sess", "run-labeled"} {
		if gotIDs[id] {
			t.Errorf("FilterRunBeads kept %q; a gc:-labeled or non-run bead must be dropped", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("FilterRunBeads returned %d beads, want %d (%v)", len(got), len(want), gotIDs)
	}
}
