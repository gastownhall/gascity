package beads

// The pinned-id fence.
//
// A class binding claims a namespace, and the residency resolver is allowed to
// retire its residence probe once that claim is true — once every bead in the
// binding can be recognized from its id alone. Minting takes care of the ids
// the store generates. It says nothing about the ids a CALLER pins: Create
// honors an explicit id verbatim, so without a fence any caller can write a
// "ga-…" bead into the graph binding and the claim quietly stops holding. The
// bead is then invisible to every id-shaped lookup and lives in a store that
// says it does not hold such things.
//
// The fence is a namespace check, not an equality check: a binding holds more
// than it mints (the nudge queue's "gcnq-" records inside the nudges store), so
// the store is opened with the namespace SET it serves rather than with its
// mint prefix alone.
//
// CreateWithForeignID is the deliberate exception and stays open. It is the
// store-migration copy path, whose entire job is to carry a preserved foreign
// id across — and the beads it writes are exactly the relics
// ClassBinding.HasLegacyResidents exists to keep the probe alive for.

import (
	"strings"
	"testing"
)

func openFencedStore(t *testing.T, prefixes ...string) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir(),
		WithSQLiteStoreIDPrefix("gcn"),
		WithSQLiteStoreReservedIDPrefixes(prefixes...),
	)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	store, ok := opened.(*SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned %T, want *SQLiteStore", opened)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

func TestFenceRefusesAPinnedForeignID(t *testing.T) {
	store := openFencedStore(t, "gcn", "gcnq")
	_, err := store.Create(Bead{ID: "ga-42", Title: "a work id pinned into the nudges binding"})
	if err == nil {
		t.Fatal("Create accepted a foreign pinned id; the binding's namespace claim is now false and the bead is unreachable by id")
	}
	if !strings.Contains(err.Error(), "ga-42") || !strings.Contains(err.Error(), "gcn") {
		t.Errorf("refusal %q names neither the id nor the namespaces it must carry", err)
	}
}

func TestFenceAcceptsEveryNamespaceTheBindingHolds(t *testing.T) {
	store := openFencedStore(t, "gcn", "gcnq")
	// The mint prefix, and the namespace the store holds without minting.
	for _, id := range []string{"gcn-7", "gcnq-abc-q"} {
		created, err := store.Create(Bead{ID: id, Title: "held"})
		if err != nil {
			t.Fatalf("Create(%q): %v", id, err)
		}
		if created.ID != id {
			t.Errorf("Create(%q) returned id %q; a pinned id inside the namespace must be honored verbatim", id, created.ID)
		}
	}
}

// The control that keeps the fence from being a blanket refusal of pinned ids:
// an unfenced store is the shipped default and must behave exactly as before.
func TestAnUnfencedStoreStillHonorsAnyPinnedID(t *testing.T) {
	store := openFencedStore(t)
	created, err := store.Create(Bead{ID: "ga-42", Title: "no fence configured"})
	if err != nil {
		t.Fatalf("an unfenced store refused a pinned id: %v", err)
	}
	if created.ID != "ga-42" {
		t.Errorf("got id %q, want ga-42", created.ID)
	}
}

// The other control: minting is untouched. The fence only ever inspects an id
// the caller supplied.
func TestFenceLeavesMintingAlone(t *testing.T) {
	store := openFencedStore(t, "gcn", "gcnq")
	created, err := store.Create(Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.ID, "gcn-") {
		t.Errorf("minted id %q does not carry the store's own prefix", created.ID)
	}
}

func TestForeignIDCreateStaysOpenForTheMigrationCopy(t *testing.T) {
	store := openFencedStore(t, "gcn", "gcnq")
	created, err := store.CreateWithForeignID(Bead{ID: "ga-42", Title: "a relic carried across by gc storage migrate"})
	if err != nil {
		t.Fatalf("CreateWithForeignID refused a preserved id: %v — the migration copy path cannot do its job", err)
	}
	if created.ID != "ga-42" {
		t.Errorf("got id %q, want the preserved ga-42", created.ID)
	}
}
