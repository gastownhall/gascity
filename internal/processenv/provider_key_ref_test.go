package processenv_test

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// TestReferencedEnvVars pins the grammar this resolver shares with session
// start, which expands config-authored env values with
// [processenv.ExpandSessionEnvValue] — os.Expand, so both ${VAR} and bare
// $VAR count. A resolver reading values any other way silently disagrees with
// the process it describes.
//
// Literal text around a reference is irrelevant to the count. A caller changes
// the REFERENCED variable, not the value, so "Bearer ${GW_KEY}" references
// exactly GW_KEY: the prefix survives expansion and only the secret moves.
func TestReferencedEnvVars(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "braced ref", value: "${ACME_KEY}", want: []string{"ACME_KEY"}},
		{name: "bare ref", value: "$ACME_KEY", want: []string{"ACME_KEY"}},
		{name: "literal prefix", value: "Bearer ${ACME_KEY}", want: []string{"ACME_KEY"}},
		{name: "literal suffix", value: "${ACME_KEY}-suffix", want: []string{"ACME_KEY"}},
		{name: "same ref twice", value: "${ACME_KEY}:${ACME_KEY}", want: []string{"ACME_KEY"}},

		// No variable to change.
		{name: "static literal", value: "sk-ant-literal"},
		{name: "empty", value: ""},

		// Two distinct variables: no single one determines the value. Order is
		// first appearance, so a caller can name them back in the order the
		// operator wrote them.
		{name: "two refs", value: "${ACME_ID}:${ACME_KEY}", want: []string{"ACME_ID", "ACME_KEY"}},
		{name: "two refs, first repeated", value: "${ACME_KEY}:${ACME_ID}:${ACME_KEY}", want: []string{"ACME_KEY", "ACME_ID"}},

		// $$ parses as a reference named "$" under os.Expand, which is not a
		// legal environment variable name.
		{name: "dollar dollar", value: "$$"},
		{name: "digit-leading name", value: "${9BAD}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processenv.ReferencedEnvVars(tt.value)
			if len(got) != len(tt.want) {
				t.Fatalf("ReferencedEnvVars(%q) = %v; want %v", tt.value, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ReferencedEnvVars(%q) = %v; want %v", tt.value, got, tt.want)
				}
			}
		})
	}
}

// TestReferencedEnvVarsControlsExpansion is the anti-vacuity guard, and it
// pins the property that actually matters rather than a proxy for it: every
// variable this function names must, when changed, change what session start
// hands the harness — and when it names exactly one, every other byte of the
// value must survive. When it names none, no variable may move the expansion.
//
// Checked against the real expander, not a second copy of the grammar.
func TestReferencedEnvVarsControlsExpansion(t *testing.T) {
	const before = "sk-ant-old"
	const after = "sk-ant-new"

	for _, value := range []string{
		"${ACME_KEY}",
		"$ACME_KEY",
		"Bearer ${ACME_KEY}",
		"${ACME_KEY}-suffix",
		"${ACME_KEY}:${ACME_KEY}",
		"${ACME_ID}:${ACME_KEY}",
		"sk-ant-literal",
		"$$",
		"",
	} {
		t.Run(value, func(t *testing.T) {
			refs := processenv.ReferencedEnvVars(value)

			t.Setenv("ACME_ID", "acct-1")
			t.Setenv("ACME_KEY", before)
			expandedBefore := processenv.ExpandSessionEnvValue(value)
			t.Setenv("ACME_KEY", after)
			expandedAfter := processenv.ExpandSessionEnvValue(value)

			names := strings.Join(refs, ",")
			moved := expandedBefore != expandedAfter

			if len(refs) == 0 {
				if moved {
					t.Fatalf("ReferencedEnvVars(%q) named nothing, yet changing ACME_KEY moved expansion %q -> %q",
						value, expandedBefore, expandedAfter)
				}
				return
			}

			// Every named variable must be live: ACME_KEY is the only one this
			// loop varies, so it is the only one whose liveness can be checked
			// here, and it must move the expansion whenever it is named.
			if strings.Contains(names, "ACME_KEY") && !moved {
				t.Fatalf("ReferencedEnvVars(%q) names ACME_KEY, but changing it left expansion at %q — acting on that name would rotate nothing",
					value, expandedAfter)
			}
			if !strings.Contains(names, "ACME_KEY") && moved {
				t.Fatalf("ReferencedEnvVars(%q) = %v omits ACME_KEY, yet changing it moved expansion %q -> %q",
					value, refs, expandedBefore, expandedAfter)
			}

			if len(refs) != 1 {
				// More than one variable feeds the value, so naming any single
				// one as "the" source would be wrong even though changing it
				// does move the expansion. Nothing further to pin.
				return
			}
			if want := strings.ReplaceAll(expandedBefore, before, after); expandedAfter != want {
				t.Fatalf("expansion of %q went %q -> %q; want %q — literal text around the sole reference was not preserved",
					value, expandedBefore, expandedAfter, want)
			}
		})
	}
}
