package beads

import "testing"

// NativeDoltStore must NOT implement Counter. The pinned beads library's
// CountIssues counts only the issues table, while SearchIssues (the List
// path) also merges the wisps table — no-history and ephemeral rows — so
// a backend COUNT undercounts open work relative to List (#1896 review).
// Hydration-free counting happens at the caching layer instead; when the
// cache cannot answer, CachingStore.Count reports ErrCountUnsupported and
// callers fall back to the hydrating List path, which is exact.
func TestNativeDoltStoreDoesNotImplementCounter(t *testing.T) {
	var store any = &NativeDoltStore{}
	if _, ok := store.(Counter); ok {
		t.Fatal("NativeDoltStore implements Counter; the backing CountIssues misses wisps-table rows (no-history/ephemeral), undercounting vs List (#1896)")
	}
}
