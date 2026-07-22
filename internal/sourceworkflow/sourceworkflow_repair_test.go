package sourceworkflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestCloseSpecSidecarsForClosedRootsSequencedContinuesAfterRootDiscoveryError(t *testing.T) {
	base := beads.NewMemStore()
	roots := make([]beads.Bead, 2)
	specs := make([]beads.Bead, 2)
	for i := range roots {
		root, err := base.Create(beads.Bead{
			Title: "closed workflow root",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			},
		})
		if err != nil {
			t.Fatalf("Create root %d: %v", i, err)
		}
		roots[i] = root
		spec, err := base.Create(beads.Bead{
			Title: "generated spec residue",
			Type:  "spec",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:       beadmeta.KindSpec,
				beadmeta.RootBeadIDMetadataKey: root.ID,
			},
		})
		if err != nil {
			t.Fatalf("Create spec %d: %v", i, err)
		}
		specs[i] = spec
		if err := base.Close(root.ID); err != nil {
			t.Fatalf("Close root %d: %v", i, err)
		}
	}

	discoveryErr := errors.New("injected first-root discovery failure")
	store := &closedRootSidecarRepairFaultStore{
		Store:     base,
		getErrors: map[string]error{roots[0].ID: discoveryErr},
	}

	result, err := CloseSpecSidecarsForClosedRootsSequenced(store, WorkflowSpecSidecarClosedReason)
	if !errors.Is(err, discoveryErr) {
		t.Fatalf("repair error = %v, want discovery error", err)
	}
	if result.Closed != 1 {
		t.Fatalf("repaired sidecars = %d, want later root's sidecar repaired", result.Closed)
	}

	for i, spec := range specs {
		after, getErr := base.Get(spec.ID)
		if getErr != nil {
			t.Fatalf("Get spec %d: %v", i, getErr)
		}
		want := "open"
		if i == 1 {
			want = "closed"
		}
		if after.Status != want {
			t.Fatalf("spec %d status = %q, want %q", i, after.Status, want)
		}
	}
}

func TestCloseSpecSidecarsForClosedRootsSequencedContinuesAndJoinsRootErrors(t *testing.T) {
	base := beads.NewMemStore()
	roots := make([]beads.Bead, 3)
	for i := range roots {
		root, err := base.Create(beads.Bead{
			Title: "closed workflow root",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			},
		})
		if err != nil {
			t.Fatalf("Create root %d: %v", i, err)
		}
		roots[i] = root
	}
	if roots[0].ID >= roots[1].ID || roots[1].ID >= roots[2].ID {
		t.Fatalf("root IDs are not in creation/sort order: %q, %q, %q", roots[0].ID, roots[1].ID, roots[2].ID)
	}

	specs := make([]beads.Bead, len(roots))
	for i, root := range roots {
		spec, err := base.Create(beads.Bead{
			Title: "generated spec residue",
			Type:  "spec",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:       beadmeta.KindSpec,
				beadmeta.RootBeadIDMetadataKey: root.ID,
			},
		})
		if err != nil {
			t.Fatalf("Create spec %d: %v", i, err)
		}
		specs[i] = spec
		if err := base.Close(root.ID); err != nil {
			t.Fatalf("Close root %d: %v", i, err)
		}
	}

	cache := beads.NewCachingStoreForTest(base, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime cache: %v", err)
	}
	firstErr := errors.New("injected first-root sidecar listing failure")
	secondErr := errors.New("injected second-root sidecar listing failure")
	store := &closedRootSidecarRepairFaultStore{
		Store: cache,
		rootErrors: map[string]error{
			roots[0].ID: firstErr,
			roots[1].ID: secondErr,
		},
	}

	result, err := CloseSpecSidecarsForClosedRootsSequenced(store, WorkflowSpecSidecarClosedReason)
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("repair error = %v, want joined first and second root errors", err)
	}
	if result.Closed != 1 {
		t.Fatalf("repaired sidecars = %d, want 1 from the later successful root", result.Closed)
	}
	if len(result.Deliveries) != 1 {
		t.Fatalf("repair deliveries = %d, want the later successful root's delivery", len(result.Deliveries))
	}
	delivered := make(chan struct{})
	result.Deliveries[0].AfterDelivery(func() { close(delivered) })
	select {
	case <-delivered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("later successful root's repair delivery did not complete")
	}

	for i, spec := range specs {
		after, getErr := base.Get(spec.ID)
		if getErr != nil {
			t.Fatalf("Get spec %d: %v", i, getErr)
		}
		want := "open"
		if i == 2 {
			want = "closed"
		}
		if after.Status != want {
			t.Fatalf("spec %d status = %q, want %q", i, after.Status, want)
		}
	}
}

type closedRootSidecarRepairFaultStore struct {
	beads.Store
	getErrors  map[string]error
	rootErrors map[string]error
}

func (s *closedRootSidecarRepairFaultStore) Get(id string) (beads.Bead, error) {
	if err := s.getErrors[id]; err != nil {
		return beads.Bead{}, err
	}
	return beads.HandlesFor(s.Store).Live.Get(id)
}

func (s *closedRootSidecarRepairFaultStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.rootErrors[query.Metadata[beadmeta.RootBeadIDMetadataKey]]; err != nil {
		return nil, err
	}
	return beads.HandlesFor(s.Store).Live.List(query)
}

func (s *closedRootSidecarRepairFaultStore) BeadObserverBarrier(id string) beads.CloseObserverDelivery {
	barrier, ok := beads.ObserverBarrierFor(s.Store)
	if !ok {
		return nil
	}
	return barrier.BeadObserverBarrier(id)
}
