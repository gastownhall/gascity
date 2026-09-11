package main

import "testing"

// transportForRuntimeName fixes the Transport axis from the Runtime selection
// name (transport is bundled with the runtime today). Pin the mapping so the
// Resolver-seam population stays honest.
func TestTransportForRuntimeName(t *testing.T) {
	cases := map[string]string{
		"acp":          "acp",
		"tmux":         "tmux",
		"k8s":          "tmux",
		"subprocess":   "tmux",
		"ssh:host":     "tmux",
		"exec:/x/pack": "tmux",
		"hybrid":       "tmux",
		"t3bridge":     "t3",
		"":             "tmux",
	}
	for name, want := range cases {
		if got := transportForRuntimeName(name); got != want {
			t.Errorf("transportForRuntimeName(%q) = %q, want %q", name, got, want)
		}
	}
}
