package beads

import (
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestConditionalWritesRequiredReadsTheResolvedStamp: the write-time
// fail-closed check reads the same factory stamp ResolveConditionalWriter
// does, through a wrapper's declared resolution target; an unstamped, auto
// or nil store is not "require".
func TestConditionalWritesRequiredReadsTheResolvedStamp(t *testing.T) {
	if ConditionalWritesRequired(nil) {
		t.Fatal("nil store: not required")
	}
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if ConditionalWritesRequired(cache) {
		t.Fatal("unstamped: not required")
	}
	if !cache.stampConditionalWritesMode(gate.Require, false) {
		t.Fatal("fixture: the stamp was refused")
	}
	if !ConditionalWritesRequired(cache) {
		t.Fatal("stamped require: required")
	}
	if !ConditionalWritesRequired(WorkStore{Store: cache}) {
		t.Fatal("through a declared resolution target: required")
	}
	auto := NewCachingStoreForTest(NewMemStore(), nil)
	auto.stampConditionalWritesMode(gate.Auto, false)
	if ConditionalWritesRequired(auto) {
		t.Fatal("auto: not required")
	}
}
