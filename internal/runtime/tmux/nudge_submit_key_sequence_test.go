package tmux

import (
	"slices"
	"testing"
)

// TestNudgeSubmitKeySequenceForFamilyDefaultsToEnter pins the declarative
// table's fallback: a family with no explicit entry in
// nudgeSubmitKeySequences gets the single-Enter default, matching every
// provider's historical behavior before this table existed. codex is
// deliberately absent from this list — it is the one family with a declared
// entry (Escape then Enter, upstream #4706); see codex_submit_test.go.
func TestNudgeSubmitKeySequenceForFamilyDefaultsToEnter(t *testing.T) {
	for _, family := range []string{"claude", "gemini", "", "some-unregistered-family"} {
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

// TestNudgeSubmitRetryKeySequenceKeepsOnlyTheCommitKey pins the retry
// derivation (ga-oqhooe): a multi-key sequence's leading keys can fuse with the
// commit key when an attempt's inter-key gap collapses, so a retry sends the
// commit key alone. A single-key sequence is its own commit key.
func TestNudgeSubmitRetryKeySequenceKeepsOnlyTheCommitKey(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{"escape then enter", []string{"Escape", "Enter"}, []string{"Enter"}},
		{"three keys", []string{"Escape", "C-u", "Enter"}, []string{"Enter"}},
		{"single key", []string{"Enter"}, []string{"Enter"}},
		{"empty", []string{}, []string{}},
		{"nil", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orig := slices.Clone(tt.in)
			if got := nudgeSubmitRetryKeySequence(tt.in); !slices.Equal(got, tt.want) {
				t.Fatalf("nudgeSubmitRetryKeySequence(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !slices.Equal(tt.in, orig) {
				t.Fatalf("input mutated: %q, was %q", tt.in, orig)
			}
		})
	}
}

// TestNudgeSubmitRetryKeySequenceNeverInterrupts: every family's retry is
// free of the keys that interrupt a running turn, and the default family's
// retry equals its first submit, so no existing family's behavior changes.
func TestNudgeSubmitRetryKeySequenceNeverInterrupts(t *testing.T) {
	for family, seq := range nudgeSubmitKeySequences {
		if retry := nudgeSubmitRetryKeySequence(seq); len(retry) != 1 || submitSequenceInterrupts(retry) {
			t.Errorf("family %q retry = %q, want one non-interrupting commit key", family, retry)
		}
	}
	def := defaultNudgeSubmitKeySequence
	if got := nudgeSubmitRetryKeySequence(def); !slices.Equal(got, def) {
		t.Errorf("default family retry = %q, want %q (unchanged)", got, def)
	}
}
