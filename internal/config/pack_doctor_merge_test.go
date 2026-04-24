package config

import "testing"

// Tests for appendDiscoveredDoctors merge semantics.
//
// The merge matters because two discovery paths can produce entries for
// the same check: convention-based (doctor/<name>/run.sh) and legacy TOML
// ([[doctor]] with script = "..."). When both fire for the same pack, the
// earlier-appended one wins the dedup. The merge preserves FixScript from
// the suppressed duplicate so CanFix does not spuriously return false on
// the winning entry.

func TestAppendDiscoveredDoctors_AppendsNew(t *testing.T) {
	a := DiscoveredDoctor{Name: "check-a", RunScript: "/a.sh"}
	b := DiscoveredDoctor{Name: "check-b", RunScript: "/b.sh"}

	got := appendDiscoveredDoctors(nil, a, b)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

func TestAppendDiscoveredDoctors_DedupesOnNameAndRunScript(t *testing.T) {
	first := DiscoveredDoctor{Name: "same", RunScript: "/path.sh"}
	second := DiscoveredDoctor{Name: "same", RunScript: "/path.sh"}

	got := appendDiscoveredDoctors(nil, first, second)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 (dedup)", len(got))
	}
}

func TestAppendDiscoveredDoctors_MergesFixScriptFromDuplicate(t *testing.T) {
	// Convention-discovered entry appends first without a fix script…
	convention := DiscoveredDoctor{
		Name:      "same",
		RunScript: "/path.sh",
		FixScript: "",
	}
	// …then the legacy TOML entry for the same check with fix declared.
	legacy := DiscoveredDoctor{
		Name:      "same",
		RunScript: "/path.sh",
		FixScript: "/legacy-fix.sh",
	}

	got := appendDiscoveredDoctors(nil, convention, legacy)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 (dedup)", len(got))
	}
	if got[0].FixScript != "/legacy-fix.sh" {
		t.Fatalf("FixScript = %q, want %q (merge from suppressed duplicate)",
			got[0].FixScript, "/legacy-fix.sh")
	}
}

func TestAppendDiscoveredDoctors_PreservesExistingFixScript(t *testing.T) {
	// If the winning entry already has a fix, don't let a sparse
	// duplicate clear it.
	winner := DiscoveredDoctor{
		Name:      "same",
		RunScript: "/path.sh",
		FixScript: "/keep.sh",
	}
	sparse := DiscoveredDoctor{
		Name:      "same",
		RunScript: "/path.sh",
		FixScript: "",
	}

	got := appendDiscoveredDoctors(nil, winner, sparse)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].FixScript != "/keep.sh" {
		t.Fatalf("FixScript = %q, want %q (should not be cleared)",
			got[0].FixScript, "/keep.sh")
	}
}

func TestAppendDiscoveredDoctors_DistinguishesByBindingName(t *testing.T) {
	// Same Name + RunScript but different BindingName = two distinct
	// checks (same pack reachable under two imports).
	a := DiscoveredDoctor{
		Name:        "same",
		RunScript:   "/path.sh",
		BindingName: "alpha",
	}
	b := DiscoveredDoctor{
		Name:        "same",
		RunScript:   "/path.sh",
		BindingName: "beta",
	}

	got := appendDiscoveredDoctors(nil, a, b)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (distinct binding names)", len(got))
	}
}
