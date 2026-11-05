package beads_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// These tests are the fidelity differential for the route-clearing seam
// (ga-l69b9m): the gated capability the decorator hands out must behave, for
// every caller that is not writing gc.routed_to, exactly as the engine's own
// capability does. The beadstest suites that bind MemStore and CachingStore
// run here over WithRouteChangeClearing(MemStore), with the options MemStore
// itself runs with, so a gated wrapper that drops a typed error, a
// precondition failure's Current revision, a label guard, or a disable verdict
// fails the same assertion the engine passes.

func decoratedMemStore(*testing.T) beads.Store {
	return beads.WithRouteChangeClearing(beads.NewMemStore(), identityNormalizer)
}

func decoratedDisabledMemStore(*testing.T) beads.Store {
	mem := beads.NewMemStore()
	mem.DisableConditionalWrites = true
	return beads.WithRouteChangeClearing(mem, identityNormalizer)
}

func TestRouteClearFidelityConditionalWriterConformance(t *testing.T) {
	beadstest.RunConditionalWriterConformanceWithOptions(t, "RouteClear/MemStore", decoratedMemStore,
		beadstest.ConditionalWriterOptions{
			RowBackedMutationFlavors: true,
			RestrictedUpdateFields:   true,
			LabelsGuarded:            true,
			SuppliesCurrent:          true,
			OpenDisabled:             decoratedDisabledMemStore,
		})
}

func TestRouteClearFidelityMetadataCASConformance(t *testing.T) {
	beadstest.RunMetadataCASConformance(t, "RouteClear/MemStore", decoratedMemStore)
}

func TestRouteClearFidelityFenceConformance(t *testing.T) {
	beadstest.RunFenceConformance(t, func() beads.Store { return decoratedMemStore(nil) })
}

func TestRouteClearFidelityAtomicConditionalCloserConformance(t *testing.T) {
	beadstest.RunAtomicConditionalCloserConformance(t, "RouteClear/AtomicCloseMemStore",
		func(*testing.T) beads.Store {
			return beads.WithRouteChangeClearing(beads.NewAtomicCloseMemStore(), identityNormalizer)
		})
}

func TestRouteClearFidelityConditionalLabelsConformance(t *testing.T) {
	beadstest.RunConditionalLabelsConformance(t, "RouteClear/MemStore", decoratedMemStore, true)
}

func TestRouteClearFidelityCloseNotificationConformance(t *testing.T) {
	beadstest.RunCloseNotificationTests(t, decoratedMemStore)
}

// The cache over the decorator keeps passing the cache's own conformance run.
func TestRouteClearFidelityCachingStoreConditionalWriterConformance(t *testing.T) {
	open := func(t *testing.T) beads.Store {
		t.Helper()
		return newCache(t, beads.WithRouteChangeClearing(beads.NewMemStore(), identityNormalizer))
	}
	beadstest.RunConditionalWriterConformanceWithOptions(t, "RouteClear/CachingStore", open,
		beadstest.ConditionalWriterOptions{
			RestrictedUpdateFields: true,
			LabelsGuarded:          true,
			SuppliesCurrent:        true,
			OpenDisabled: func(t *testing.T) beads.Store {
				t.Helper()
				mem := beads.NewMemStore()
				mem.DisableConditionalWrites = true
				return newCache(t, beads.WithRouteChangeClearing(mem, identityNormalizer))
			},
		})
}

// Mode and latch parity: ResolveConditionalWriter over the decorated engine
// answers exactly what it answers over the bare engine, in every mode, because
// the decorator is neither a mode source nor a conditional writer of its own.
func TestRouteClearFidelityResolveConditionalWriterModeParity(t *testing.T) {
	for _, mode := range []gate.Mode{gate.ModeUnset, gate.Off, gate.Auto, gate.Require} {
		for _, disabled := range []bool{false, true} {
			name := string(mode)
			if disabled {
				name += "/disabled"
			}
			t.Run(name, func(t *testing.T) {
				open := func() (beads.Store, beads.Store) {
					bare := beads.NewMemStore()
					bare.DisableConditionalWrites = disabled
					other := beads.NewMemStore()
					other.DisableConditionalWrites = disabled
					if mode != gate.ModeUnset {
						for _, s := range []*beads.MemStore{bare, other} {
							if err := beads.StampOpenedStore(s, "MemStore", mode, false, nil, nil); err != nil {
								t.Fatalf("StampOpenedStore: %v", err)
							}
						}
					}
					return bare, beads.WithRouteChangeClearing(other, identityNormalizer)
				}
				bare, decorated := open()
				wantW, wantDiag, wantErr := beads.ResolveConditionalWriter(bare)
				gotW, gotDiag, gotErr := beads.ResolveConditionalWriter(decorated)
				if (wantW == nil) != (gotW == nil) {
					t.Errorf("writer resolved = %v over the decorator, %v over the bare engine", gotW != nil, wantW != nil)
				}
				if !reflect.DeepEqual(wantDiag, gotDiag) {
					t.Errorf("diagnostic = %+v over the decorator, %+v over the bare engine", gotDiag, wantDiag)
				}
				var wantReq, gotReq *beads.ConditionalWritesRequiredError
				if errors.As(wantErr, &wantReq) != errors.As(gotErr, &gotReq) {
					t.Errorf("error = %v over the decorator, %v over the bare engine", gotErr, wantErr)
				}
			})
		}
	}
}

// The decorator is not a mode source, so the latch and the stamp belong to the
// engine alone: stamping through the decorator is refused, never absorbed.
func TestRouteClearFidelityDecoratorIsNotAModeSource(t *testing.T) {
	decorated := beads.WithRouteChangeClearing(beads.NewMemStore(), identityNormalizer)
	if err := beads.StampOpenedStore(decorated, "RouteClear", gate.Require, false, nil, nil); err == nil {
		t.Error("stamping require through the decorator succeeded; a mode source would let the gate lower or latch a mode the engine does not carry")
	}
}
