package runtime_test

import (
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// flippingAttestation is a provider whose listing attestation turns on
// while it lists, as an exec pack's does when its failed handshake recovers
// during the call: the listing it returns is the unattested one.
type flippingAttestation struct {
	*runtime.Fake
	on *atomic.Bool
}

func (f flippingAttestation) ListRunning(prefix string) ([]string, error) {
	names, err := f.Fake.ListRunning(prefix)
	f.on.Store(true)
	return names, err
}

func (f flippingAttestation) ListRunningComplete() bool { return f.on.Load() }

// Kills (PAR-EXEC MUST-1) an attestation read only after the listing,
// which reads the unattested listing an attestation that turned on during
// it as proof of absence.
func TestListRunningWithAttestationReadsBeforeAndAfter(t *testing.T) {
	sp := flippingAttestation{Fake: runtime.NewFake(), on: new(atomic.Bool)}
	if _, attested, err := runtime.ListRunningWithAttestation(sp, ""); err != nil || attested {
		t.Fatalf("attestation that turned on during the listing: attested = %v (%v), want false", attested, err)
	}
	if _, attested, _ := runtime.ListRunningWithAttestation(sp, ""); !attested {
		t.Fatal("attested before and after: attested = false, want true")
	}
	flip := flippingAttestation{Fake: runtime.NewFake(), on: new(atomic.Bool)}
	if _, _, attested, _ := runtime.ListInventoryWithAttestation(flip, ""); attested {
		t.Fatal("ListInventoryWithAttestation: attested across a flip")
	}
	lister := listerBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, available: true, lists: new(int), flip: new(atomic.Bool)}
	if _, inv, attested, _ := runtime.ListInventoryWithAttestation(lister, ""); attested || inv == nil {
		t.Fatalf("ListInventoryWithAttestation through a lister: attested = %v (inventory %v) across a flip, want false", attested, inv)
	}
}

// listerBackend is a gated inventory lister that counts its reads. With
// flip set, its attestation turns on during its first listing.
type listerBackend struct {
	runtime.FullFake
	available bool
	lists     *int
	flip      *atomic.Bool
}

func (l listerBackend) InventoryAvailable() bool { return l.available }

func (l listerBackend) ListRunningComplete() bool { return l.flip == nil || l.flip.Load() }

func (l listerBackend) ListInventory(prefix string) ([]string, map[string]runtime.InventoryEntry, error) {
	*l.lists++
	if l.flip != nil {
		l.flip.Store(true)
	}
	return []string{prefix + "x"}, map[string]runtime.InventoryEntry{prefix + "x": {Incarnation: "i"}}, nil
}

// Kills a lister read where its inventory is gated off, or not read where
// it is served.
func TestListInventoryWithAttestationGatesTheLister(t *testing.T) {
	for _, available := range []bool{true, false} {
		l := listerBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, available: available, lists: new(int)}
		names, inv, attested, err := runtime.ListInventoryWithAttestation(l, "p-")
		if err != nil || !attested {
			t.Fatalf("available %v: %v, %v", available, attested, err)
		}
		if available != (*l.lists == 1) || available != (inv != nil) || available && !slices.Equal(names, []string{"p-x"}) {
			t.Errorf("available %v: lister reads %d, names %v, inventory %v", available, *l.lists, names, inv)
		}
	}
}
