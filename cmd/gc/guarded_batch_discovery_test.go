package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The effects' blind-write-refusing store declares its inner store as the
// conditional-writes resolve target so FENCED writes resolve through it. A
// guarded batch may carry unguarded updates and pinned creates, so guarded
// batch discovery must not follow that target: through the wrapper, and
// through any cache, class wrapper or policy wrapper layered over it, there is
// no batcher (CONTRACT v5 R3).
func TestGuardedBatcherForStopsAtTheBlindWriteRefusingStore(t *testing.T) {
	inner := beads.NewMemStore()
	if _, ok := beads.GuardedBatcherFor(inner); !ok {
		t.Fatal("fixture: the inner MemStore must apply guarded batches")
	}
	refusing := blindWriteRefusingStore{inner: inner}
	for _, tc := range []struct {
		name  string
		store beads.Store
	}{
		{"blind-write-refusing store", refusing},
		{"cache over it", beads.NewCachingStoreForTest(refusing, nil)},
		{"class wrapper over it", beads.SessionStore{Store: refusing}},
		{"policy wrapper over it", wrapStoreWithBeadPolicies(refusing, &config.City{})},
		{"emitter over it", &emittingClassStore{Store: refusing, cityPath: t.TempDir()}},
	} {
		if b, ok := beads.GuardedBatcherFor(tc.store); ok {
			t.Errorf("GuardedBatcherFor(%s) = (%T, true): a guarded batch would bypass the no-blind-write rule", tc.name, b)
		}
	}
}
