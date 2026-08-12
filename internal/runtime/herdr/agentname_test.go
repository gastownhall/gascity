package herdr

import (
	"regexp"
	"strings"
	"testing"
)

// herdr ≥0.7.5 rejects agent names that don't match
// ^[a-z][a-z0-9_-]{0,31}$ — gc session names carry rig names verbatim
// ("Indigo--anthony", "CIPcodes--gastown__witness") and can exceed 32 chars,
// so every such session failed `agent start` on every reconcile tick (a
// bounded but hot retry loop found live in the anthony flip). herdrAgentName
// maps any gc session name to a valid, deterministic herdr name.

var validHerdrName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func TestHerdrAgentNameValidNamesPassThrough(t *testing.T) {
	for _, name := range []string{"mayor", "gastown__witness", "kit--anthony", "polecat-gc-wisp-3nvj3yx"} {
		if got := herdrAgentName(name); got != name {
			t.Errorf("herdrAgentName(%q) = %q; want unchanged", name, got)
		}
	}
}

func TestHerdrAgentNameLowercasesAndMapsInvalid(t *testing.T) {
	tests := map[string]string{
		"Indigo--anthony":            "indigo--anthony",
		"CIPcodes--gastown__witness": "cipcodes--gastown__witness",
		"a.b/c":                      "a-b-c",
	}
	for in, want := range tests {
		if got := herdrAgentName(in); got != want {
			t.Errorf("herdrAgentName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestHerdrAgentNameAlwaysValid(t *testing.T) {
	cases := []string{
		"GunnInternships--gastown__refinery", // >32 chars
		"review_pdf_to_latex--gastown__witness",
		"9starts-with-digit",
		"_starts-with-underscore",
		"",
		strings.Repeat("x", 100),
		"ALLCAPS", "Ünïcode--agent",
	}
	for _, in := range cases {
		got := herdrAgentName(in)
		if !validHerdrName.MatchString(got) {
			t.Errorf("herdrAgentName(%q) = %q; not a valid herdr agent name", in, got)
		}
	}
}

func TestHerdrAgentNameLongNamesStayDistinctAndStable(t *testing.T) {
	a := herdrAgentName("GunnInternships--gastown__refinery")
	b := herdrAgentName("GunnInternships--gastown__witnessx")
	if a == b {
		t.Fatalf("distinct long names collided: %q", a)
	}
	if a != herdrAgentName("GunnInternships--gastown__refinery") {
		t.Error("mapping is not deterministic")
	}
}
