package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestRunMembers(t *testing.T) {
	root := beads.Bead{ID: "run1", Type: "molecule"}
	childByParent := beads.Bead{ID: "c1", ParentID: "run1"}
	childByRootMeta := beads.Bead{ID: "c2", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "run1"}}
	childByPrefix := beads.Bead{ID: "run1.step3", Type: "task"}
	unrelated := beads.Bead{ID: "other", ParentID: "run2"}
	otherRoot := beads.Bead{ID: "run2", Type: "molecule"}

	beadList := []beads.Bead{root, childByParent, unrelated, childByRootMeta, otherRoot, childByPrefix}

	got := RunMembers(beadList, "run1")
	gotIDs := make(map[string]bool, len(got))
	for _, b := range got {
		gotIDs[b.ID] = true
	}

	want := []string{"run1", "c1", "c2", "run1.step3"}
	if len(got) != len(want) {
		t.Fatalf("RunMembers returned %d beads (%v), want %d (%v)", len(got), gotIDs, len(want), want)
	}
	for _, id := range want {
		if !gotIDs[id] {
			t.Errorf("RunMembers missing member %q; got %v", id, gotIDs)
		}
	}
	if gotIDs["other"] || gotIDs["run2"] {
		t.Errorf("RunMembers included a non-member; got %v", gotIDs)
	}
}

func TestRunMembersEmptyRoot(t *testing.T) {
	if got := RunMembers(nil, ""); got != nil {
		t.Fatalf("RunMembers(nil, \"\") = %v, want nil", got)
	}
	if got := RunMembers([]beads.Bead{{ID: "x"}}, ""); len(got) != 0 {
		t.Fatalf("RunMembers with empty rootID returned %d, want 0", len(got))
	}
}
