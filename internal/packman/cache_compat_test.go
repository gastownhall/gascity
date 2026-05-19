package packman

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestCacheKeyAlignment verifies that packman.RepoCacheKey and
// config.RepoCacheKey produce identical results for the same inputs.
// This is critical: if they diverge, gc import writes to one cache
// path and the loader looks for a different one.
func TestCacheKeyAlignment(t *testing.T) {
	cases := []struct {
		source string
		commit string
	}{
		{"github.com/gastownhall/gastown", "abc123"},
		{"https://github.com/org/repo.git", "def456"},
		{"git@github.com:org/repo.git", "789abc"},
		{"https://github.com/org/repo/tree/v1.0/packs/base", "aaa111"},
		{"file:///tmp/repo.git//sub/path", "bbb222"},
		{"github.com/org/repo//subpath#v1.0", "ccc333"},
		{"simple-source", "ddd444"},
	}

	for _, tc := range cases {
		packmanKey := RepoCacheKey(tc.source, tc.commit)
		configKey := config.RepoCacheKey(tc.source, tc.commit)
		if packmanKey != configKey {
			t.Errorf("cache key mismatch for source=%q commit=%q:\n  packman: %s\n  config:  %s",
				tc.source, tc.commit, packmanKey, configKey)
		}
	}
}
