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
