package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

// The scope-ready wait asks the store it opens for its schema migration cursor
// (beads.CheckSchemaAtTarget). Every store cmd/gc opens is wrapped by the
// policy layer, which embeds the beads.Store interface and so hides every
// method it does not name: without a forwarder the check is a silent no-op on
// every production path and a scope stalled at v60 of v66 passes the wait
// (ga-zlmadr, review round 1). These tests hold the wrapper to the same rule
// as the batched dep read in bead_policy_store_capability_test.go (ga-50tsx).

// stalledCursorStore is a leaf store that reports a schema migration cursor.
type stalledCursorStore struct {
	beads.Store
	current, target int
	err             error
}

func (s stalledCursorStore) SchemaCursor(context.Context) (int, int, error) {
	return s.current, s.target, s.err
}

// stalledCursorGraphStore is the same over a graph-apply-capable engine: it
// embeds the concrete store, so the graph-apply methods are promoted and the
// policy layer wraps it in *beadPolicyGraphStore.
type stalledCursorGraphStore struct {
	*beads.SQLiteStore
	current, target int
	err             error
}

func (s stalledCursorGraphStore) SchemaCursor(context.Context) (int, int, error) {
	return s.current, s.target, s.err
}

// cursorWrapShapes is every way the policy layer wraps a store: the plain and
// the graph-apply shape, through both the owning and the invocation-memo
// (borrowed) constructor, over a leaf reporting cursor current of target.
func cursorWrapShapes(t *testing.T, current, target int) []struct {
	name    string
	wrapped beads.Store
} {
	t.Helper()
	cfg := &config.City{}
	plain := func() beads.Store {
		return stalledCursorStore{Store: beads.NewMemStore(), current: current, target: target}
	}
	graph := func() beads.Store {
		sqlite, ok := mustOpenSQLiteStoreForCapabilityTest(t).(*beads.SQLiteStore)
		if !ok {
			t.Fatal("the sqlite engine is not a *beads.SQLiteStore")
		}
		return stalledCursorGraphStore{SQLiteStore: sqlite, current: current, target: target}
	}
	shapes := []struct {
		name    string
		wrapped beads.Store
	}{
		{"wrapStoreWithBeadPolicies over a plain store", wrapStoreWithBeadPolicies(plain(), cfg)},
		{"wrapStoreWithBeadPolicies over a graph-capable store", wrapStoreWithBeadPolicies(graph(), cfg)},
		{"borrowedStoreWithBeadPolicies over a plain store", borrowedStoreWithBeadPolicies(plain(), cfg)},
		{"borrowedStoreWithBeadPolicies over a graph-capable store", borrowedStoreWithBeadPolicies(graph(), cfg)},
	}
	for i, wantGraph := range []bool{false, true, false, true} {
		_, isGraph := shapes[i].wrapped.(*beadPolicyGraphStore)
		if isGraph != wantGraph {
			t.Fatalf("%s wrapped as %T, want graph shape = %v", shapes[i].name, shapes[i].wrapped, wantGraph)
		}
	}
	return shapes
}

