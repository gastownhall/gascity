package session

import "testing"

// TestRuntimeEnvWithSessionContextAlignsBeadsActorWithAlias pins the #5048
// invariant: BEADS_ACTOR (what bd --claim writes) must equal the alias-first
// ownership identity already exported as GC_ALIAS / GC_AGENT — never a
// sanitized-only form that the session no longer polls.
func TestRuntimeEnvWithSessionContextAlignsBeadsActorWithAlias(t *testing.T) {
	env := RuntimeEnvWithSessionContext(
		"ma-wisp-dw8d64",
		"bd__dog-ma-wisp-dw8d64",
		"bd.dog-1",
		"bd.dog",
		"ephemeral",
		1,
		1,
		"tok",
	)
	if got := env["GC_ALIAS"]; got != "bd.dog-1" {
		t.Fatalf("GC_ALIAS = %q, want bd.dog-1", got)
	}
	if got := env["GC_AGENT"]; got != "bd.dog-1" {
		t.Fatalf("GC_AGENT = %q, want bd.dog-1", got)
	}
	if got := env["BEADS_ACTOR"]; got != "bd.dog-1" {
		t.Fatalf("BEADS_ACTOR = %q, want bd.dog-1 (must match GC_ALIAS for claim/poll agreement)", got)
	}
	if got := env["GC_SESSION_NAME"]; got != "bd__dog-ma-wisp-dw8d64" {
		t.Fatalf("GC_SESSION_NAME = %q, want sanitized handle", got)
	}
}

func TestRuntimeEnvWithSessionContextFallsBackToSessionNameWhenUnaliased(t *testing.T) {
	env := RuntimeEnvWithSessionContext(
		"s1",
		"worker-s1",
		"",
		"worker",
		"ephemeral",
		1,
		1,
		"tok",
	)
	if got := env["BEADS_ACTOR"]; got != "worker-s1" {
		t.Fatalf("BEADS_ACTOR = %q, want session name fallback worker-s1", got)
	}
	if got := env["GC_AGENT"]; got != "worker-s1" {
		t.Fatalf("GC_AGENT = %q, want session name fallback worker-s1", got)
	}
}
