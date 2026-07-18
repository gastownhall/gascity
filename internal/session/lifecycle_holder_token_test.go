package session

import "testing"

// TestRuntimeEnvSetsHolderTokenFromInstanceToken pins the single authoritative
// wiring point: RuntimeEnv derives BEADS_HOLDER_TOKEN from the same
// instanceToken it stamps into GC_INSTANCE_TOKEN, so a claim made by the session
// records an incarnation-unique holder token that matches its instance token.
func TestRuntimeEnvSetsHolderTokenFromInstanceToken(t *testing.T) {
	env := RuntimeEnv("sid", "sname", DefaultGeneration, DefaultContinuationEpoch, "tok-123")
	if got := env["BEADS_HOLDER_TOKEN"]; got != "tok-123" {
		t.Errorf("BEADS_HOLDER_TOKEN = %q, want tok-123", got)
	}
	// The holder token IS the instance token — they must never diverge.
	if env["BEADS_HOLDER_TOKEN"] != env["GC_INSTANCE_TOKEN"] {
		t.Errorf("BEADS_HOLDER_TOKEN %q != GC_INSTANCE_TOKEN %q", env["BEADS_HOLDER_TOKEN"], env["GC_INSTANCE_TOKEN"])
	}
}

// TestRuntimeEnvVariantsPropagateHolderToken proves the alias/context variants,
// which build on RuntimeEnv, carry the holder token too.
func TestRuntimeEnvVariantsPropagateHolderToken(t *testing.T) {
	alias := RuntimeEnvWithAlias("sid", "sname", "al", DefaultGeneration, DefaultContinuationEpoch, "tok-a")
	if alias["BEADS_HOLDER_TOKEN"] != "tok-a" {
		t.Errorf("WithAlias BEADS_HOLDER_TOKEN = %q, want tok-a", alias["BEADS_HOLDER_TOKEN"])
	}
	ctx := RuntimeEnvWithSessionContext("sid", "sname", "al", "tmpl", "cli", DefaultGeneration, DefaultContinuationEpoch, "tok-c")
	if ctx["BEADS_HOLDER_TOKEN"] != "tok-c" {
		t.Errorf("WithSessionContext BEADS_HOLDER_TOKEN = %q, want tok-c", ctx["BEADS_HOLDER_TOKEN"])
	}
}
