package exec //nolint:revive // internal package, always imported with alias

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestExecStoreDeclaresPayloadBlindOnlyWhenGranted pins S5 §1.4: whether an
// exec provider is bd-backed is the opener's knowledge, so a store declares it
// carries no dependency payload only once the opener grants it, and through a
// typed handle it answers the same.
func TestExecStoreDeclaresPayloadBlindOnlyWhenGranted(t *testing.T) {
	store := NewStore("/opt/providers/gc-beads-file")
	if _, absent := beads.DepMetadataAbsent(store); absent {
		t.Fatal("an exec store declared itself payload-blind before its opener granted it")
	}
	store.DeclarePayloadBlind()
	protocol, absent := beads.DepMetadataAbsent(store)
	if !absent || protocol != "exec" {
		t.Fatalf("after the grant: (%q, %v), want (exec, true)", protocol, absent)
	}
	if _, absent := beads.DepMetadataAbsent(beads.WorkStore{Store: store}); !absent {
		t.Error("a work handle over a granted exec store hides its declaration")
	}
	if _, absent := beads.DepMetadataAbsent(beads.NewMemStore()); absent {
		t.Error("a store that declares nothing reads as payload-blind")
	}
}
