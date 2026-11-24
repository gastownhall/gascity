package beads

import "testing"

func TestRowRefresherForFindsTheCacheBelowHandlesAndWrappers(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	for name, s := range map[string]Store{
		"cache":           cache,
		"session handle":  SessionStore{Store: cache},
		"wrapped":         &resolveTargetWrapper{Store: cache, target: cache},
		"handle, wrapped": SessionStore{Store: &resolveTargetWrapper{Store: cache, target: cache}},
	} {
		got, ok := RowRefresherFor(s)
		if !ok || got != RowRefresher(cache) {
			t.Errorf("%s: RowRefresherFor = %v, %v; want the cache", name, got, ok)
		}
	}
}

func TestRowRefresherForReportsAnUncachedStore(t *testing.T) {
	for name, s := range map[string]Store{
		"nil":            nil,
		"mem":            NewMemStore(),
		"session handle": SessionStore{Store: NewMemStore()},
		"wrapped":        &resolveTargetWrapper{Store: NewMemStore()},
	} {
		if got, ok := RowRefresherFor(s); ok {
			t.Errorf("%s: RowRefresherFor = %v, true; want false", name, got)
		}
	}
}
