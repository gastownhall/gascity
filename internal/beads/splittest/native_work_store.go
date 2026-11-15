package splittest

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// NewNativeWorkStore opens a REAL bd/Dolt work store: an embedded Dolt
// workspace in a temporary directory, opened through the linked beads library
// exactly as gc's native store opens a city's work ledger, minting under
// prefix.
//
// It exists for the questions the kit's doubles cannot answer, because the
// doubles model bd's WRITE strictness, not its READ semantics. The one that
// matters for a split city is a blocking edge whose target the store does not
// hold: BdSemantics refuses to write it at all, but real bd accepts it on its
// other doors (a create-time dependency, an import, an edge the migration
// clear kept) and then classifies a cross-prefix target as external and never
// blocks on it (beads v1.3.2, issueops/dependencies.go). A reader that must
// enforce a cross-store rule over that answer can only be tested against the
// backend that gives it.
//
// The store is returned unwrapped (no strict layer, no bead policy) so the
// backend's own answer is what a test sees. The environment is made hermetic
// first: HOME is a temporary directory and the ambient BEADS_ server switches
// are cleared, so the workspace is always the embedded backend and never a
// server some other process runs. A build whose beads library cannot open an
// embedded workspace skips the test rather than failing it, as the other real
// workspace proofs in this tree do.
func NewNativeWorkStore(t *testing.T, prefix string) *beads.NativeDoltStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"BEADS_DIR", "BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_SHARED_SERVER", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BD_EVENTS_JOURNAL"} {
		t.Setenv(key, "")
	}
	ctx := context.Background()
	root := t.TempDir()
	seed, err := beads.OpenNativeStorage(ctx, root, nil)
	if err != nil {
		t.Skipf("the linked beads library cannot open an embedded workspace here: %v", err)
	}
	if err := seed.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		_ = seed.Close()
		t.Fatalf("configuring the workspace id prefix %q: %v", prefix, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the workspace seed: %v", err)
	}
	store, err := beads.OpenNativeDoltStoreAt(ctx, root, nil)
	if err != nil {
		t.Fatalf("opening the native work store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("closing the native work store: %v", err)
		}
	})
	return store
}
