package processenv_test

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// TestSoleReferencedEnvVar pins the grammar this resolver shares with session
// start, which expands config-authored env values with
// [processenv.ExpandSessionEnvValue] — os.Expand, so both ${VAR} and bare
// $VAR count. A resolver reading values any other way silently disagrees with
// the process it describes.
//
// Literal text around a reference is accepted on purpose. A caller changes the
// REFERENCED variable, not the value, so "Bearer ${GW_KEY}" is rotatable: the
// prefix survives expansion and only the secret moves.
func TestSoleReferencedEnvVar(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "braced ref", value: "${ACME_KEY}", want: "ACME_KEY", ok: true},
		{name: "bare ref", value: "$ACME_KEY", want: "ACME_KEY", ok: true},
		{name: "literal prefix", value: "Bearer ${ACME_KEY}", want: "ACME_KEY", ok: true},
		{name: "literal suffix", value: "${ACME_KEY}-suffix", want: "ACME_KEY", ok: true},
		{name: "same ref twice", value: "${ACME_KEY}:${ACME_KEY}", want: "ACME_KEY", ok: true},

		// No variable to change.
		{name: "static literal", value: "sk-ant-literal", ok: false},
		{name: "empty", value: "", ok: false},

		// Two distinct variables: no single one determines the credential.
		{name: "two refs", value: "${ACME_ID}:${ACME_KEY}", ok: false},

		// $$ parses as a reference named "$" under os.Expand, which is not a
		// legal environment variable name.
		{name: "dollar dollar", value: "$$", ok: false},
		{name: "digit-leading name", value: "${9BAD}", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := processenv.SoleReferencedEnvVar(tt.value)
			if ok != tt.ok {
				t.Fatalf("SoleReferencedEnvVar(%q) ok = %v; want %v (name %q)", tt.value, ok, tt.ok, got)
			}
			if got != tt.want {
				t.Errorf("SoleReferencedEnvVar(%q) = %q; want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestSoleReferencedEnvVarControlsExpansion is the anti-vacuity guard, and it
// pins the property that actually matters rather than a proxy for it: when
// this function names a variable, changing THAT variable must change what
// session start hands the harness, and every other byte of the value must
// survive. When it refuses, changing one variable must not be presentable as
// rotating the credential.
//
// Checked against the real expander, not a second copy of the grammar.
func TestSoleReferencedEnvVarControlsExpansion(t *testing.T) {
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
		"",
	} {
		t.Run(value, func(t *testing.T) {
			name, ok := processenv.SoleReferencedEnvVar(value)

			t.Setenv("ACME_ID", "acct-1")
			t.Setenv("ACME_KEY", before)
			expandedBefore := processenv.ExpandSessionEnvValue(value)
			t.Setenv("ACME_KEY", after)
			expandedAfter := processenv.ExpandSessionEnvValue(value)

			if !ok {
				// Refused. Either the value references nothing, or a second
				// variable also feeds it — and then naming ACME_KEY as "the"
				// credential variable would be wrong even though changing it
				// does move the expansion.
				if !processenv.HasEnvRef(value) && expandedBefore != expandedAfter {
					t.Fatalf("refused %q as referencing nothing, yet changing ACME_KEY moved expansion %q -> %q",
						value, expandedBefore, expandedAfter)
				}
				return
			}

			if name != "ACME_KEY" {
				t.Fatalf("SoleReferencedEnvVar(%q) = %q; want ACME_KEY", value, name)
			}
			if expandedBefore == expandedAfter {
				t.Fatalf("SoleReferencedEnvVar(%q) names %s, but changing %s left expansion at %q — writing it would rotate nothing",
					value, name, name, expandedAfter)
			}
			if want := strings.ReplaceAll(expandedBefore, before, after); expandedAfter != want {
				t.Fatalf("expansion of %q went %q -> %q; want %q — literal text around the reference was not preserved",
					value, expandedBefore, expandedAfter, want)
			}
		})
	}
}
