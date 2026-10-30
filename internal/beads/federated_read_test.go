package beads_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFederatedReadRunsTheReadAndReturnsItsError pins that the marker changes
// nothing at runtime: it runs the read once and hands back its error.
func TestFederatedReadRunsTheReadAndReturnsItsError(t *testing.T) {
	t.Parallel()
	calls := 0
	want := errors.New("boom")
	if err := beads.FederatedRead(func() error { calls++; return want }); !errors.Is(err, want) {
		t.Fatalf("FederatedRead error = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("read ran %d times, want 1", calls)
	}
}

// TestFederatedGetReturnsTheStoreAnswer pins the by-id convenience form.
func TestFederatedGetReturnsTheStoreAnswer(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := beads.FederatedGet(store, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("FederatedGet = %+v, %v; want %s", got, err, created.ID)
	}
	if _, err := beads.FederatedGet(store, "gc-404"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("FederatedGet(missing) error = %v, want ErrNotFound", err)
	}
}

// TestFederatedReadFrameIsOnTheStack pins the property the split-strict test
// gate relies on: the marker is a real, non-inlined frame a store wrapper can
// find on its caller's stack.
func TestFederatedReadFrameIsOnTheStack(t *testing.T) {
	t.Parallel()
	var marked bool
	_ = beads.FederatedRead(func() error {
		marked = beads.FederatedReadOnStack(callerPCs())
		return nil
	})
	if !marked {
		t.Fatal("FederatedReadOnStack did not find the FederatedRead frame")
	}
	if beads.FederatedReadOnStack(callerPCs()) {
		t.Fatal("FederatedReadOnStack reported a marker outside any FederatedRead")
	}
	var viaGet bool
	probe := probeStore{Store: beads.NewMemStore(), onGet: func() { viaGet = beads.FederatedReadOnStack(callerPCs()) }}
	_, _ = beads.FederatedGet(probe, "gc-1")
	if !viaGet {
		t.Fatal("FederatedGet did not leave a marker frame")
	}
}

type probeStore struct {
	beads.Store
	onGet func()
}

func (p probeStore) Get(id string) (beads.Bead, error) {
	p.onGet()
	return p.Store.Get(id)
}

func callerPCs() []uintptr {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(1, pcs)
	return pcs[:n]
}

// TestFederatedReadMarkerNamesAreStable pins the frame names the gate matches.
func TestFederatedReadMarkerNamesAreStable(t *testing.T) {
	t.Parallel()
	for _, name := range beads.FederatedReadMarkerFuncs() {
		if !strings.HasPrefix(name, "github.com/gastownhall/gascity/internal/beads.Federated") &&
			name != "github.com/gastownhall/gascity/internal/beads.RelicRead" {
			t.Fatalf("marker func %q is not a beads.Federated* or RelicRead function", name)
		}
	}
}

// TestRelicReadIsADistinctMarker pins that a relic leg is told apart from a
// legitimate federation leg, innermost marker first.
func TestRelicReadIsADistinctMarker(t *testing.T) {
	t.Parallel()
	var got beads.ReadMarker
	if err := beads.RelicRead(func() error { got = beads.ReadMarkerOnStack(callerPCs()); return nil }); err != nil {
		t.Fatal(err)
	}
	if got != beads.RelicReadMarker {
		t.Fatalf("marker inside RelicRead = %v, want RelicReadMarker", got)
	}
	_ = beads.FederatedRead(func() error {
		_ = beads.RelicRead(func() error { got = beads.ReadMarkerOnStack(callerPCs()); return nil })
		return nil
	})
	if got != beads.RelicReadMarker {
		t.Fatalf("innermost marker = %v, want RelicReadMarker", got)
	}
	if beads.ReadMarkerOnStack(callerPCs()) != beads.NoReadMarker {
		t.Fatal("a marker was reported outside any marker")
	}
}