func TestBeadPolicyStoreReachesTheSchemaCursorThroughEveryWrap(t *testing.T) {
	for _, tc := range cursorWrapShapes(t, 60, 66) {
		t.Run(tc.name, func(t *testing.T) {
			reporter, ok := beads.As[beads.SchemaCursorReporter](tc.wrapped)
			if !ok {
				t.Fatalf("beads.As[SchemaCursorReporter](%T) = false; the policy wrapper hides the cursor, so "+
					"CheckSchemaAtTarget is a silent no-op over a stalled store", tc.wrapped)
			}
			current, target, err := reporter.SchemaCursor(context.Background())
			if err != nil || current != 60 || target != 66 {
				t.Fatalf("SchemaCursor through %T = (%d, %d, %v), want (60, 66, nil)", tc.wrapped, current, target, err)
			}
			err = beads.CheckSchemaAtTarget(context.Background(), tc.wrapped)
			if err == nil {
				t.Fatalf("CheckSchemaAtTarget over %T passed a store stalled at v60 of v66", tc.wrapped)
			}
			for _, want := range []string{"v60", "v66", "bd migrate schema"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("CheckSchemaAtTarget error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestBeadPolicyStoreSchemaCursorPassesAStoreAtTarget(t *testing.T) {
	for _, tc := range cursorWrapShapes(t, 66, 66) {
		t.Run(tc.name, func(t *testing.T) {
			if err := beads.CheckSchemaAtTarget(context.Background(), tc.wrapped); err != nil {
				t.Fatalf("CheckSchemaAtTarget over a store at its target: %v", err)
			}
		})
	}
}

// The wrapper's cursor must exist on the engine it forwards from, and on both
// wrapper shapes, or the forwarding tests above prove nothing about a tree
// where no engine can answer.
func TestSchemaCursorExistsOnTheSourceEngineAndBothWrappers(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(&beads.NativeDoltStore{}),
		reflect.TypeOf(&beadPolicyStore{}),
		reflect.TypeOf(&beadPolicyGraphStore{}),
	} {
		if _, ok := typ.MethodByName("SchemaCursor"); !ok {
			t.Errorf("%s has no SchemaCursor", typ)
		}
	}
}

// A wrapper over a store that cannot report a cursor must say so, never answer
// (0, 0, nil): CheckSchemaAtTarget reads "unavailable" as "not asked" and a
// zero answer as a cursor at target.
func TestBeadPolicyStoreSchemaCursorReportsAnUnsupportedBacking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wrapped beads.Store
	}{
		{"policy store", wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{})},
		{"policy graph store", wrapStoreWithBeadPolicies(mustOpenSQLiteStoreForCapabilityTest(t), &config.City{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reporter, ok := beads.As[beads.SchemaCursorReporter](tc.wrapped)
			if !ok {
				t.Fatalf("beads.As[SchemaCursorReporter](%T) = false; the wrapper must advertise the capability and say "+
					"'unavailable' itself when its backing has none", tc.wrapped)
			}
			current, target, err := reporter.SchemaCursor(context.Background())
			if !errors.Is(err, beads.ErrSchemaCursorUnavailable) {
				t.Fatalf("SchemaCursor over a backing with no cursor = (%d, %d, %v), want ErrSchemaCursorUnavailable", current, target, err)
			}
			if err := beads.CheckSchemaAtTarget(context.Background(), tc.wrapped); err != nil {
				t.Errorf("CheckSchemaAtTarget over an unsupported backing = %v, want nil (the store is not asked)", err)
			}
		})
	}
}

// NativeDoltStore.SchemaCursor is the engine end of the chain. An embedded
// workspace exports no live database handle, so it must say "unavailable"
// rather than invent a cursor; a closed store must report the closure, which
// is not "unavailable" (that would make CheckSchemaAtTarget skip a dead store).
func TestNativeDoltStoreSchemaCursorEmbeddedAndClosed(t *testing.T) {
	store := splittest.NewNativeWorkStore(t, "gc")
	current, target, err := store.SchemaCursor(context.Background())
	switch {
	case errors.Is(err, beads.ErrSchemaCursorUnavailable):
		// An embedded storage exports no UnderlyingDB: the documented answer.
	case err == nil && current == target && target > 0:
		// A library that does export one reports a fully migrated fresh store.
	default:
		t.Fatalf("SchemaCursor over a fresh embedded native store = (%d, %d, %v), want ErrSchemaCursorUnavailable or a cursor at target", current, target, err)
	}

	closed := splittest.NewNativeWorkStore(t, "gc")
	if err := closed.CloseStore(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	_, _, err = closed.SchemaCursor(context.Background())
	if err == nil || errors.Is(err, beads.ErrSchemaCursorUnavailable) {
		t.Fatalf("SchemaCursor over a closed store = %v, want a store-closed error that is not ErrSchemaCursorUnavailable", err)
	}
	if !errors.Is(err, beads.ErrStoreClosed) {
		t.Errorf("SchemaCursor over a closed store = %v, want ErrStoreClosed", err)
	}
}

// TestWaitForBeadsScopeReadyAfterRecoveryRefusesAStalledScopeBehindThePolicyWrap
// drives the real wait through the production wrap. It is the test that would
// have caught the fail-open: the unit tests above hold the pieces, this one
// holds the wiring (the store the wait opens is policy-wrapped, and the check
// must see through it).
func TestWaitForBeadsScopeReadyAfterRecoveryRefusesAStalledScopeBehindThePolicyWrap(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(withBuiltinProviderAliasesTOMLForTest(`
[workspace]
name = "my-city"
provider = "claude"
`, "claude")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	// observeOpenedStore sits at the single store-open point, before the policy
	// wrap, so the store the wait checks is the production wrapper over a leaf
	// that reports the cursor.
	openWith := func(t *testing.T, current, target int) {
		t.Helper()
		prev := observeOpenedStore
		observeOpenedStore = func(_ string, store beads.Store) beads.Store {
			return stalledCursorStore{Store: store, current: current, target: target}
		}
		t.Cleanup(func() { observeOpenedStore = prev })
	}
	wait := func() error {
		return waitForBeadsScopeReadyAfterRecovery(context.Background(), cityPath, cityPath, time.Now().Add(500*time.Millisecond))
	}

	t.Run("stalled", func(t *testing.T) {
		openWith(t, 60, 66)
		err := wait()
		if err == nil {
			t.Fatal("the wait passed a scope stalled at v60 of v66: the cursor check never saw through the policy wrap")
		}
		for _, want := range []string{cityPath, "v60", "v66", "bd migrate schema"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("wait error %q does not name %q", err, want)
			}
		}
	})

	t.Run("at target", func(t *testing.T) {
		openWith(t, 66, 66)
		if err := wait(); err != nil {
			t.Fatalf("the wait refused a scope at its target: %v", err)
		}
	})

	t.Run("ahead of target", func(t *testing.T) {
		openWith(t, 70, 66)
		if err := wait(); err != nil {
			t.Fatalf("the wait refused a scope ahead of its target (skew belongs to the open path): %v", err)
		}
	})
}
