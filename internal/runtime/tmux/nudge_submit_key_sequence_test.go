package tmux

import "testing"

// TestNudgeSubmitKeySequenceForFamilyDefaultsToEnter pins the declarative
// table's fallback: a family with no explicit entry in
// nudgeSubmitKeySequences gets the single-Enter default, matching every
// provider's historical behavior before this table existed.
func TestNudgeSubmitKeySequenceForFamilyDefaultsToEnter(t *testing.T) {
	for _, family := range []string{"claude", "codex", "gemini", "", "some-unregistered-family"} {
		got := nudgeSubmitKeySequenceForFamily(family)
		if len(got) != 1 || got[0] != "Enter" {
			t.Errorf("nudgeSubmitKeySequenceForFamily(%q) = %v, want [Enter] (no entries are registered today)", family, got)
		}
	}
}

// TestNudgeSubmitKeySequenceForFamilyHonorsTableEntry proves the lookup
// actually reads nudgeSubmitKeySequences rather than always returning the
// default — this is the mechanism a future claude-specific (or codex, per
// upstream #4706) fix would rely on.
func TestNudgeSubmitKeySequenceForFamilyHonorsTableEntry(t *testing.T) {
	orig := nudgeSubmitKeySequences
	nudgeSubmitKeySequences = map[string][]string{"testfam": {"Escape", "Enter"}}
	defer func() { nudgeSubmitKeySequences = orig }()

	got := nudgeSubmitKeySequenceForFamily("testfam")
	want := []string{"Escape", "Enter"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("nudgeSubmitKeySequenceForFamily(testfam) = %v, want %v", got, want)
	}
	// An unrelated family is unaffected by testfam's entry.
	if got := nudgeSubmitKeySequenceForFamily("claude"); len(got) != 1 || got[0] != "Enter" {
		t.Fatalf("nudgeSubmitKeySequenceForFamily(claude) = %v, want [Enter]", got)
	}
}
