package api

import (
	"fmt"
	"testing"
)

// The cache must not grow without bound. A hostile or buggy client that
// generates N distinct query-parameter combinations should not push the
// map past responseCacheMaxEntries.
func TestResponseCacheRespectsMaxEntries(t *testing.T) {
	s := &Server{}
	for i := 0; i < responseCacheMaxEntries*2; i++ {
		s.storeResponse(fmt.Sprintf("key-%d", i), uint64(i), "payload")
	}
	if got := len(s.responseCacheEntries); got > responseCacheMaxEntries {
		t.Errorf("cache size = %d, want ≤ %d", got, responseCacheMaxEntries)
	}
}
