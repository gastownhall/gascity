package beads_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// These tests pin how the route-clearing decorator carries a backing's
// edge-completeness declaration (ga-l69b9m, architect Amendment 2). A cache
// asserts on the declaration's presence first and then trusts its value, so the
// decorator must declare exactly when its backing does and answer with the
// backing's live answer; a decorator that always declared would turn a silent
// backing into a cache that declines every ready read.

// flippingStore declares edge completeness with an answer a test can flip, as
// BdStore's answer changes with live state.
type flippingStore struct {
	beads.Store
	beads.CompleteDeps
}

func newFlippingStore(answer *bool) *flippingStore {
	return &flippingStore{Store: beads.NewMemStore(), CompleteDeps: beads.CompleteDeps{Answer: func() bool { return *answer }}}
}

// silentStore embeds the Store interface only, so it promotes no optional
// capability and declares nothing.
type silentStore struct{ beads.Store }

func openFileStoreForCompleteness(t *testing.T) beads.Store {
	t.Helper()
	fs, err := beads.OpenFileStore(fsys.OSFS{}, t.TempDir()+"/beads.json")
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	return fs
}

// Over a declaring backing the decorator declares and follows the backing's
// answer as it changes; over a silent one it does not declare.
func TestRouteClearCompletenessFollowsTheBackingPresenceAndLiveAnswer(t *testing.T) {
	answer := true
	declaring := beads.WithRouteChangeClearing(newFlippingStore(&answer), identityNormalizer)
	if got, declared := beads.DeclaresCompleteDependencies(declaring); !declared || !got {
		t.Fatalf("over a backing declaring true: (answer=%v, declared=%v), want (true, true)", got, declared)
	}
	answer = false
	if got, declared := beads.DeclaresCompleteDependencies(declaring); !declared || got {
		t.Errorf("after the backing flipped to false: (answer=%v, declared=%v), want (false, true): the answer must be read at call time", got, declared)
	}

	for _, tc := range []struct {
		name  string
		inner beads.Store
	}{
		{"MemStore", beads.NewMemStore()},
		{"FileStore", openFileStoreForCompleteness(t)},
		{"a silent spy", silentStore{beads.NewMemStore()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, declared := beads.DeclaresCompleteDependencies(beads.WithRouteChangeClearing(tc.inner, identityNormalizer)); declared {
				t.Errorf("the decorator over %s declares edge completeness its backing does not", tc.name)
			}
		})
	}
}

// The decorator carries the declaration iff the engine declares it, for every
// engine that does.
func TestRouteClearCarriesCompletenessIffTheEngineDeclares(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner beads.Store
		want  bool
	}{
		{"SQLite", newSQLiteForConformance(t), true},
		{"NativeDolt", beads.NewNativeDoltStoreForConformance(), true},
		{"Proxied", beads.NewProxiedStoreForConformance("rc"), true},
		{"MemStore", beads.NewMemStore(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, engineDeclares := beads.DeclaresCompleteDependencies(tc.inner)
			if engineDeclares != tc.want {
				t.Fatalf("fixture: %s declares = %v, want %v", tc.name, engineDeclares, tc.want)
			}
			if _, carried := beads.DeclaresCompleteDependencies(beads.WithRouteChangeClearing(tc.inner, identityNormalizer)); carried != engineDeclares {
				t.Errorf("the decorator over %s carries the declaration = %v, but the engine declares it = %v", tc.name, carried, engineDeclares)
			}
		})
	}
}

// A cache over the decorator over a backing that declares nothing primes
// complete and serves its cached ready read, as a cache over the bare backing
// does. The decorator must not turn the backing's silence into a decline.
func TestRouteClearCacheOverASilentBackingServesCachedReady(t *testing.T) {
	mem := beads.NewMemStore()
	row, err := mem.Create(beads.Bead{Title: "ready row", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newCache(t, beads.WithRouteChangeClearing(mem, identityNormalizer))
	judgment, err := cache.ReadyJudgmentContext(context.Background(), beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("cache-only ready judgment over the decorator over a silent backing: %v", err)
	}
	if len(judgment.Rows) != 1 || judgment.Rows[0].ID != row.ID {
		t.Errorf("cache-only ready judgment rows = %v, want the one ready row %s", judgment.Rows, row.ID)
	}
	ready, err := beads.HandlesFor(cache).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("cached Ready over the decorator over a silent backing: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != row.ID {
		t.Errorf("cached Ready = %v, want the one ready row %s", ready, row.ID)
	}
}

// The decorator stays recognizable as the decorator whichever shape it takes.
func TestRouteClearUnwrapRecognizesBothShapes(t *testing.T) {
	answer := true
	for _, tc := range []struct {
		name  string
		inner beads.Store
	}{
		{"declaring backing", newFlippingStore(&answer)},
		{"silent backing", silentStore{beads.NewMemStore()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := beads.UnwrapRouteChangeClearing(beads.WithRouteChangeClearing(tc.inner, identityNormalizer))
			if !ok || got != tc.inner {
				t.Errorf("UnwrapRouteChangeClearing = (%v, %v), want the backing and true", got, ok)
			}
		})
	}
}

// Each forward over a backing without the capability answers that capability's
// own sentinel, which its own consumer reads as the cue for its fallback.
func TestRouteClearUnsupportedForwardsCarryTheirOwnSentinel(t *testing.T) {
	store := beads.WithRouteChangeClearing(silentStore{beads.NewMemStore()}, identityNormalizer)
	for _, tc := range []struct {
		name string
		call func() error
		want error
	}{
		{"CreateWithForeignIDBatch", func() error { return store.(beads.ForeignIDBatchCreator).CreateWithForeignIDBatch(nil) }, beads.ErrBatchCopyUnsupported},
		{"DepAddBatch", func() error { return store.(beads.DepBatchWriter).DepAddBatch(nil) }, beads.ErrBatchCopyUnsupported},
		{"DepRemoveBatch", func() error { return store.(beads.DepBatchRemover).DepRemoveBatch(nil) }, beads.ErrDepRemoveBatchUnsupported},
		{"TransferIfCurrent", func() error {
			_, err := store.(beads.ConditionalAssigneeTransferer).TransferIfCurrent("x", "a", "b")
			return err
		}, beads.ErrConditionalTransferUnsupported},
		{"DepListBatch", func() error {
			_, err := store.(beads.DependencyBatchLister).DepListBatch(nil)
			return err
		}, beads.ErrDepListBatchUnsupported},
		{"DepMetadataBatch", func() error {
			_, err := store.(beads.DepMetadataBatchReader).DepMetadataBatch(nil)
			return err
		}, beads.ErrDepMetadataUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Errorf("%s over a backing without it = %v, want %v", tc.name, err, tc.want)
			}
		})
	}
}
