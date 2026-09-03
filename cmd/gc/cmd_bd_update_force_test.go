package main

import "testing"

// TestBdMutationWriteIDsAcceptsUpdateForce guards the guard: the documented
// unassign-and-block idiom must scan to a clean single id, not fail closed as
// ambiguous. A regression here is silent — gc exits 1 having written nothing,
// with a message about bead-ID safety rather than about the flag.
func TestBdMutationWriteIDsAcceptsUpdateForce(t *testing.T) {
	args := []string{"update", "cr-abc12", "--status=blocked", "--force", "-a", ""}
	ids, ok, ambiguous := bdMutationWriteIDs(args)
	if !ok {
		t.Fatalf("update not recognized as a write mutation")
	}
	if ambiguous {
		t.Fatalf("`gc bd %v` scans as ambiguous, so gc aborts without writing", args)
	}
	if len(ids) != 1 || ids[0] != "cr-abc12" {
		t.Fatalf("ids = %v, want [cr-abc12]", ids)
	}
}
