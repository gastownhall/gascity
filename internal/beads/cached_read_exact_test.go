package beads

import "testing"

// Kills: a wrong leg classification. The controller reads a leg's demand from
// its CachingStore only when the backing declares CachedReadExact, so a bd
// store that declared it would count blocked work as demand (EB-42o8), and a
// SQLite store that stopped declaring it would push the sessions binding onto
// the backstop lane's live reads.
func TestSQLiteDeclaresCachedReadExactBdStoreDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
		exact bool
	}{
		{"SQLiteStore", (*SQLiteStore)(nil), true},
		{"BdStore", (*BdStore)(nil), false},
		{"NativeDoltStore", (*NativeDoltStore)(nil), false},
		{"MemStore", (*MemStore)(nil), false},
		{"FileStore", (*FileStore)(nil), false},
		{"CachingStore", (*CachingStore)(nil), false},
	} {
		declarer, ok := tc.store.(CachedReadExact)
		if got := ok && declarer.CachedReadExact(); got != tc.exact {
			t.Errorf("%s declares CachedReadExact = %t, want %t", tc.name, got, tc.exact)
		}
	}
}
