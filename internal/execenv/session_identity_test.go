package execenv

import (
	"slices"
	"testing"
)

// Kills a scrub that misses a session-scoped key, removes a key merely
// sharing a prefix, or modifies its input.
func TestWithoutSessionIdentity(t *testing.T) {
	in := []string{"PATH=/usr/bin", "GC_SESSION_ID_X=keep", "GC_CITY_PATH=/city"}
	for _, k := range SessionScopedEnvKeys {
		in = append(in, k+"=v")
	}
	orig := slices.Clone(in)
	got := WithoutSessionIdentity(in)
	want := []string{"PATH=/usr/bin", "GC_SESSION_ID_X=keep", "GC_CITY_PATH=/city"}
	if !slices.Equal(got, want) {
		t.Fatalf("WithoutSessionIdentity = %v, want %v", got, want)
	}
	if !slices.Equal(in, orig) {
		t.Fatalf("WithoutSessionIdentity modified its input: %v", in)
	}
}
