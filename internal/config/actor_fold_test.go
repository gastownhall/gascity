package config

import "testing"

// TestCanonicalActor pins the fold to the bead store's actor comparison
// (beads' canonicalActor) and to the worked examples of
// docs/reference/specs/identity-separator-contract-v1.md.
func TestCanonicalActor(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Empty stays empty.
		{"", ""},
		// Contract §2 and §5: "/" and its "--" encoding fold alike; "." and
		// its "__" encoding fold alike; the two axes stay distinct.
		{"mayor", "mayor"},
		{"hello-world/polecat", "hello_world/polecat"},
		{"hello-world--polecat", "hello_world/polecat"},
		{"hello_world__polecat", "hello_world_polecat"},
		{"gastown.mayor", "gastown_mayor"},
		{"gastown__mayor", "gastown_mayor"},
		{"gastown/mayor", "gastown/mayor"},
		{"gastown--mayor", "gastown/mayor"},
		// Only an exact two-byte "--" run is the rig boundary.
		{"x--y", "x/y"},
		{"a---b", "a_b"},
		{"a----b", "a_b"},
		{"a-_b", "a_b"},
		{"a.-b", "a_b"},
		{"a_-_b", "a_b"},
		// Single separators collapse to "_".
		{"a.b", "a_b"},
		{"a-b", "a_b"},
		{"a_b", "a_b"},
		// Leading and trailing runs fold too.
		{"--x", "/x"},
		{"x--", "x/"},
		{".x.", "_x_"},
		// Case is kept; the store compares case-sensitively.
		{"Beta/Worker", "Beta/Worker"},
		{"BETA--W", "BETA/W"},
		// A raw "/" passes through and is not merged with neighbors.
		{"a/-b", "a/_b"},
		{"a//b", "a//b"},
		// Multi-byte runes pass through untouched.
		{"ré-gion--wörker", "ré_gion/wörker"},
		// Spaces are not separators.
		{" a b ", " a b "},
	}
	for _, tc := range cases {
		if got := CanonicalActor(tc.in); got != tc.want {
			t.Errorf("CanonicalActor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalActorIsIdempotent(t *testing.T) {
	for _, in := range []string{"beta--pk__worker", "a---b", "x.y-z_w", "Beta/x"} {
		once := CanonicalActor(in)
		// A second fold must not change a value that has no "--" left.
		if twice := CanonicalActor(once); twice != once {
			t.Errorf("CanonicalActor(CanonicalActor(%q)) = %q, want %q", in, twice, once)
		}
	}
}
