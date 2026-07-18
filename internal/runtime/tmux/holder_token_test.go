package tmux

import "testing"

// TestEnsureInstanceTokenAlignsHolderTokenOnMint pins the backstop invariant:
// when GC_INSTANCE_TOKEN is absent the backstop mints one AND sets a matching
// BEADS_HOLDER_TOKEN, so an unmanaged/legacy start never runs with an instance
// token but a divergent (or absent) holder token — the silent actor-only
// downgrade the DESIGN calls out.
func TestEnsureInstanceTokenAlignsHolderTokenOnMint(t *testing.T) {
	env, err := ensureInstanceToken(nil)
	if err != nil {
		t.Fatal(err)
	}
	gc := env["GC_INSTANCE_TOKEN"]
	if gc == "" {
		t.Fatal("backstop did not mint GC_INSTANCE_TOKEN")
	}
	if env["BEADS_HOLDER_TOKEN"] != gc {
		t.Errorf("BEADS_HOLDER_TOKEN = %q, want minted GC_INSTANCE_TOKEN %q", env["BEADS_HOLDER_TOKEN"], gc)
	}
}

// TestEnsureInstanceTokenRealignsStaleHolderToken proves the backstop enforces
// the invariant even when a stale/mismatched BEADS_HOLDER_TOKEN rides in: it is
// realigned to the current GC_INSTANCE_TOKEN, never left to diverge.
func TestEnsureInstanceTokenRealignsStaleHolderToken(t *testing.T) {
	env, err := ensureInstanceToken(map[string]string{
		"GC_INSTANCE_TOKEN":  "managed-tok",
		"BEADS_HOLDER_TOKEN": "stale-mismatch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if env["GC_INSTANCE_TOKEN"] != "managed-tok" {
		t.Fatalf("GC_INSTANCE_TOKEN changed to %q, want managed-tok", env["GC_INSTANCE_TOKEN"])
	}
	if env["BEADS_HOLDER_TOKEN"] != "managed-tok" {
		t.Errorf("BEADS_HOLDER_TOKEN = %q, want realigned to GC_INSTANCE_TOKEN managed-tok", env["BEADS_HOLDER_TOKEN"])
	}
}
