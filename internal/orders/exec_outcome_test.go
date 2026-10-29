package orders

import (
	"strings"
	"testing"
)

func TestDecodeExecOutcomeAcceptsDeclarations(t *testing.T) {
	t.Parallel()

	got, ok, err := DecodeExecOutcome([]byte(`{"outcome":"partial","reason":"some scopes or steps did not run","scopes":[{"scope":"rig:r1","reason":"bead store unreachable"}]}` + "\n"))
	if err != nil || !ok {
		t.Fatalf("DecodeExecOutcome() = ok %v, err %v; want a declaration", ok, err)
	}
	if got.Outcome != ExecOutcomePartial || got.Reason != "some scopes or steps did not run" {
		t.Fatalf("DecodeExecOutcome() = %+v", got)
	}
	if len(got.Scopes) != 1 || got.Scopes[0].Scope != "rig:r1" || got.Scopes[0].Reason != "bead store unreachable" {
		t.Fatalf("DecodeExecOutcome().Scopes = %+v", got.Scopes)
	}

	skipped, ok, err := DecodeExecOutcome([]byte(`{"outcome":"skipped","reason":"no bead scope was reachable","scopes":[]}`))
	if err != nil || !ok || skipped.Outcome != ExecOutcomeSkipped {
		t.Fatalf("DecodeExecOutcome(skipped) = %+v, ok %v, err %v", skipped, ok, err)
	}
}

func TestDecodeExecOutcomeEmptyMeansNoDeclaration(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "  \n"} {
		_, ok, err := DecodeExecOutcome([]byte(raw))
		if err != nil || ok {
			t.Fatalf("DecodeExecOutcome(%q) = ok %v, err %v; want no declaration and no error", raw, ok, err)
		}
	}
}

func TestDecodeExecOutcomeRejectsMalformedDeclarations(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not json":         `skipped`,
		"unknown outcome":  `{"outcome":"done","reason":"x","scopes":[]}`,
		"missing outcome":  `{"reason":"x","scopes":[]}`,
		"unknown field":    `{"outcome":"skipped","reason":"x","scopes":[],"extra":1}`,
		"scope without id": `{"outcome":"partial","reason":"x","scopes":[{"scope":"","reason":"y"}]}`,
		"trailing value":   `{"outcome":"skipped","reason":"x","scopes":[]} {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeExecOutcome([]byte(raw)); err == nil {
				t.Fatalf("DecodeExecOutcome(%s) accepted %q", name, raw)
			}
		})
	}
}

func TestExecOutcomeFileEnvIsReserved(t *testing.T) {
	t.Parallel()

	if !IsReservedExecEnvKey(ExecOutcomeFileEnv) {
		t.Fatalf("%s must be controller-owned", ExecOutcomeFileEnv)
	}
	err := ValidateExecEnvOverrides(Order{Name: "o", Env: map[string]string{ExecOutcomeFileEnv: "/tmp/x"}})
	if err == nil || !strings.Contains(err.Error(), ExecOutcomeFileEnv) {
		t.Fatalf("ValidateExecEnvOverrides() = %v, want a reserved-key refusal", err)
	}
}
