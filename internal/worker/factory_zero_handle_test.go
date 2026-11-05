package worker

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestNewFactoryTreatsAZeroHandleAsNoStore pins R11 at the worker boundary
// (typed-stores T6a): a zero class handle boxed into FactoryConfig.Store is a
// non-nil interface whose first call panics, so the factory must see it as no
// store, exactly as it sees nil: no by-id resolver over it, and a target
// lookup that falls to the runtime instead of reading it.
func TestNewFactoryTreatsAZeroHandleAsNoStore(t *testing.T) {
	for name, store := range map[string]beads.Store{"nil": nil, "a zero sessions handle": beads.SessionStore{}, "a zero work handle": beads.WorkStore{}} {
		factory, err := NewFactory(FactoryConfig{Store: store, Provider: runtime.NewFake()})
		if err != nil {
			t.Fatalf("%s: NewFactory: %v", name, err)
		}
		if factory.store != nil || factory.resolveBead != nil {
			t.Errorf("%s: the factory kept a store (%v) or a by-id resolver over it", name, factory.store)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: HandleForTarget panicked: %v", name, r)
				}
			}()
			_, _ = factory.HandleForTarget("nobody", nil)
		}()
	}
}
