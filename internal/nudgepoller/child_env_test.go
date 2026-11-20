package nudgepoller

import (
	"slices"
	"testing"
)

// Kills: a poller child that keeps its spawner's GC_SESSION_ID or
// GC_RUNTIME_EPOCH (the process-table orphan sweep would read it as that
// session's leaked process), or that drops anything else.
func TestChildEnvDropsSessionIdentity(t *testing.T) {
	in := []string{"PATH=/usr/bin", "GC_SESSION_ID=gc-1", "GC_RUNTIME_EPOCH=4", "GC_CITY_PATH=/city", "GC_SESSION_ID_X=keep"}
	got := ChildEnv(in)
	want := []string{"PATH=/usr/bin", "GC_CITY_PATH=/city", "GC_SESSION_ID_X=keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("ChildEnv = %q, want %q", got, want)
	}
	if len(in) != 5 || in[1] != "GC_SESSION_ID=gc-1" {
		t.Fatalf("ChildEnv mutated its input: %q", in)
	}
}

// Kills: a poller child that keeps the GC_HOOK_DEADLINE of the hook that
// spawned it. The poller outlives that hook by design, so the deadline is long
// past for it, and anything it runs would read itself as out of time.
func TestChildEnvDropsHookDeadline(t *testing.T) {
	got := ChildEnv([]string{"PATH=/usr/bin", "GC_HOOK_DEADLINE=2026-10-11T12:00:00Z", "GC_HOOK_DEADLINE_X=keep"})
	want := []string{"PATH=/usr/bin", "GC_HOOK_DEADLINE_X=keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("ChildEnv = %q, want %q", got, want)
	}
}
